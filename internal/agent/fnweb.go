package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Lambda web apps: the function's container runs an ordinary HTTP server (started
// with PORT set). A function URL request is forwarded to it as is; any other event
// (API, console test, schedule, queue) is POSTed to /events as JSON, like the AWS
// Lambda Web Adapter. The server's answer becomes the usual function result.

const webMaxResponse = 6 << 20

var webClient = &http.Client{
	Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second, DisableCompression: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// waitWebReady waits until the app accepts connections on its port; alive reports
// an error as soon as the server's process has stopped.
func waitWebReady(ctx context.Context, addr string, limit time.Duration, alive func() error) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if err := alive(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("the web app did not listen on %s within %d s (does it use the PORT variable?)", addr[strings.LastIndex(addr, ":")+1:], int(limit.Seconds()))
}

type urlEvent struct {
	RawPath        string            `json:"rawPath"`
	RawQuery       string            `json:"rawQueryString"`
	Headers        map[string]string `json:"headers"`
	Cookies        []string          `json:"cookies"`
	Body           *string           `json:"body"`
	IsBase64       bool              `json:"isBase64Encoded"`
	RequestContext struct {
		HTTP *struct {
			Method   string `json:"method"`
			SourceIP string `json:"sourceIp"`
		} `json:"http"`
		RequestID string `json:"requestId"`
	} `json:"requestContext"`
}

func textualType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml") ||
		strings.Contains(ct, "javascript") || strings.Contains(ct, "x-www-form-urlencoded")
}

// webInvoke sends the event to the app and returns the function result JSON
// ({"ok": true, "result": ...}).
func webInvoke(ctx context.Context, addr string, event []byte, requestID string) ([]byte, error) {
	var ev urlEvent
	_ = json.Unmarshal(event, &ev)
	var req *http.Request
	var err error
	isHTTP := ev.RequestContext.HTTP != nil && ev.RequestContext.HTTP.Method != ""
	if isHTTP {
		var body []byte
		if ev.Body != nil {
			if ev.IsBase64 {
				if body, err = base64.StdEncoding.DecodeString(*ev.Body); err != nil {
					return nil, fmt.Errorf("request body: %w", err)
				}
			} else {
				body = []byte(*ev.Body)
			}
		}
		path := ev.RawPath
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		u := "http://" + addr + path
		if ev.RawQuery != "" {
			u += "?" + ev.RawQuery
		}
		if req, err = http.NewRequestWithContext(ctx, ev.RequestContext.HTTP.Method, u, bytes.NewReader(body)); err != nil {
			return nil, err
		}
		for k, v := range ev.Headers {
			if k == "host" || k == "content-length" || k == "connection" {
				continue
			}
			req.Header.Set(k, v)
		}
		if host := ev.Headers["host"]; host != "" {
			req.Host = host
		}
		if len(ev.Cookies) > 0 {
			req.Header.Set("Cookie", strings.Join(ev.Cookies, "; "))
		}
		if ip := ev.RequestContext.HTTP.SourceIP; ip != "" {
			req.Header.Set("X-Forwarded-For", ip)
		}
		req.Header.Set("X-Forwarded-Proto", "https")
	} else {
		if req, err = http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/events", bytes.NewReader(event)); err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("X-Platform-Request-Id", requestID)

	resp, err := webClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, webMaxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(body) > webMaxResponse {
		return nil, errors.New("the response is larger than 6 MB")
	}
	var result any
	if isHTTP {
		headers := map[string]string{}
		var cookies []string
		for k, vs := range resp.Header {
			lk := strings.ToLower(k)
			switch lk {
			case "set-cookie":
				cookies = append(cookies, vs...)
			case "content-length", "connection", "transfer-encoding", "keep-alive":
			default:
				headers[lk] = strings.Join(vs, ",")
			}
		}
		r := map[string]any{"statusCode": resp.StatusCode, "headers": headers}
		if len(cookies) > 0 {
			r["cookies"] = cookies
		}
		if textualType(resp.Header.Get("Content-Type")) && utf8.Valid(body) {
			r["body"], r["isBase64Encoded"] = string(body), false
		} else {
			r["body"], r["isBase64Encoded"] = base64.StdEncoding.EncodeToString(body), true
		}
		result = r
	} else {
		// The answer to an event: its JSON if it is JSON, otherwise its text.
		if json.Valid(body) && len(bytes.TrimSpace(body)) > 0 {
			result = json.RawMessage(body)
		} else {
			result = string(body)
		}
		if resp.StatusCode >= 400 {
			return json.Marshal(map[string]any{"ok": false, "errorType": "WebApp.HTTP" + strconv.Itoa(resp.StatusCode),
				"errorMessage": truncate(strings.TrimSpace(string(body)), 500)})
		}
	}
	return json.Marshal(map[string]any{"ok": true, "result": result})
}

// webAddr finds the app's address on the project network.
func (a *Agent) webAddr(ctx context.Context, containerID string, port int) (string, error) {
	info, err := a.dk.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}
	for _, ep := range info.NetworkSettings.Networks {
		if ep.IPAddress != "" {
			return net.JoinHostPort(ep.IPAddress, strconv.Itoa(port)), nil
		}
	}
	return "", errors.New("the web app's container has no network address")
}

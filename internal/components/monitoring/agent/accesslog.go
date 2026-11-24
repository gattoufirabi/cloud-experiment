package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"platform/internal/core/worker"
	"platform/internal/lib/docker"
)

type visitorSalt struct {
	day  string
	salt []byte
}

func (v *visitorSalt) get(now time.Time) []byte {
	if d := now.UTC().Format("2006-01-02"); d != v.day {
		v.day, v.salt = d, make([]byte, 32)
		_, _ = rand.Read(v.salt)
	}
	return v.salt
}

func first(h map[string][]string, k string) string {
	if v := h[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// parseUA classifies a user agent into browser, operating system, device and bot.
func parseUA(ua string) (browser, osName, device string, bot bool) {
	l := strings.ToLower(ua)
	for _, b := range []string{"bot", "crawler", "spider", "curl/", "wget/", "python-requests", "go-http-client", "httpclient", "headless", "monitor", "uptime", "preview"} {
		if strings.Contains(l, b) {
			bot = true
		}
	}
	switch {
	case strings.Contains(l, "edg/"):
		browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "firefox/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	case strings.Contains(l, "curl/"):
		browser = "curl"
	case ua == "":
		browser = "Unknown"
	default:
		browser = "Other"
	}
	switch {
	case strings.Contains(l, "android"):
		osName = "Android"
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad") || strings.Contains(l, "ios"):
		osName = "iOS"
	case strings.Contains(l, "windows"):
		osName = "Windows"
	case strings.Contains(l, "mac os") || strings.Contains(l, "macintosh"):
		osName = "macOS"
	case strings.Contains(l, "linux"):
		osName = "Linux"
	default:
		osName = "Other"
	}
	switch {
	case bot:
		device = "bot"
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet"):
		device = "tablet"
	case strings.Contains(l, "mobile") || strings.Contains(l, "android") || strings.Contains(l, "iphone"):
		device = "mobile"
	default:
		device = "desktop"
	}
	return
}

// accessRecord turns one proxy line into the stored record (nil when the host is unknown).
func accessRecord(line []byte, idx map[string]worker.HostRef, salt []byte) map[string]any {
	var e worker.CaddyAccess
	if json.Unmarshal(line, &e) != nil || e.Request.Host == "" {
		return nil
	}
	host := strings.ToLower(e.Request.Host)
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	ref, ok := idx[host]
	if !ok {
		return nil
	}
	path := e.Request.URI
	if i := strings.IndexAny(path, "?#"); i >= 0 { // query strings can hold secrets: not kept
		path = path[:i]
	}
	if len(path) > 300 {
		path = path[:300]
	}
	ua := first(e.Request.Headers, "User-Agent")
	browser, osName, device, bot := parseUA(ua)
	ip := e.Request.ClientIP
	if ip == "" {
		ip = e.Request.RemoteIP
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(ip + "|" + ua + "|" + ref.ServiceID))
	ref0 := ""
	if u, err := url.Parse(first(e.Request.Headers, "Referer")); err == nil && u.Host != "" && !strings.EqualFold(u.Hostname(), host) {
		ref0 = strings.ToLower(u.Hostname())
	}
	t := time.Unix(0, int64(e.Ts*float64(time.Second)))
	return map[string]any{
		"_time": t.UTC().Format(time.RFC3339Nano), "_msg": e.Request.Method + " " + path + " " + strconv.Itoa(e.Status),
		"an_org": ref.OrgID, "an_svc": ref.ServiceID, "an_kind": "access", "an_project": ref.ProjectID,
		"host": host, "method": e.Request.Method, "path": path, "status": e.Status, "class": strconv.Itoa(e.Status/100) + "xx",
		"duration_ms": int64(e.Duration * 1000), "bytes": e.Size, "referrer": ref0,
		"browser": browser, "os": osName, "device": device, "bot": strconv.FormatBool(bot),
		"visitor": hex.EncodeToString(h.Sum(nil))[:16],
	}
}

func (a *Worker) AccessLogLoop(ctx context.Context) {
	if !a.IsPrimary() || a.Cfg.CaddyContainer == "" {
		return
	}
	var salt visitorSalt
	since := time.Now().Unix()
	out := make(chan map[string]any, 2000)
	go a.pushAccess(ctx, out)
	for ctx.Err() == nil {
		rc, err := a.Docker.ContainerLogs(ctx, a.Cfg.CaddyContainer, since, true, "")
		if err == nil {
			_ = docker.Demux(rc, func(stream byte, line string) {
				if stream != 1 {
					return
				}
				ts, msg, found := strings.Cut(line, " ")
				if found {
					if tm, err := time.Parse(time.RFC3339Nano, ts); err == nil {
						since = tm.Unix() + 1
						line = msg
					}
				}
				if strings.Contains(line, `"http.log.access.s3"`) {
					a.BucketEvent(ctx, []byte(line))
					return
				}
				if !strings.Contains(line, `"http.log.access.tenant"`) || a.Cfg.VictoriaLogsURL == "" {
					return
				}
				a.Mu.Lock()
				idx := a.HostIndex
				a.Mu.Unlock()
				if rec := accessRecord([]byte(line), idx, salt.get(time.Now())); rec != nil {
					select {
					case out <- rec:
					default: // the store is behind: drop rather than block the proxy's log
					}
				}
			})
			rc.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *Worker) pushAccess(ctx context.Context, in <-chan map[string]any) {
	hc := &http.Client{Timeout: 15 * time.Second}
	var batch []map[string]any
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, r := range batch {
			_ = enc.Encode(r)
		}
		batch = batch[:0]
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Cfg.VictoriaLogsURL+"/insert/jsonline?_stream_fields=an_org,an_svc,an_kind&_msg_field=_msg&_time_field=_time", &buf)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/stream+json")
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
		} else {
			a.Log.Warn("ship access logs", "err", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-in:
			if batch = append(batch, r); len(batch) >= 500 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

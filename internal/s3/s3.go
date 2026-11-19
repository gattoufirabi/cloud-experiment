// Package s3 signs S3 requests (AWS Signature Version 4) and implements the few object
// calls the console needs: list, upload, delete and presigned download links.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	unsignedPayload = "UNSIGNED-PAYLOAD"
	emptySHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type Credentials struct{ AccessKey, SecretKey, Region string }

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// uriEncode encodes like AWS: unreserved characters kept, everything else %XX; "/" is
// kept in paths.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func signingKey(secret, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	return hmacSHA256(k, "aws4_request")
}

// signature computes the SigV4 signature of a canonical request.
func signature(c Credentials, method, path string, query url.Values, headers map[string]string, payloadHash string, t time.Time) (sig, signedHeaders, scope string) {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	lower := map[string]string{}
	for k, v := range headers {
		lower[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + lower[n] + "\n")
	}
	signedHeaders = strings.Join(names, ";")
	canonical := strings.Join([]string{method, uriEncode(path, true), canonicalQuery(query), ch.String(), signedHeaders, payloadHash}, "\n")
	date := t.UTC().Format("20060102")
	scope = date + "/" + c.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + t.UTC().Format("20060102T150405Z") + "\n" + scope + "\n" + sha256Hex(canonical)
	return hex.EncodeToString(hmacSHA256(signingKey(c.SecretKey, date, c.Region), toSign)), signedHeaders, scope
}

// Sign adds SigV4 headers to req. payloadHash is the hex SHA-256 of the body, or
// "UNSIGNED-PAYLOAD".
func Sign(req *http.Request, c Credentials, payloadHash string, t time.Time) {
	req.Header.Set("X-Amz-Date", t.UTC().Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	headers := map[string]string{"host": req.URL.Host}
	for k := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || lk == "range" || strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = req.Header.Get(k)
		}
	}
	sig, signed, scope := signature(c, req.Method, req.URL.Path, req.URL.Query(), headers, payloadHash, t)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKey, scope, signed, sig))
}

// Presign returns a URL that allows method on rawURL for the given time, without
// credentials (a download link, for example).
func Presign(c Credentials, method, rawURL string, expires time.Duration, t time.Time) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", c.AccessKey+"/"+t.UTC().Format("20060102")+"/"+c.Region+"/s3/aws4_request")
	q.Set("X-Amz-Date", t.UTC().Format("20060102T150405Z"))
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	sig, _, _ := signature(c, method, u.Path, q, map[string]string{"host": u.Host}, unsignedPayload, t)
	q.Set("X-Amz-Signature", sig)
	u.RawQuery = canonicalQuery(q)
	return u.String(), nil
}

// Client talks to one S3 endpoint with path-style addressing.
type Client struct {
	Endpoint string // e.g. http://garage:3900
	Creds    Credentials
	HTTP     *http.Client
}

func NewClient(endpoint string, c Credentials) *Client {
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), Creds: c, HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

// ObjectURL is the path-style URL of an object.
func ObjectURL(endpoint, bucket, key string) string {
	return strings.TrimRight(endpoint, "/") + "/" + bucket + "/" + uriEncode(key, true)
}

// Error is an S3 error answer.
type Error struct {
	Status  int
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (e *Error) Error() string { return fmt.Sprintf("s3: %d %s: %s", e.Status, e.Code, e.Message) }

func (c *Client) do(ctx context.Context, method, bucket, key string, query url.Values, body io.Reader, size int64, contentType string) (*http.Response, error) {
	u := c.Endpoint + "/" + bucket
	if key != "" {
		u += "/" + uriEncode(key, true)
	}
	if len(query) > 0 {
		u += "?" + canonicalQuery(query)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	payload := emptySHA256
	if body != nil {
		payload = unsignedPayload
		req.ContentLength = size
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}
	Sign(req, c.Creds, payload, time.Now())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		e := &Error{Status: resp.StatusCode}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = xml.Unmarshal(data, e)
		return nil, e
	}
	return resp, nil
}

type Object struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
}

type Listing struct {
	Objects   []Object `xml:"Contents"`
	Prefixes  []string `xml:"CommonPrefixes>Prefix"`
	Truncated bool     `xml:"IsTruncated"`
	NextToken string   `xml:"NextContinuationToken"`
}

// List lists up to max objects under prefix; with delimiter "/" it groups "folders".
func (c *Client) List(ctx context.Context, bucket, prefix, delimiter, token string, max int) (Listing, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(max)}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if delimiter != "" {
		q.Set("delimiter", delimiter)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	var l Listing
	resp, err := c.do(ctx, http.MethodGet, bucket, "", q, nil, 0, "")
	if err != nil {
		return l, err
	}
	defer resp.Body.Close()
	err = xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&l)
	return l, err
}

// Put uploads an object of a known size.
func (c *Client) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) error {
	if body == nil {
		return errors.New("no body")
	}
	resp, err := c.do(ctx, http.MethodPut, bucket, key, nil, body, size, contentType)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Delete(ctx context.Context, bucket, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, bucket, key, nil, nil, 0, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Empty deletes every object of a bucket (at most limit), returning how many it deleted.
func (c *Client) Empty(ctx context.Context, bucket string, limit int) (int, error) {
	n := 0
	for n < limit {
		l, err := c.List(ctx, bucket, "", "", "", 1000)
		if err != nil {
			return n, err
		}
		if len(l.Objects) == 0 {
			return n, nil
		}
		for _, o := range l.Objects {
			if err := c.Delete(ctx, bucket, o.Key); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, fmt.Errorf("more than %d objects", limit)
}

// SetCORS lets web pages of these origins use the bucket from the browser (GET, HEAD, PUT
// and POST, any request header, ETag readable); no origins removes the rule. It needs a
// key with owner rights on the bucket.
func (c *Client) SetCORS(ctx context.Context, bucket string, origins []string) error {
	if len(origins) == 0 {
		resp, err := c.do(ctx, http.MethodDelete, bucket, "", url.Values{"cors": {""}}, nil, 0, "")
		if err != nil {
			var e *Error
			if errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Code == "NoSuchCORSConfiguration") {
				return nil
			}
			return err
		}
		resp.Body.Close()
		return nil
	}
	var b strings.Builder
	b.WriteString(`<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><CORSRule>`)
	for _, o := range origins {
		b.WriteString("<AllowedOrigin>")
		_ = xml.EscapeText(&b, []byte(o))
		b.WriteString("</AllowedOrigin>")
	}
	for _, m := range []string{"GET", "HEAD", "PUT", "POST"} {
		b.WriteString("<AllowedMethod>" + m + "</AllowedMethod>")
	}
	b.WriteString(`<AllowedHeader>*</AllowedHeader><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>3600</MaxAgeSeconds></CORSRule></CORSConfiguration>`)
	body := []byte(b.String())
	sum := md5.Sum(body)
	u := c.Endpoint + "/" + bucket + "?cors="
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	Sign(req, c.Creds, sha256Hex(string(body)), time.Now())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = xml.Unmarshal(data, e)
		return e
	}
	return nil
}

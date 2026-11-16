// Package garage is a small client for the admin API (v2) of Garage, the S3-compatible
// object store behind the platform's buckets. Only the calls the platform uses are here.
package garage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	URL, Token string
	HTTP       *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{URL: baseURL, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Error is an error answer of the admin API.
type Error struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("garage: %d %s: %s", e.Status, e.Code, e.Message) }

// IsNotFound reports whether err is Garage's answer for a missing bucket or key.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// IsNotEmpty reports whether a bucket deletion was refused because it holds objects.
func IsNotEmpty(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusBadRequest && e.Code == "BucketNotEmpty"
}

func (c *Client) call(ctx context.Context, method, op string, query url.Values, body, out any) error {
	u := c.URL + "/v2/" + op
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		_ = json.Unmarshal(data, e)
		if e.Code == "" {
			e.Message = string(bytes.TrimSpace(data))
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type Quotas struct {
	MaxSize    *int64 `json:"maxSize"`
	MaxObjects *int64 `json:"maxObjects"`
}

type Perms struct {
	Read  bool `json:"read"`
	Write bool `json:"write"`
	Owner bool `json:"owner"`
}

type BucketKey struct {
	AccessKeyID string `json:"accessKeyId"`
	Name        string `json:"name"`
	Permissions Perms  `json:"permissions"`
}

type Bucket struct {
	ID            string      `json:"id"`
	GlobalAliases []string    `json:"globalAliases"`
	WebsiteAccess bool        `json:"websiteAccess"`
	Keys          []BucketKey `json:"keys"`
	Objects       int64       `json:"objects"`
	Bytes         int64       `json:"bytes"`
	Quotas        Quotas      `json:"quotas"`
}

type BucketSummary struct {
	ID            string   `json:"id"`
	GlobalAliases []string `json:"globalAliases"`
}

type Key struct {
	AccessKeyID     string `json:"accessKeyId"`
	Name            string `json:"name"`
	SecretAccessKey string `json:"secretAccessKey"`
}

func (c *Client) Health(ctx context.Context) error {
	var h struct {
		Status string `json:"status"`
	}
	if err := c.call(ctx, http.MethodGet, "GetClusterHealth", nil, nil, &h); err != nil {
		return err
	}
	if h.Status != "healthy" {
		return fmt.Errorf("garage cluster is %s", h.Status)
	}
	return nil
}

func (c *Client) CreateBucket(ctx context.Context, alias string) (Bucket, error) {
	var b Bucket
	err := c.call(ctx, http.MethodPost, "CreateBucket", nil, map[string]any{"globalAlias": alias}, &b)
	return b, err
}

func (c *Client) GetBucket(ctx context.Context, id string) (Bucket, error) {
	var b Bucket
	err := c.call(ctx, http.MethodGet, "GetBucketInfo", url.Values{"id": {id}}, nil, &b)
	return b, err
}

func (c *Client) ListBuckets(ctx context.Context) ([]BucketSummary, error) {
	var out []BucketSummary
	err := c.call(ctx, http.MethodGet, "ListBuckets", nil, nil, &out)
	return out, err
}

// SetQuota caps a bucket's size in bytes (0 = no cap).
func (c *Client) SetQuota(ctx context.Context, id string, maxBytes int64) error {
	q := Quotas{}
	if maxBytes > 0 {
		q.MaxSize = &maxBytes
	}
	return c.call(ctx, http.MethodPost, "UpdateBucket", url.Values{"id": {id}}, map[string]any{"quotas": q}, nil)
}

// SetWebsite turns public website access on (with its index and optional error
// document) or off.
func (c *Client) SetWebsite(ctx context.Context, id string, on bool, index, errorDoc string) error {
	w := map[string]any{"enabled": on}
	if on {
		w["indexDocument"] = index
		if errorDoc != "" {
			w["errorDocument"] = errorDoc
		}
	}
	return c.call(ctx, http.MethodPost, "UpdateBucket", url.Values{"id": {id}}, map[string]any{"websiteAccess": w}, nil)
}

// DeleteBucket deletes an empty bucket (IsNotEmpty otherwise).
func (c *Client) DeleteBucket(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "DeleteBucket", url.Values{"id": {id}}, nil, nil)
}

// CreateKey creates an access key; the secret is only returned here.
func (c *Client) CreateKey(ctx context.Context, name string) (Key, error) {
	var k Key
	err := c.call(ctx, http.MethodPost, "CreateKey", nil, map[string]any{"name": name}, &k)
	return k, err
}

func (c *Client) ListKeys(ctx context.Context) ([]Key, error) {
	var out []Key
	err := c.call(ctx, http.MethodGet, "ListKeys", nil, nil, &out)
	return out, err
}

func (c *Client) DeleteKey(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "DeleteKey", url.Values{"id": {id}}, nil, nil)
}

// SetPerms makes a key's permissions on a bucket exactly p (Garage only adds or removes
// the flags that are true, so this allows the wanted ones and denies the others).
func (c *Client) SetPerms(ctx context.Context, bucketID, keyID string, p Perms) error {
	allow := Perms{Read: p.Read, Write: p.Write, Owner: p.Owner}
	deny := Perms{Read: !p.Read, Write: !p.Write, Owner: !p.Owner}
	if allow.Read || allow.Write || allow.Owner {
		if err := c.call(ctx, http.MethodPost, "AllowBucketKey", nil,
			map[string]any{"bucketId": bucketID, "accessKeyId": keyID, "permissions": allow}, nil); err != nil {
			return err
		}
	}
	if deny.Read || deny.Write || deny.Owner {
		return c.call(ctx, http.MethodPost, "DenyBucketKey", nil,
			map[string]any{"bucketId": bucketID, "accessKeyId": keyID, "permissions": deny}, nil)
	}
	return nil
}

package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Bucket triggers: the proxy records every S3 request (logger "s3"); a successful
// upload (PUT, or the POST completing a multipart upload) or delete of an object
// starts the functions subscribed to that bucket, asynchronously, with an
// S3-style event. Batch deletes (POST ?delete) carry no keys and start nothing.

type bucketTrigger struct {
	OrgID, FunctionID, Events, Prefix, Suffix string
}

type bucketTriggers struct {
	mu   sync.Mutex
	at   time.Time
	byBk map[string][]bucketTrigger // bucket full name -> triggers
}

func (a *Agent) triggersFor(ctx context.Context, bucket string) []bucketTrigger {
	bt := &a.bucketTrig
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if time.Since(bt.at) > 20*time.Second {
		m := map[string][]bucketTrigger{}
		rows, err := a.pool.Query(ctx, `
			SELECT b.full_name, t.org_id, t.function_id, t.events, t.prefix, t.suffix
			FROM bucket_triggers t JOIN buckets b ON b.id = t.bucket_id JOIN orgs o ON o.id = t.org_id WHERE NOT o.suspended`)
		if err == nil {
			for rows.Next() {
				var name string
				var t bucketTrigger
				if rows.Scan(&name, &t.OrgID, &t.FunctionID, &t.Events, &t.Prefix, &t.Suffix) == nil {
					m[name] = append(m[name], t)
				}
			}
			rows.Close()
			bt.byBk, bt.at = m, time.Now()
		}
	}
	return bt.byBk[bucket]
}

func (a *Agent) refreshBucketTriggers() {
	a.bucketTrig.mu.Lock()
	a.bucketTrig.at = time.Time{}
	a.bucketTrig.mu.Unlock()
}

// s3Event parses one proxy line into (bucket, key, eventName, size); ok false when it
// is not an object upload or delete that succeeded.
func s3Event(line []byte) (bucket, key, name string, size int64, t time.Time, ok bool) {
	var e caddyAccess
	if json.Unmarshal(line, &e) != nil || e.Status < 200 || e.Status > 299 {
		return
	}
	u, err := url.Parse(e.Request.URI)
	if err != nil {
		return
	}
	q := u.Query()
	path := strings.TrimPrefix(u.EscapedPath(), "/")
	b, k, found := strings.Cut(path, "/")
	if !found || b == "" || k == "" {
		return
	}
	key, err = url.PathUnescape(k)
	if err != nil {
		return
	}
	switch e.Request.Method {
	case "PUT":
		if q.Has("partNumber") || q.Has("acl") || q.Has("tagging") {
			return "", "", "", 0, time.Time{}, false
		}
		name = "ObjectCreated:Put"
		if e.Request.Headers["X-Amz-Copy-Source"] != nil {
			name = "ObjectCreated:Copy"
		}
	case "POST":
		if !q.Has("uploadId") {
			return "", "", "", 0, time.Time{}, false
		}
		name = "ObjectCreated:CompleteMultipartUpload"
	case "DELETE":
		if q.Has("uploadId") {
			return "", "", "", 0, time.Time{}, false
		}
		name = "ObjectRemoved:Delete"
	default:
		return "", "", "", 0, time.Time{}, false
	}
	return b, key, name, e.BytesRead, time.Unix(0, int64(e.Ts*float64(time.Second))), true
}

func (a *Agent) bucketEvent(ctx context.Context, line []byte) {
	bucket, key, name, size, at, ok := s3Event(line)
	if !ok {
		return
	}
	for _, t := range a.triggersFor(ctx, bucket) {
		created := strings.HasPrefix(name, "ObjectCreated")
		if t.Events == "created" && !created || t.Events == "removed" && created ||
			!strings.HasPrefix(key, t.Prefix) || !strings.HasSuffix(key, t.Suffix) {
			continue
		}
		obj := map[string]any{"key": key}
		if created {
			obj["size"] = size
		}
		event, _ := json.Marshal(map[string]any{"Records": []any{map[string]any{
			"eventVersion": "2.1", "eventSource": "platform:s3", "eventTime": at.UTC().Format(time.RFC3339Nano), "eventName": name,
			"s3": map[string]any{"bucket": map[string]any{"name": bucket}, "object": obj},
		}}})
		var backlog, conc int
		if err := a.pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM function_invocations WHERE function_id = $1 AND status IN ('queued', 'running')), concurrency
			FROM functions WHERE id = $1`, t.FunctionID).Scan(&backlog, &conc); err != nil || backlog >= conc*20 {
			continue // the function is gone or far behind: drop rather than pile up
		}
		if _, err := a.pool.Exec(ctx, `INSERT INTO function_invocations (org_id, function_id, trigger, async, event) VALUES ($1, $2, 'bucket', true, $3)`,
			t.OrgID, t.FunctionID, event); err != nil {
			a.log.Warn("bucket trigger", "err", err)
			continue
		}
		a.pokeFn()
	}
}

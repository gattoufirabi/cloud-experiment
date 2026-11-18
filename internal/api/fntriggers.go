package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"platform/internal/secure"
)

// Function triggers beyond URL, schedule, API and queues: bucket events (the agent
// starts the function when an object is uploaded to or deleted from a bucket) and a
// secret webhook URL any service can call (GitHub, Stripe...), answered at once (202)
// while the function runs in the background.

type bucketTriggerView struct {
	ID, Bucket, Events, Prefix, Suffix string
}

type fnTriggers struct {
	Buckets    []bucketTriggerView
	AllBuckets []string
	HasWebhook bool
}

func (s *Server) loadFnTriggers(ctx context.Context, tx pgx.Tx, fnID string) (fnTriggers, error) {
	var t fnTriggers
	rows, err := tx.Query(ctx, `SELECT bt.id, b.name, bt.events, bt.prefix, bt.suffix FROM bucket_triggers bt JOIN buckets b ON b.id = bt.bucket_id
		WHERE bt.function_id = $1 ORDER BY b.name`, fnID)
	if err != nil {
		return t, err
	}
	if t.Buckets, err = pgx.CollectRows(rows, pgx.RowToStructByPos[bucketTriggerView]); err != nil {
		return t, err
	}
	rows, err = tx.Query(ctx, `SELECT name FROM buckets ORDER BY name`)
	if err != nil {
		return t, err
	}
	if t.AllBuckets, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return t, err
	}
	err = tx.QueryRow(ctx, `SELECT webhook_token_hash IS NOT NULL FROM functions WHERE id = $1`, fnID).Scan(&t.HasWebhook)
	return t, err
}

func (s *Server) addBucketTrigger(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		events, prefix, suffix := r.PostFormValue("events"), r.PostFormValue("prefix"), r.PostFormValue("suffix")
		if events != "created" && events != "removed" && events != "all" {
			events = "created"
		}
		if len(prefix) > 200 || len(suffix) > 50 {
			formErr = "Prefix: at most 200 characters; suffix: at most 50."
			return nil
		}
		var bucketID string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM buckets WHERE name = $1`, r.PostFormValue("bucket")).Scan(&bucketID); err != nil {
			formErr = "Choose a bucket."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO bucket_triggers (org_id, bucket_id, function_id, events, prefix, suffix) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (bucket_id, function_id) DO UPDATE SET events = EXCLUDED.events, prefix = EXCLUDED.prefix, suffix = EXCLUDED.suffix`,
			o.ID, bucketID, f.ID, events, prefix, suffix); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "function.trigger_added", "function:"+p.Slug+"/"+f.Slug, map[string]string{"bucket": r.PostFormValue("bucket"), "events": events})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, fnPath(o, p, f)+"#triggers", "", formErr)
		return
	}
	back(w, r, fnPath(o, p, f)+"#triggers", "Bucket trigger saved: the function runs within seconds of each matching change.", "")
}

func (s *Server) deleteBucketTrigger(w http.ResponseWriter, r *http.Request) {
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM bucket_triggers WHERE id::text = $1 AND function_id = $2`, r.PathValue("id"), f.ID)
		if err == nil && tag.RowsAffected() > 0 {
			s.audit(r, tx, o.ID, "function.trigger_removed", "function:"+p.Slug+"/"+f.Slug, nil)
		}
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, fnPath(o, p, f)+"#triggers", "Bucket trigger removed.", "")
}

// functionWebhook creates (or replaces) the function's secret webhook URL, shown once;
// with remove=1 it turns the webhook off.
func (s *Server) functionWebhook(w http.ResponseWriter, r *http.Request) {
	remove := r.PostFormValue("remove") == "1"
	token := "whk_" + secure.NewToken()
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		var hash any
		if !remove {
			hash = secure.HashToken(token)
		}
		if _, err := tx.Exec(r.Context(), `UPDATE functions SET webhook_token_hash = $2 WHERE id = $1`, f.ID, hash); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, map[bool]string{true: "function.webhook_removed", false: "function.webhook_created"}[remove], "function:"+p.Slug+"/"+f.Slug, nil)
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if remove {
		back(w, r, fnPath(o, p, f)+"#triggers", "Webhook removed: its URL no longer works.", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderFunction(w, r, o, p, f, data{"Tab": "triggers", "WebhookURL": s.cfg.BaseURL + "/hooks/fn/" + token})
}

// functionWebhookCall is the public webhook endpoint: it queues an asynchronous
// invocation with the request as a function URL style event and answers 202.
func (s *Server) functionWebhookCall(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !strings.HasPrefix(token, "whk_") || !s.limiter.allow("fnhook:"+clientIP(r), 120, time.Minute) {
		fnJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	var orgID, fnID string
	if err := s.pool.QueryRow(r.Context(), `SELECT org_id, function_id FROM function_by_webhook($1)`, secure.HashToken(token)).Scan(&orgID, &fnID); err != nil {
		fnJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		fnJSONError(w, http.StatusBadRequest, "Bad Request")
		return
	}
	headers := map[string]string{}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "cookie" || lk == "authorization" {
			continue
		}
		headers[lk] = strings.Join(vs, ",")
	}
	ev := map[string]any{
		"version": "2.0", "routeKey": "$default", "rawPath": "/", "rawQueryString": r.URL.RawQuery, "headers": headers,
		"requestContext": map[string]any{"http": map[string]any{"method": r.Method, "path": "/", "sourceIp": clientIP(r), "userAgent": r.UserAgent()},
			"requestId": newRequestID(), "timeEpoch": time.Now().UnixMilli()},
		"isBase64Encoded": false,
	}
	if len(body) > 0 {
		if utf8.Valid(body) {
			ev["body"] = string(body)
		} else {
			ev["body"], ev["isBase64Encoded"] = base64.StdEncoding.EncodeToString(body), true
		}
	}
	event, _ := json.Marshal(ev)
	id, err := s.queueInvocation(r.Context(), orgID, fnID, "webhook", event, true)
	if err == errThrottled {
		fnJSONError(w, http.StatusTooManyRequests, "Rate Exceeded.")
		return
	}
	if err != nil {
		s.log.Error("webhook invocation", "err", err)
		fnJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "queued"})
}

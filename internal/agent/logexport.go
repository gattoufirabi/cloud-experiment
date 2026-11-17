package agent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/netguard"
	"platform/internal/s3"
)

// Log exports: every minute the primary sends each enabled export the logs between its
// cursor and one minute ago, an hour at most per step, then moves the cursor. A failed
// send keeps the cursor, so nothing is lost or sent twice; older logs than the plan's
// retention are skipped.

func (a *Agent) pokeLogExport() {
	select {
	case a.logExportWake <- struct{}{}:
	default:
	}
}

func (a *Agent) logExportLoop(ctx context.Context) {
	if !a.isPrimary() || a.cfg.VictoriaLogsURL == "" {
		return
	}
	hc := netguard.Client(a.cfg.AlertAllowPrivate)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		a.runLogExports(ctx, hc)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.logExportWake:
		}
	}
}

type logExport struct {
	ID, OrgID, Name, Kind, Prefix, URL string
	BucketID, BucketName, ConsoleKey   string
	ConsoleSecretEnc, SecretEnc        []byte
	ProjectID                          string
	Cursor                             time.Time
	RetentionDays                      int
}

func (a *Agent) runLogExports(ctx context.Context, hc *http.Client) {
	rows, err := a.pool.Query(ctx, `
		SELECT e.id, e.org_id, e.name, e.kind, e.prefix, e.url, coalesce(b.id::text, ''), coalesce(b.full_name, ''), coalesce(b.console_key_id, ''),
		       coalesce(b.console_secret_enc, '\x'::bytea), coalesce(e.secret_enc, '\x'::bytea), coalesce(e.project_id::text, ''), e.cursor_at, p.log_retention_days
		FROM log_exports e JOIN orgs o ON o.id = e.org_id JOIN plans p ON p.id = o.plan_id LEFT JOIN buckets b ON b.id = e.bucket_id
		WHERE e.enabled AND NOT o.suspended`)
	if err != nil {
		a.log.Warn("load log exports", "err", err)
		return
	}
	exports, err := pgx.CollectRows(rows, pgx.RowToStructByPos[logExport])
	if err != nil {
		return
	}
	for _, e := range exports {
		for step := 0; step < 24; step++ { // catch up at most a day per run
			moved, err := a.exportStep(ctx, hc, e)
			if err != nil {
				a.log.Warn("log export", "export", e.ID, "err", err)
				_, _ = a.pool.Exec(ctx, `UPDATE log_exports SET last_run = now(), last_status = 'error', last_error = $2 WHERE id = $1`, e.ID, truncate(err.Error(), 300))
				break
			}
			if moved.IsZero() {
				break
			}
			e.Cursor = moved
		}
	}
}

// exportStep sends one window of logs; it returns the new cursor (zero when caught up).
func (a *Agent) exportStep(ctx context.Context, hc *http.Client, e logExport) (time.Time, error) {
	from := e.Cursor
	if oldest := time.Now().Add(-time.Duration(e.RetentionDays) * 24 * time.Hour); from.Before(oldest) {
		from = oldest
	}
	to := time.Now().Add(-time.Minute).Truncate(time.Second)
	if hourEnd := from.Truncate(time.Hour).Add(time.Hour); to.After(hourEnd) {
		to = hourEnd // one file per hour
	}
	if !to.After(from) {
		return time.Time{}, nil
	}
	names, err := a.serviceNames(ctx, e.OrgID)
	if err != nil {
		return time.Time{}, err
	}
	q := fmt.Sprintf(`_time:[%s, %s) org_id:=%s`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), strconv.Quote(e.OrgID))
	if e.ProjectID != "" {
		q += " project_id:=" + strconv.Quote(e.ProjectID)
	}
	q += " | sort by (_time) | limit 500000"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.VictoriaLogsURL+"/select/logsql/query?"+url.Values{"query": {q}}.Encode(), nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return time.Time{}, fmt.Errorf("logs query: %d %s", resp.StatusCode, b)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	n := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		nm := names[m["service_id"]]
		_ = enc.Encode(map[string]string{"time": m["_time"], "project": nm[0], "service": nm[1], "stream": m["stream"], "message": m["_msg"]})
		n++
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, err
	}
	if err := gz.Close(); err != nil {
		return time.Time{}, err
	}
	if n > 0 {
		switch e.Kind {
		case "bucket":
			secret, err := a.box.Open(e.ConsoleSecretEnc, "bucket:"+e.BucketID)
			if err != nil {
				return time.Time{}, err
			}
			cl := s3.NewClient("http://"+a.cfg.S3Upstream, s3.Credentials{AccessKey: e.ConsoleKey, SecretKey: string(secret), Region: a.cfg.S3Region})
			key := fmt.Sprintf("%s%s/%d-%d.ndjson.gz", e.Prefix, from.UTC().Format("2006/01/02/15"), from.Unix(), to.Unix())
			if err := cl.Put(ctx, e.BucketName, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "application/gzip"); err != nil {
				return time.Time{}, fmt.Errorf("write %s: %w", key, err)
			}
		case "https":
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(buf.Bytes()))
			if err != nil {
				return time.Time{}, err
			}
			req.Header.Set("Content-Type", "application/x-ndjson")
			req.Header.Set("Content-Encoding", "gzip")
			req.Header.Set("User-Agent", "platform-log-export")
			req.Header.Set("X-Platform-Export", e.Name)
			if len(e.SecretEnc) > 0 {
				secret, err := a.box.Open(e.SecretEnc, "logexport:"+e.ID)
				if err != nil {
					return time.Time{}, err
				}
				mac := hmac.New(sha256.New, secret)
				mac.Write(buf.Bytes())
				req.Header.Set("X-Platform-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			}
			resp, err := netguard.Do(ctx, hc, req)
			if err != nil {
				return time.Time{}, err
			}
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				return time.Time{}, fmt.Errorf("endpoint answered %d", resp.StatusCode)
			}
		}
	}
	if _, err := a.pool.Exec(ctx, `UPDATE log_exports SET cursor_at = $2, last_run = now(), last_status = 'ok', last_error = '', lines_total = lines_total + $3 WHERE id = $1`,
		e.ID, to, n); err != nil {
		return time.Time{}, err
	}
	return to, nil
}

// serviceNames maps service IDs to [project, service] slugs.
func (a *Agent) serviceNames(ctx context.Context, orgID string) (map[string][2]string, error) {
	rows, err := a.pool.Query(ctx, `SELECT s.id, p.slug, s.slug FROM services s JOIN projects p ON p.id = s.project_id WHERE s.org_id = $1`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var id, p, s string
		if err := rows.Scan(&id, &p, &s); err != nil {
			return nil, err
		}
		out[id] = [2]string{p, s}
	}
	return out, rows.Err()
}

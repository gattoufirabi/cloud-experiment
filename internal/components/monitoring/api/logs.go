package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/netguard"
)

// Organization-wide logs (Monitoring > Logs): search every service's logs within the
// plan's retention, download them, and manage continuous exports to a bucket or an
// HTTPS endpoint (run by the agent).

var regexpPrefix = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,100}$`)

var logRanges = []struct {
	Key   string
	Label string
	Dur   time.Duration
}{
	{"1h", "last hour", time.Hour}, {"6h", "last 6 hours", 6 * time.Hour}, {"24h", "last 24 hours", 24 * time.Hour},
	{"7d", "last 7 days", 7 * 24 * time.Hour}, {"30d", "last 30 days", 30 * 24 * time.Hour},
}

type orgLogLine struct {
	Time, Service, Stream, Msg string
}

// logFilter builds the fixed part of a LogsQL query: tenant, optional project or
// service, time window. Values are always quoted.
func logFilter(orgID, projectID, serviceID string, window time.Duration) string {
	q := fmt.Sprintf(`_time:%ds org_id:=%s`, int(window.Seconds()), strconv.Quote(orgID))
	if projectID != "" {
		q += " project_id:=" + strconv.Quote(projectID)
	}
	if serviceID != "" {
		q += " service_id:=" + strconv.Quote(serviceID)
	}
	return q
}

func cleanSearch(search string) string {
	search = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, search)
	if len(search) > 100 {
		search = search[:100]
	}
	return search
}

// streamLogs runs a LogsQL query and calls fn for each line (newest first).
func (s *API) streamLogs(ctx context.Context, filter, search string, limit int, fn func(m map[string]string) error) error {
	if s.Cfg.VictoriaLogsURL == "" {
		return nil
	}
	q := filter
	if search = cleanSearch(search); search != "" {
		q += " " + strconv.Quote(search)
	}
	q += fmt.Sprintf(" | sort by (_time desc) | limit %d", limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Cfg.VictoriaLogsURL+"/select/logsql/query?"+url.Values{"query": {q}}.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("logs query: %d %s", resp.StatusCode, b)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			if err := fn(m); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

type logScope struct {
	Retention        int // days
	Range, Q, Target string
	Window           time.Duration
	ProjectID, SvcID string
	Names            map[string]string   // service ID -> project/service
	Targets          []web.ProjectOption // "project" and "project/service" choices
}

// logScopeFor reads the page's filters, capped by the plan's retention.
func (s *API) logScopeFor(r *http.Request, tx pgx.Tx, o *web.OrgInfo) (logScope, error) {
	sc := logScope{Range: r.URL.Query().Get("range"), Q: r.URL.Query().Get("q"), Target: r.URL.Query().Get("target"), Names: map[string]string{}}
	if err := tx.QueryRow(r.Context(), `SELECT log_retention_days FROM plans WHERE id = $1`, o.PlanID).Scan(&sc.Retention); err != nil {
		return sc, err
	}
	max := time.Duration(sc.Retention) * 24 * time.Hour
	sc.Window = time.Hour
	for _, lr := range logRanges {
		if lr.Key == sc.Range {
			sc.Window = lr.Dur
		}
	}
	if sc.Window > max {
		sc.Window = max
	}
	rows, err := tx.Query(r.Context(), `
		SELECT p.id, p.slug, p.name, coalesce(s.id::text, ''), coalesce(s.slug, '')
		FROM projects p LEFT JOIN services s ON s.project_id = p.id ORDER BY p.slug, s.slug`)
	if err != nil {
		return sc, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var pid, pslug, pname, sid, sslug string
		if err := rows.Scan(&pid, &pslug, &pname, &sid, &sslug); err != nil {
			return sc, err
		}
		if !seen[pslug] {
			seen[pslug] = true
			sc.Targets = append(sc.Targets, web.ProjectOption{Slug: pslug, Name: "project " + pname})
			if sc.Target == pslug {
				sc.ProjectID = pid
			}
		}
		if sid != "" {
			sc.Names[sid] = pslug + "/" + sslug
			sc.Targets = append(sc.Targets, web.ProjectOption{Slug: pslug + "/" + sslug, Name: "  " + pslug + "/" + sslug})
			if sc.Target == pslug+"/"+sslug {
				sc.SvcID = sid
			}
		}
	}
	return sc, rows.Err()
}

func (s *API) orgLogsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	var sc logScope
	var exports []logExportView
	var buckets []*web.BucketInfo
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if sc, err = s.logScopeFor(r, tx, o); err != nil {
			return err
		}
		if exports, err = loadLogExports(r.Context(), tx); err != nil {
			return err
		}
		if s.Cfg.StorageEnabled() {
			buckets, err = web.LoadBuckets(r.Context(), tx)
		}
		return err
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	var lines []orgLogLine
	if sc.Target == "" || sc.ProjectID != "" || sc.SvcID != "" {
		qerr := s.streamLogs(r.Context(), logFilter(o.ID, sc.ProjectID, sc.SvcID, sc.Window), sc.Q, 500, func(m map[string]string) error {
			lines = append(lines, orgLogLine{Time: m["_time"], Service: sc.Names[m["service_id"]], Stream: m["stream"], Msg: m["_msg"]})
			return nil
		})
		if qerr != nil {
			s.Log.Warn("org logs", "err", qerr)
		}
	}
	type rangeOpt struct {
		Key, Label string
		Allowed    bool
	}
	var ranges []rangeOpt
	for _, lr := range logRanges {
		ranges = append(ranges, rangeOpt{lr.Key, lr.Label, lr.Dur <= time.Duration(sc.Retention)*24*time.Hour})
	}
	s.Render(w, r, "logs", http.StatusOK, web.Data{"Org": o, "Scope": sc, "Lines": lines, "Ranges": ranges, "Exports": exports, "Buckets": buckets,
		"CanEdit": model.RoleAtLeast(o.Role, "admin"), "StorageNav": s.Cfg.StorageEnabled(), "Area": "monitoring"})
}

// downloadLogs streams the matching lines (up to 100,000) as a text file.
func (s *API) downloadLogs(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	var sc logScope
	if err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		sc, err = s.logScopeFor(r, tx, o)
		return err
	}); err != nil {
		s.ServerError(w, r, err)
		return
	}
	if sc.Target != "" && sc.ProjectID == "" && sc.SvcID == "" {
		s.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="logs-%s-%s.txt"`, o.Slug, time.Now().UTC().Format("20060102-1504")))
	bw := bufio.NewWriterSize(w, 64<<10)
	defer bw.Flush()
	_ = s.streamLogs(r.Context(), logFilter(o.ID, sc.ProjectID, sc.SvcID, sc.Window), sc.Q, 100000, func(m map[string]string) error {
		_, err := fmt.Fprintf(bw, "%s %s %s %s\n", m["_time"], sc.Names[m["service_id"]], m["stream"], strings.ReplaceAll(m["_msg"], "\n", " "))
		return err
	})
	_ = db.Audit(r.Context(), s.Pool, o.ID, web.SessionFrom(r).UserID, "logs.downloaded", "org:"+o.Slug, map[string]string{"range": sc.Range, "target": sc.Target}, web.ClientIP(r))
}

// ---------- exports ----------

type logExportView struct {
	ID, Name, Kind, Bucket, Prefix, URL, Project, LastStatus, LastError string
	Enabled                                                             bool
	Cursor                                                              time.Time
	LastRun                                                             *time.Time
	Lines                                                               int64
}

func loadLogExports(ctx context.Context, tx pgx.Tx) ([]logExportView, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.id, e.name, e.kind, coalesce(b.full_name, ''), e.prefix, e.url, coalesce(p.slug, ''), e.last_status, e.last_error,
		       e.enabled, e.cursor_at, e.last_run, e.lines_total
		FROM log_exports e LEFT JOIN buckets b ON b.id = e.bucket_id LEFT JOIN projects p ON p.id = e.project_id ORDER BY e.created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[logExportView])
}

func (s *API) createLogExport(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	dest := o.Path() + "/monitoring/logs#exports"
	name := strings.TrimSpace(r.PostFormValue("name"))
	kind := r.PostFormValue("kind")
	prefix := strings.TrimSpace(r.PostFormValue("prefix"))
	target := strings.TrimSpace(r.PostFormValue("url"))
	secret := r.PostFormValue("secret")
	if prefix == "" {
		prefix = "logs/"
	}
	switch {
	case name == "" || len(name) > 60:
		web.Back(w, r, dest, "", "Give the export a name.")
		return
	case kind != "bucket" && kind != "https":
		web.Back(w, r, dest, "", "Choose where to send the logs.")
		return
	case kind == "https" && netguard.ValidateURL(target, !s.Cfg.AlertAllowPrivate, s.Cfg.AlertAllowPrivate) != nil:
		web.Back(w, r, dest, "", "Endpoint: an https:// URL on a public address.")
		return
	case strings.Contains(prefix, "..") || !regexpPrefix.MatchString(prefix):
		web.Back(w, r, dest, "", "Prefix: letters, digits, dots, dashes, underscores and slashes.")
		return
	}
	var formErr string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var bucketID, projectID any
		if kind == "bucket" {
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT id FROM buckets WHERE name = $1`, r.PostFormValue("bucket")).Scan(&id); err != nil {
				formErr = "Choose one of your buckets."
				return nil
			}
			bucketID = id
		}
		if p := r.PostFormValue("project"); p != "" {
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE slug = $1`, p).Scan(&id); err != nil {
				formErr = "Choose a project."
				return nil
			}
			projectID = id
		}
		var exportID string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO log_exports (org_id, name, kind, bucket_id, prefix, url, project_id, cursor_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now() - interval '1 hour') RETURNING id`,
			o.ID, name, kind, bucketID, prefix, web.Ternary(kind == "https", target, ""), projectID).Scan(&exportID); err != nil {
			return err
		}
		if kind == "https" && secret != "" {
			enc, err := s.Box.Seal([]byte(secret), "logexport:"+exportID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `UPDATE log_exports SET secret_enc = $2 WHERE id = $1`, exportID, enc); err != nil {
				return err
			}
		}
		s.Audit(r, tx, o.ID, "log_export.created", "log_export:"+name, map[string]any{"kind": kind, "url": target, "bucket": r.PostFormValue("bucket")})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'logexport')`)
		return err
	})
	switch {
	case err != nil:
		s.ServerError(w, r, err)
	case formErr != "":
		web.Back(w, r, dest, "", formErr)
	default:
		web.Back(w, r, dest, "Export created. It starts with the last hour of logs, then sends new lines every minute.", "")
	}
}

func (s *API) updateLogExport(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	action := r.PostFormValue("action")
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var name string
		var err error
		switch action {
		case "delete":
			err = tx.QueryRow(r.Context(), `DELETE FROM log_exports WHERE id = $1 RETURNING name`, r.PathValue("id")).Scan(&name)
		case "pause", "resume":
			err = tx.QueryRow(r.Context(), `UPDATE log_exports SET enabled = $2 WHERE id = $1 RETURNING name`, r.PathValue("id"), action == "resume").Scan(&name)
		default:
			return pgx.ErrNoRows
		}
		if err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "log_export."+action, "log_export:"+name, nil)
		return nil
	})
	if err != nil && !web.IsNoRows(err) {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/monitoring/logs#exports", "Export updated.", "")
}

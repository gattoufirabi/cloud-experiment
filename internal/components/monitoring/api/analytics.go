package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
)

// Analytics: per-request traffic of each service (from the proxy's access logs,
// recorded by the agent) and visitor behavior (page views, sessions and custom events
// sent by a small script the application includes). Both live in VictoriaLogs under
// the an_org / an_svc / an_kind stream fields. No IP address or cookie is stored:
// visitors are a daily-salted hash of address and browser.

// ---------- collection: the page-view script ----------

const analyticsScript = `(function () {
  var s = document.currentScript, key = s && s.getAttribute("data-site");
  if (!key) return;
  var endpoint = s.src.replace(/\/a\/s\.js.*$/, "/a/e"), sid;
  try { sid = sessionStorage.getItem("_pa_s"); if (!sid) { sid = Math.random().toString(36).slice(2, 14); sessionStorage.setItem("_pa_s", sid); } } catch (e) { sid = ""; }
  function send(name, props) {
    var body = JSON.stringify({ k: key, n: name, u: location.href, r: document.referrer, w: window.innerWidth, s: sid, p: props || null });
    if (navigator.sendBeacon) navigator.sendBeacon(endpoint, body);
    else fetch(endpoint, { method: "POST", body: body, keepalive: true, mode: "no-cors" });
  }
  window.platformTrack = function (name, props) { send(String(name).slice(0, 60), props); };
  var last = "";
  function page() { if (location.href !== last) { last = location.href; send("pageview"); } }
  var push = history.pushState;
  history.pushState = function () { push.apply(this, arguments); page(); };
  window.addEventListener("popstate", page);
  page();
})();
`

func (s *API) analyticsScriptHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	_, _ = io.WriteString(w, analyticsScript)
}

type dailySalt struct {
	mu   sync.Mutex
	day  string
	salt []byte
}

func (d *dailySalt) get() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if day := time.Now().UTC().Format("2006-01-02"); day != d.day {
		d.day, d.salt = day, make([]byte, 32)
		_, _ = rand.Read(d.salt)
	}
	return d.salt
}

var (
	analyticsSalt  dailySalt
	sessionIDRe    = regexp.MustCompile(`^[a-z0-9]{6,32}$`)
	eventNameRe    = regexp.MustCompile(`^[A-Za-z0-9 _.:/-]{1,60}$`)
	propKeyRe      = regexp.MustCompile(`^[A-Za-z0-9_]{1,30}$`)
	analyticsKeyRe = regexp.MustCompile(`^[a-z0-9]{16,40}$`)
)

// analyticsCollect receives one event from the script. It always answers 204 so that
// pages never see errors; refused events are simply not stored.
func (s *API) analyticsCollect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusNoContent)
	if r.Method != http.MethodPost || s.Cfg.VictoriaLogsURL == "" || !s.Limiter.Allow("analytics:"+web.ClientIP(r), 120, time.Minute) {
		return
	}
	var ev struct {
		K, N, U, R, S string
		W             int
		P             map[string]any
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&ev) != nil || !analyticsKeyRe.MatchString(ev.K) || !eventNameRe.MatchString(ev.N) {
		return
	}
	var orgID, projectID, serviceID, svcSlug, projSlug, orgSlug string
	var hosts []string
	if err := s.Pool.QueryRow(r.Context(), `SELECT org_id, project_id, service_id, hosts, service_slug, project_slug, org_slug FROM analytics_site($1)`, ev.K).
		Scan(&orgID, &projectID, &serviceID, &hosts, &svcSlug, &projSlug, &orgSlug); err != nil {
		return
	}
	page, err := url.Parse(ev.U)
	if err != nil || (page.Scheme != "https" && page.Scheme != "http") {
		return
	}
	host := strings.ToLower(page.Hostname())
	allowed := host == model.DefaultHost(svcSlug, projSlug, orgSlug, s.Cfg.AppsDomain)
	for _, h := range hosts {
		allowed = allowed || strings.EqualFold(h, host)
	}
	if !allowed { // only the application's own pages may report events
		return
	}
	ua := r.UserAgent()
	browser, osName, device, bot := uaClass(ua)
	if bot {
		return
	}
	if ev.W > 0 && ev.W < 768 {
		device = "mobile"
	} else if ev.W >= 768 && ev.W < 1100 && device != "mobile" {
		device = "tablet"
	}
	h := sha256.New()
	h.Write(analyticsSalt.get())
	h.Write([]byte(web.ClientIP(r) + "|" + ua + "|" + serviceID))
	ref := ""
	if u, err := url.Parse(ev.R); err == nil && u.Host != "" && !strings.EqualFold(u.Hostname(), host) {
		ref = strings.ToLower(u.Hostname())
	}
	path := page.Path
	if path == "" {
		path = "/"
	}
	if len(path) > 300 {
		path = path[:300]
	}
	rec := map[string]any{
		"_time": time.Now().UTC().Format(time.RFC3339Nano), "_msg": ev.N + " " + path,
		"an_org": orgID, "an_svc": serviceID, "an_kind": "event", "an_project": projectID,
		"name": ev.N, "path": path, "referrer": ref, "browser": browser, "os": osName, "device": device,
		"visitor": hex.EncodeToString(h.Sum(nil))[:16],
	}
	if sessionIDRe.MatchString(ev.S) {
		rec["session"] = ev.S
	}
	q := page.Query()
	for _, k := range []string{"utm_source", "utm_medium", "utm_campaign"} {
		if v := q.Get(k); v != "" && len(v) <= 100 {
			rec[k] = v
		}
	}
	n := 0
	for k, v := range ev.P {
		if n >= 10 || !propKeyRe.MatchString(k) {
			continue
		}
		str := fmt.Sprint(v)
		if len(str) > 200 {
			str = str[:200]
		}
		rec["p_"+k] = str
		n++
	}
	body, _ := json.Marshal(rec)
	go func() {
		req, err := http.NewRequest(http.MethodPost, s.Cfg.VictoriaLogsURL+"/insert/jsonline?_stream_fields=an_org,an_svc,an_kind&_msg_field=_msg&_time_field=_time", bytes.NewReader(append(body, '\n')))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/stream+json")
		if resp, err := s.HTTP.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

// uaClass mirrors the agent's user-agent classification (kept small on purpose).
func uaClass(ua string) (browser, osName, device string, bot bool) {
	l := strings.ToLower(ua)
	for _, b := range []string{"bot", "crawler", "spider", "curl/", "wget/", "python-requests", "go-http-client", "headless", "preview"} {
		if strings.Contains(l, b) {
			bot = true
		}
	}
	switch {
	case strings.Contains(l, "edg/"):
		browser = "Edge"
	case strings.Contains(l, "opr/"):
		browser = "Opera"
	case strings.Contains(l, "firefox/"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	default:
		browser = "Other"
	}
	switch {
	case strings.Contains(l, "android"):
		osName = "Android"
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad"):
		osName = "iOS"
	case strings.Contains(l, "windows"):
		osName = "Windows"
	case strings.Contains(l, "macintosh"):
		osName = "macOS"
	case strings.Contains(l, "linux"):
		osName = "Linux"
	default:
		osName = "Other"
	}
	device = "desktop"
	if strings.Contains(l, "mobile") || strings.Contains(l, "android") || strings.Contains(l, "iphone") {
		device = "mobile"
	}
	return
}

// ---------- queries ----------

var analyticsPeriods = map[string]struct {
	Window, Bucket string
	Step           time.Duration
}{
	"1h":  {"1h", "5m", 5 * time.Minute},
	"24h": {"24h", "1h", time.Hour},
	"7d":  {"7d", "6h", 6 * time.Hour},
	"30d": {"30d", "1d", 24 * time.Hour},
}

type kv struct {
	Key   string
	Count int64
	Extra string
}

type seriesPoint struct {
	Time   time.Time
	Value  int64
	Errors int64
	Pct    int // height in the chart, 0 to 100
	X, Y   int // SVG position of the bar
	EY, EH int // SVG position and height of its error part
}

type analyticsReport struct {
	Period                                  string
	Requests, Visitors, Errors, Bytes       int64
	P50, P95, P99                           int64
	Series                                  []seriesPoint
	Paths, Slowest, Statuses, Referrers     []kv
	Browsers, OSes, Devices                 []kv
	Pageviews, PVisitors, Sessions, Bounced int64
	BounceRate                              int
	Pages, Events, Sources, EntryReferrers  []kv
	EventCount                              int64
	HasTraffic, HasEvents                   bool
	ChartWidth                              int
}

func (s *API) logsql(ctx context.Context, q string) ([]map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Cfg.VictoriaLogsURL+"/select/logsql/query",
		strings.NewReader(url.Values{"query": {q}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("logs store: HTTP %d: %s", resp.StatusCode, b)
	}
	var out []map[string]string
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 8<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

func n64(m map[string]string, k string) int64 {
	f, _ := strconv.ParseFloat(m[k], 64)
	return int64(f)
}

func (s *API) topBy(ctx context.Context, base, field, extra string, limit int) []kv {
	rows, err := s.logsql(ctx, fmt.Sprintf(`%s %s | stats by (%s) count() n | sort by (n desc) | limit %d`, base, extra, field, limit))
	if err != nil {
		return nil
	}
	out := make([]kv, 0, len(rows))
	for _, r := range rows {
		if r[field] == "" {
			continue
		}
		out = append(out, kv{Key: r[field], Count: n64(r, "n")})
	}
	return out
}

// analyticsFor runs the report's queries for one service.
func (s *API) analyticsFor(ctx context.Context, orgID, serviceID, period string) analyticsReport {
	p, ok := analyticsPeriods[period]
	if !ok {
		period, p = "24h", analyticsPeriods["24h"]
	}
	rep := analyticsReport{Period: period}
	if s.Cfg.VictoriaLogsURL == "" {
		return rep
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	stream := func(kind string) string {
		return fmt.Sprintf(`{an_org=%q, an_svc=%q, an_kind=%q} _time:%s`, orgID, serviceID, kind, p.Window)
	}
	acc := stream("access")
	if rows, err := s.logsql(ctx, acc+` | stats count() requests, count_uniq(visitor) if (bot:false) visitors, count() if (class:5xx) errors, sum(bytes) bytes,
		quantile(0.5, duration_ms) p50, quantile(0.95, duration_ms) p95, quantile(0.99, duration_ms) p99`); err == nil && len(rows) > 0 {
		r := rows[0]
		rep.Requests, rep.Visitors, rep.Errors, rep.Bytes = n64(r, "requests"), n64(r, "visitors"), n64(r, "errors"), n64(r, "bytes")
		rep.P50, rep.P95, rep.P99 = n64(r, "p50"), n64(r, "p95"), n64(r, "p99")
	}
	rep.HasTraffic = rep.Requests > 0
	if rep.HasTraffic {
		if rows, err := s.logsql(ctx, fmt.Sprintf(`%s | stats by (_time:%s) count() n, count() if (class:5xx) e | sort by (_time)`, acc, p.Bucket)); err == nil {
			var max int64 = 1
			for _, r := range rows {
				t, _ := time.Parse(time.RFC3339, r["_time"])
				rep.Series = append(rep.Series, seriesPoint{Time: t, Value: n64(r, "n"), Errors: n64(r, "e")})
				if v := n64(r, "n"); v > max {
					max = v
				}
			}
			for i := range rep.Series {
				pt := &rep.Series[i]
				pt.Pct = int(pt.Value * 100 / max)
				pt.X, pt.Y = i*10, 100-pt.Pct
				pt.EH = int(pt.Errors * 100 / max)
				pt.EY = 100 - pt.EH
			}
			rep.ChartWidth = len(rep.Series) * 10
		}
		// The breakdowns are independent: run them at the same time.
		var wg sync.WaitGroup
		top := func(dst *[]kv, base, field, extra string, limit int) {
			wg.Add(1)
			go func() { defer wg.Done(); *dst = s.topBy(ctx, base, field, extra, limit) }()
		}
		top(&rep.Paths, acc, "path", "", 15)
		top(&rep.Statuses, acc, "status", "", 10)
		top(&rep.Referrers, acc, "referrer", "referrer:*", 10)
		top(&rep.Browsers, acc, "browser", "bot:false", 8)
		top(&rep.OSes, acc, "os", "bot:false", 8)
		top(&rep.Devices, acc, "device", "", 5)
		if rows, err := s.logsql(ctx, acc+` | stats by (path) quantile(0.95, duration_ms) p95, count() n | filter n:>=3 | sort by (p95 desc) | limit 10`); err == nil {
			for _, r := range rows {
				rep.Slowest = append(rep.Slowest, kv{Key: r["path"], Count: n64(r, "p95"), Extra: r["n"]})
			}
		}
		wg.Wait()
	}
	ev := stream("event")
	if rows, err := s.logsql(ctx, ev+` | stats count() if (name:=pageview) pv, count_uniq(visitor) visitors, count_uniq(session) sessions, count() if (name:!=pageview) events`); err == nil && len(rows) > 0 {
		r := rows[0]
		rep.Pageviews, rep.PVisitors, rep.Sessions, rep.EventCount = n64(r, "pv"), n64(r, "visitors"), n64(r, "sessions"), n64(r, "events")
	}
	rep.HasEvents = rep.Pageviews+rep.EventCount > 0
	if rep.HasEvents {
		if rows, err := s.logsql(ctx, ev+` name:=pageview session:* | stats by (session) count() pv | stats count() sessions, count() if (pv:=1) bounced`); err == nil && len(rows) > 0 {
			rep.Bounced = n64(rows[0], "bounced")
			if total := n64(rows[0], "sessions"); total > 0 {
				rep.BounceRate = int(rep.Bounced * 100 / total)
			}
		}
		var wg sync.WaitGroup
		for _, q := range []struct {
			dst           *[]kv
			field, filter string
			limit         int
		}{{&rep.Pages, "path", "name:=pageview", 15}, {&rep.Events, "name", "name:!=pageview", 15},
			{&rep.Sources, "utm_source", "utm_source:*", 10}, {&rep.EntryReferrers, "referrer", "name:=pageview referrer:*", 10}} {
			wg.Add(1)
			go func() { defer wg.Done(); *q.dst = s.topBy(ctx, ev, q.field, q.filter, q.limit) }()
		}
		wg.Wait()
	}
	return rep
}

// ---------- console and API ----------

type analyticsService struct{ Project, Slug, ID, Key string }

func (s *API) loadAnalyticsServices(ctx context.Context, tx pgx.Tx) ([]analyticsService, error) {
	rows, err := tx.Query(ctx, `SELECT p.slug, s.slug, s.id, coalesce(s.analytics_key, '') FROM services s JOIN projects p ON p.id = s.project_id
		WHERE s.preview_of IS NULL AND NOT s.worker ORDER BY p.slug, s.slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[analyticsService])
}

func (s *API) analyticsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	var svcs []analyticsService
	if err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		svcs, err = s.loadAnalyticsServices(r.Context(), tx)
		return err
	}); err != nil {
		s.ServerError(w, r, err)
		return
	}
	sel := r.URL.Query().Get("service")
	var cur *analyticsService
	for i := range svcs {
		if svcs[i].Project+"/"+svcs[i].Slug == sel || (sel == "" && i == 0) {
			cur = &svcs[i]
		}
	}
	d := web.Data{"Org": o, "Services": svcs, "Current": cur, "Periods": []string{"1h", "24h", "7d", "30d"}, "CanEdit": model.RoleAtLeast(o.Role, "developer")}
	if cur != nil {
		rep := s.analyticsFor(r.Context(), o.ID, cur.ID, r.URL.Query().Get("period"))
		d["Report"] = rep
		if cur.Key != "" {
			d["Snippet"] = fmt.Sprintf(`<script defer src="%s/a/s.js" data-site="%s"></script>`, s.Cfg.BaseURL, cur.Key)
		}
	}
	s.Render(w, r, "analytics", http.StatusOK, d)
}

// analyticsScriptToggle turns the page-view script on (a new key) or off for a service.
func (s *API) analyticsScriptToggle(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	target := r.PostFormValue("service")
	ps, ss, _ := strings.Cut(target, "/")
	on := r.PostFormValue("on") == "1"
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var key any
		if on {
			b := make([]byte, 12)
			_, _ = rand.Read(b)
			key = hex.EncodeToString(b)
		}
		tag, err := tx.Exec(r.Context(), `UPDATE services s SET analytics_key = $3 FROM projects p WHERE p.id = s.project_id AND p.slug = $1 AND s.slug = $2`, ps, ss, key)
		if err == nil && tag.RowsAffected() == 0 {
			return errNotFound
		}
		if err == nil {
			s.Audit(r, tx, o.ID, map[bool]string{true: "analytics.script_on", false: "analytics.script_off"}[on], "service:"+target, nil)
		}
		return err
	})
	if err == errNotFound {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	msg := "Page-view script turned off; past events are kept."
	if on {
		msg = "Page-view script turned on: add the snippet below to your pages."
	}
	web.Back(w, r, o.Path()+"/analytics?service="+url.QueryEscape(target)+"#behavior", msg, "")
}

func (s *API) apiAnalytics(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var rep analyticsReport
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		rep = s.analyticsFor(r.Context(), o.ID, sv.ID, r.URL.Query().Get("period"))
		return nil
	}) {
		return
	}
	web.WriteJSON(w, rep)
}

var errNotFound = fmt.Errorf("not found")

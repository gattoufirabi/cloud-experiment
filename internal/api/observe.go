package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"platform/internal/model"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) statusJSON(w http.ResponseWriter, r *http.Request) {
	var deps []deploymentView
	_, _, _, err, ok := s.withService(w, r, "viewer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		var err error
		deps, err = recentDeployments(r, tx, sv.ID, 15)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, deps)
}

// logsJSON queries VictoriaLogs. The tenant filter is added here, server side, and the
// user's search text is always quoted, so it can never widen the query to other tenants.
func (s *Server) logsJSON(w http.ResponseWriter, r *http.Request) {
	o, _, sv, err, ok := s.withService(w, r, "viewer", func(pgx.Tx, *orgInfo, *projectInfo, *serviceInfo) error { return nil })
	if !ok {
		return
	}
	if err != nil {
		writeJSON(w, []logLine{})
		return
	}
	writeJSON(w, s.queryLogs(o.ID, sv.ID, r.URL.Query().Get("q")))
}

type logLine struct {
	Time   string `json:"time"`
	Msg    string `json:"msg"`
	Stream string `json:"stream"`
}

// queryLogs returns up to 300 lines of the last 6 hours for one service, oldest first.
// The tenant and service filters are fixed here; the search text is always quoted.
func (s *Server) queryLogs(orgID, serviceID, search string) []logLine {
	out := []logLine{}
	if s.cfg.VictoriaLogsURL == "" {
		return out
	}
	search = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, search)
	if len(search) > 100 {
		search = search[:100]
	}
	q := fmt.Sprintf(`_time:6h org_id:=%s service_id:=%s`, strconv.Quote(orgID), strconv.Quote(serviceID))
	if search != "" {
		q += " " + strconv.Quote(search)
	}
	q += " | sort by (_time desc) | limit 300"
	resp, err := s.hc.Get(s.cfg.VictoriaLogsURL + "/select/logsql/query?" + url.Values{"query": {q}}.Encode())
	if err != nil {
		s.log.Warn("query logs", "err", err)
		return out
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, logLine{Time: m["_time"], Msg: m["_msg"], Stream: m["stream"]})
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // oldest first
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// metricsJSON returns the last hour of CPU (% of one core) and memory (MB) for the service.
func (s *Server) metricsJSON(w http.ResponseWriter, r *http.Request) {
	o, _, sv, err, ok := s.withService(w, r, "viewer", func(pgx.Tx, *orgInfo, *projectInfo, *serviceInfo) error { return nil })
	if !ok {
		return
	}
	empty := map[string]any{"cpu": [][2]float64{}, "memory": [][2]float64{}, "limit_mb": 0}
	if err != nil || s.cfg.VictoriaMetricsURL == "" {
		writeJSON(w, empty)
		return
	}
	sel := fmt.Sprintf(`{org_id=%s,service_id=%s}`, strconv.Quote(o.ID), strconv.Quote(sv.ID))
	end := time.Now()
	start := end.Add(-time.Hour)
	cpu := s.queryRange(`sum(rate(platform_container_cpu_seconds_total`+sel+`[2m])) * 100`, start, end)
	mem := s.queryRange(`sum(platform_container_memory_bytes`+sel+`) / 1048576`, start, end)
	writeJSON(w, map[string]any{"cpu": cpu, "memory": mem, "limit_mb": sv.MemoryMB})
}

func (s *Server) queryRange(query string, start, end time.Time) [][2]float64 {
	u := s.cfg.VictoriaMetricsURL + "/api/v1/query_range?" + url.Values{
		"query": {query}, "start": {strconv.FormatInt(start.Unix(), 10)},
		"end": {strconv.FormatInt(end.Unix(), 10)}, "step": {"60"},
	}.Encode()
	out := [][2]float64{}
	resp, err := s.hc.Get(u)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			Result []struct {
				Values [][2]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || len(body.Data.Result) == 0 {
		return out
	}
	for _, v := range body.Data.Result[0].Values {
		ts, _ := v[0].(float64)
		str, _ := v[1].(string)
		val, err := strconv.ParseFloat(str, 64)
		if err == nil {
			out = append(out, [2]float64{ts, val})
		}
	}
	return out
}

// ---------- internal listener (not reachable through Caddy) ----------

func (s *Server) internalRoutes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	m.HandleFunc("GET /internal/tls-ask", s.tlsAsk)
	m.HandleFunc("POST /internal/registry-events", s.registryEvents)
	return m
}

// tlsAsk approves on-demand certificates only for hosts that serve a live application:
// verified custom domains and platform default hosts. Anything else is refused, so
// nobody can make the platform request certificates for arbitrary names.
func (s *Server) tlsAsk(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.URL.Query().Get("domain"))
	if host == s.cfg.ConsoleHost {
		w.WriteHeader(http.StatusOK)
		return
	}
	var ok bool
	if !model.ValidHostname(host) {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	// Function URL: <function>-<project>-<org>.fn.<apps domain>.
	if label, found := strings.CutSuffix(host, ".fn."+s.cfg.AppsDomain); found && !strings.Contains(label, ".") {
		var n int
		if err := s.pool.QueryRow(r.Context(), `SELECT count(*) FROM function_by_host($1)`, label).Scan(&n); err == nil && n > 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	if s.cfg.StorageEnabled() {
		if host == hostOnly(s.cfg.S3Host) {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Public website bucket: <bucket>.web.<apps domain>.
		if name, found := strings.CutSuffix(host, "."+s.cfg.WebDomain()); found && !strings.Contains(name, ".") {
			if err := s.pool.QueryRow(r.Context(), `SELECT bucket_website_served($1)`, name).Scan(&ok); err == nil && ok {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
	}
	if err := s.pool.QueryRow(r.Context(), `SELECT host_is_served($1, $2)`, host, s.cfg.AppsDomain).Scan(&ok); err != nil || !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

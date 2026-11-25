package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/model"
)

func (s *API) statusJSON(w http.ResponseWriter, r *http.Request) {
	var deps []web.DeploymentView
	_, _, _, err, ok := s.WithService(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		deps, err = web.RecentDeployments(r, tx, sv.ID, 15)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	web.WriteJSON(w, deps)
}

// logsJSON queries VictoriaLogs. The tenant filter is added here, server side, and the
// user's search text is always quoted, so it can never widen the query to other tenants.
func (s *API) logsJSON(w http.ResponseWriter, r *http.Request) {
	o, _, sv, err, ok := s.WithService(w, r, "viewer", func(pgx.Tx, *web.OrgInfo, *web.ProjectInfo, *web.ServiceInfo) error { return nil })
	if !ok {
		return
	}
	if err != nil {
		web.WriteJSON(w, []web.LogLine{})
		return
	}
	web.WriteJSON(w, s.QueryLogs(o.ID, sv.ID, r.URL.Query().Get("q")))
}

// metricsJSON returns the last hour of CPU (% of one core) and memory (MB) for the service.
func (s *API) metricsJSON(w http.ResponseWriter, r *http.Request) {
	o, _, sv, err, ok := s.WithService(w, r, "viewer", func(pgx.Tx, *web.OrgInfo, *web.ProjectInfo, *web.ServiceInfo) error { return nil })
	if !ok {
		return
	}
	empty := map[string]any{"cpu": [][2]float64{}, "memory": [][2]float64{}, "limit_mb": 0}
	if err != nil || s.Cfg.VictoriaMetricsURL == "" {
		web.WriteJSON(w, empty)
		return
	}
	sel := fmt.Sprintf(`{org_id=%s,service_id=%s}`, strconv.Quote(o.ID), strconv.Quote(sv.ID))
	end := time.Now()
	start := end.Add(-time.Hour)
	cpu := s.queryRange(`sum(rate(platform_container_cpu_seconds_total`+sel+`[2m])) * 100`, start, end)
	mem := s.queryRange(`sum(platform_container_memory_bytes`+sel+`) / 1048576`, start, end)
	web.WriteJSON(w, map[string]any{"cpu": cpu, "memory": mem, "limit_mb": sv.MemoryMB})
}

func (s *API) queryRange(query string, start, end time.Time) [][2]float64 {
	u := s.Cfg.VictoriaMetricsURL + "/api/v1/query_range?" + url.Values{
		"query": {query}, "start": {strconv.FormatInt(start.Unix(), 10)},
		"end": {strconv.FormatInt(end.Unix(), 10)}, "step": {"60"},
	}.Encode()
	out := [][2]float64{}
	resp, err := s.HTTP.Get(u)
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

// tlsAsk approves on-demand certificates only for hosts that serve a live application:
// verified custom domains and platform default hosts. Anything else is refused, so
// nobody can make the platform request certificates for arbitrary names.
func (s *API) tlsAsk(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.URL.Query().Get("domain"))
	if host == s.Cfg.ConsoleHost {
		w.WriteHeader(http.StatusOK)
		return
	}
	var ok bool
	if !model.ValidHostname(host) {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}
	// Function URL: <function>-<project>-<org>.fn.<apps domain>.
	if label, found := strings.CutSuffix(host, ".fn."+s.Cfg.AppsDomain); found && !strings.Contains(label, ".") {
		var n int
		if err := s.Pool.QueryRow(r.Context(), `SELECT count(*) FROM function_by_host($1)`, label).Scan(&n); err == nil && n > 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	if s.Cfg.StorageEnabled() {
		if host == web.HostOnly(s.Cfg.S3Host) {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Public website bucket: <bucket>.web.<apps domain>.
		if name, found := strings.CutSuffix(host, "."+s.Cfg.WebDomain()); found && !strings.Contains(name, ".") {
			if err := s.Pool.QueryRow(r.Context(), `SELECT bucket_website_served($1)`, name).Scan(&ok); err == nil && ok {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
	}
	if err := s.Pool.QueryRow(r.Context(), `SELECT host_is_served($1, $2)`, host, s.Cfg.AppsDomain).Scan(&ok); err != nil || !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

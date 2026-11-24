package api

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"platform/internal/core/web"
)

// Prometheus-compatible query API: /api/v1/prometheus is a Prometheus server URL for
// Grafana, promtool or any client, with an API token as bearer. Queries go to
// VictoriaMetrics with extra_label=org_id=<organization>, which VictoriaMetrics adds to
// every series selector: an organization only ever sees its own series (the platform's
// container metrics and the metrics scraped from its services).

var promPaths = map[string]bool{
	"query": true, "query_range": true, "series": true, "labels": true, "metadata": true,
	"query_exemplars": true, "status/buildinfo": true, "format_query": true,
}

func (s *API) apiPrometheus(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if s.Cfg.VictoriaMetricsURL == "" {
		web.APIError(w, http.StatusServiceUnavailable, "metrics are not enabled on this platform")
		return
	}
	rest := r.PathValue("rest")
	isLabelValues := strings.HasPrefix(rest, "label/") && strings.HasSuffix(rest, "/values") && strings.Count(rest, "/") == 2
	if !promPaths[rest] && !isLabelValues {
		web.APIError(w, http.StatusNotFound, "unsupported Prometheus API path")
		return
	}
	if err := r.ParseForm(); err != nil {
		web.APIError(w, http.StatusBadRequest, "invalid form")
		return
	}
	params := url.Values{}
	for k, vs := range r.Form {
		if k == "extra_label" || k == "extra_filters" || k == "extra_filters[]" || strings.HasPrefix(k, "nocache") {
			continue // the platform sets the organization filter itself
		}
		params[k] = vs
	}
	params.Set("extra_label", "org_id="+o.ID)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.Cfg.VictoriaMetricsURL+"/api/v1/"+rest, strings.NewReader(params.Encode()))
	if err != nil {
		web.APIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := &http.Client{Timeout: 60 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		web.APIError(w, http.StatusBadGateway, "the metrics store did not answer")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 64<<20))
}

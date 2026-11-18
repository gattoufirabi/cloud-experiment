package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Custom metrics: every 30 seconds the agent scrapes the Prometheus endpoint of each
// instance of the services that declare one (Metrics path on the service) and stores
// the samples in VictoriaMetrics with the organization, project, service and instance
// as labels. Any org_id/project_id/service_id label the application sets is dropped, so
// an application can never write series into another organization.

const (
	scrapeMaxBytes   = 4 << 20
	scrapeMaxSamples = 20000
)

var reservedLabel = regexp.MustCompile(`(^|[{,])\s*(org_id|project_id|service_id|instance|service|project)\s*=\s*"(?:[^"\\]|\\.)*"\s*,?`)

type scrapeTarget struct {
	OrgID, ProjectID, ServiceID, Service, Project, Path string
	Port                                                int
	Instances                                           []instance
}

func (a *Agent) scrapeLoop(ctx context.Context) {
	if a.cfg.VictoriaMetricsURL == "" {
		return
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		rows, err := a.pool.Query(ctx, `
			SELECT d.org_id, s.project_id, s.id, s.slug, p.slug, s.metrics_path,
			       CASE WHEN s.metrics_port > 0 THEN s.metrics_port ELSE s.port END, d.instances
			FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id JOIN orgs o ON o.id = d.org_id
			WHERE d.status = 'live' AND s.metrics_path <> '' AND NOT o.suspended AND `+mineSQL, a.mineArgs()...)
		if err != nil {
			continue
		}
		var targets []scrapeTarget
		for rows.Next() {
			var t scrapeTarget
			var insts []byte
			if rows.Scan(&t.OrgID, &t.ProjectID, &t.ServiceID, &t.Service, &t.Project, &t.Path, &t.Port, &insts) == nil {
				_ = json.Unmarshal(insts, &t.Instances)
				targets = append(targets, t)
			}
		}
		rows.Close()
		for _, t := range targets {
			for i, in := range t.Instances {
				if err := a.scrapeOne(ctx, t, i, in); err != nil {
					a.log.Debug("scrape", "service", t.ServiceID, "instance", i, "err", err)
				}
			}
		}
	}
}

var scrapeClient = &http.Client{Timeout: 5 * time.Second,
	Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// sanitizeExposition keeps the Prometheus text lines, drops the labels the platform
// sets and stops at scrapeMaxSamples samples.
func sanitizeExposition(r io.Reader) ([]byte, int) {
	var out bytes.Buffer
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if n >= scrapeMaxSamples {
			break
		}
		if i := strings.IndexByte(line, '{'); i >= 0 {
			if j := strings.LastIndexByte(line, '}'); j > i {
				labels := line[i : j+1]
				for prev := ""; prev != labels; { // adjacent reserved labels: repeat until none is left
					prev, labels = labels, reservedLabel.ReplaceAllString(labels, "$1")
				}
				labels = strings.Replace(strings.Replace(labels, ",}", "}", 1), "{ ", "{", 1)
				line = line[:i] + labels + line[j+1:]
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
		n++
	}
	return out.Bytes(), n
}

func (a *Agent) scrapeOne(ctx context.Context, t scrapeTarget, idx int, in instance) error {
	host := in.Name
	if host == "" {
		return fmt.Errorf("instance without a name")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, strconv.Itoa(t.Port))+t.Path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := scrapeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, n := sanitizeExposition(io.LimitReader(resp.Body, scrapeMaxBytes))
	if n == 0 {
		return nil
	}
	q := url.Values{}
	for _, l := range []string{"org_id=" + t.OrgID, "project_id=" + t.ProjectID, "service_id=" + t.ServiceID,
		"project=" + t.Project, "service=" + t.Service, "instance=" + strconv.Itoa(idx)} {
		q.Add("extra_label", l)
	}
	push, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.VictoriaMetricsURL+"/api/v1/import/prometheus?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	presp, err := (&http.Client{Timeout: 10 * time.Second}).Do(push)
	if err != nil {
		return err
	}
	presp.Body.Close()
	return nil
}

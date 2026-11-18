package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// Metric alarms: a PromQL query of the organization's metrics compared with a
// threshold. The agent evaluates them every minute; a breach lasting the set minutes
// opens an incident and notifies the channels (event "metric_alarm").

type alarmView struct {
	ID, Name, Query, Op, State, LastError string
	Threshold                             float64
	ForMinutes                            int
	Enabled                               bool
	LastValue                             *float64
	LastEval                              *time.Time
}

func (a alarmView) ThresholdText() string { return strconv.FormatFloat(a.Threshold, 'g', -1, 64) }
func (a alarmView) ValueText() string {
	if a.LastValue == nil {
		return "no data"
	}
	return strconv.FormatFloat(*a.LastValue, 'g', 4, 64)
}

func loadAlarms(ctx context.Context, tx pgx.Tx) ([]alarmView, error) {
	rows, err := tx.Query(ctx, `SELECT id, name, query, op, state, last_error, threshold, for_minutes, enabled, last_value, last_eval FROM alarm_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[alarmView])
}

// testPromQL runs the query within the organization and returns how many series answer.
func (s *Server) testPromQL(ctx context.Context, orgID, query string) (int, error) {
	if s.cfg.VictoriaMetricsURL == "" {
		return 0, apiErr{http.StatusServiceUnavailable, "metrics are not enabled on this platform"}
	}
	form := url.Values{"query": {query}, "extra_label": {"org_id=" + orgID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.VictoriaMetricsURL+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, apiErr{http.StatusBadGateway, "the metrics store did not answer"}
	}
	defer resp.Body.Close()
	var out struct {
		Status, Error string
		Data          struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if json.Unmarshal(body, &out) != nil || out.Status != "success" {
		msg := out.Error
		if msg == "" {
			msg = "invalid query"
		}
		return 0, apiErr{http.StatusUnprocessableEntity, "query: " + truncateStr(msg, 200)}
	}
	if out.Data.ResultType != "vector" && out.Data.ResultType != "scalar" {
		return 0, apiErr{http.StatusUnprocessableEntity, "query: it must return a number or an instant vector (use an aggregation such as max(...) or rate(...))"}
	}
	return len(out.Data.Result), nil
}

type alarmInput struct {
	Name       string  `json:"name"`
	Query      string  `json:"query"`
	Op         string  `json:"op"`
	Threshold  float64 `json:"threshold"`
	ForMinutes int     `json:"for_minutes"`
}

func (s *Server) createAlarmTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *orgInfo, in alarmInput) error {
	in.Name, in.Query = strings.TrimSpace(in.Name), strings.TrimSpace(in.Query)
	switch {
	case in.Name == "" || len(in.Name) > 80:
		return apiErr{http.StatusUnprocessableEntity, "name: 1 to 80 characters"}
	case in.Query == "" || len(in.Query) > 1000:
		return apiErr{http.StatusUnprocessableEntity, "query: 1 to 1000 characters"}
	case in.Op != ">" && in.Op != ">=" && in.Op != "<" && in.Op != "<=":
		return apiErr{http.StatusUnprocessableEntity, "op: one of >, >=, <, <="}
	case math.IsNaN(in.Threshold) || math.IsInf(in.Threshold, 0):
		return apiErr{http.StatusUnprocessableEntity, "threshold: a number"}
	case in.ForMinutes < 0 || in.ForMinutes > 120:
		return apiErr{http.StatusUnprocessableEntity, "for_minutes: 0 to 120"}
	}
	if _, err := s.testPromQL(ctx, o.ID, in.Query); err != nil {
		return err
	}
	var n, max int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM alarm_rules), max_alarms FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return err
	}
	if n >= max {
		return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d alarms", max)}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO alarm_rules (org_id, name, query, op, threshold, for_minutes) VALUES ($1, $2, $3, $4, $5, $6)`,
		o.ID, in.Name, in.Query, in.Op, in.Threshold, in.ForMinutes); err != nil {
		if isUniqueViolation(err) {
			return apiErr{http.StatusConflict, "an alarm with this name already exists"}
		}
		return err
	}
	s.audit(r, tx, o.ID, "alarm.created", "alarm:"+in.Name, map[string]any{"query": in.Query, "op": in.Op, "threshold": in.Threshold, "for_minutes": in.ForMinutes})
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'alarms')`)
	return err
}

// ---------- console ----------

func (s *Server) createAlarm(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	th, terr := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("threshold")), 64)
	in := alarmInput{Name: r.PostFormValue("name"), Query: r.PostFormValue("query"), Op: r.PostFormValue("op"), Threshold: th, ForMinutes: atoiDefault(r.PostFormValue("for_minutes"), 5)}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if terr != nil {
			return apiErr{http.StatusUnprocessableEntity, "threshold: a number"}
		}
		return s.createAlarmTx(r.Context(), r, tx, o, in)
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/monitoring#alarms", "", "Alarm not created. "+ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/monitoring#alarms", "Alarm "+strings.TrimSpace(in.Name)+" created; it is evaluated every minute.", "")
	}
}

func (s *Server) alarmAction(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	action := r.PostFormValue("action")
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var name string
		var err error
		switch action {
		case "delete":
			err = tx.QueryRow(r.Context(), `DELETE FROM alarm_rules WHERE id::text = $1 RETURNING name`, r.PathValue("id")).Scan(&name)
		case "pause", "resume":
			err = tx.QueryRow(r.Context(), `UPDATE alarm_rules SET enabled = $2, state = CASE WHEN $2 THEN state ELSE 'ok' END, pending_since = NULL
				WHERE id::text = $1 RETURNING name`, r.PathValue("id"), action == "resume").Scan(&name)
			if err == nil && action == "pause" { // a paused alarm resolves its open incident
				_, err = tx.Exec(r.Context(), `UPDATE alert_incidents SET status = 'resolved', resolved_at = now() WHERE alarm_id::text = $1 AND status = 'firing'`, r.PathValue("id"))
			}
		default:
			return apiErr{http.StatusBadRequest, "unknown action"}
		}
		if err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "alarm."+action+map[bool]string{true: "d", false: ""}[action != "delete"], "alarm:"+name, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'alarms')`)
		return err
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	var ae apiErr
	if errors.As(err, &ae) {
		back(w, r, o.Path()+"/monitoring#alarms", "", ae.msg)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/monitoring#alarms", "Alarm "+map[string]string{"delete": "deleted", "pause": "paused", "resume": "resumed"}[action]+".", "")
}

// ---------- REST API ----------

func (s *Server) apiAlarms(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []alarmView
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		out, err = loadAlarms(r.Context(), tx)
		return err
	})
	if apiTx(w, s, err) {
		if out == nil {
			out = []alarmView{}
		}
		writeJSON(w, out)
	}
}

func (s *Server) apiCreateAlarm(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var in alarmInput
	in.ForMinutes = 5
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"name": "...", "query": "...", "op": ">", "threshold": 0.8, "for_minutes": 5}`)
		return
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error { return s.createAlarmTx(r.Context(), r, tx, o, in) })
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) apiDeleteAlarm(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `DELETE FROM alarm_rules WHERE id::text = $1 OR name = $1 RETURNING name`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "alarm.deleted", "alarm:"+name, nil)
		return nil
	})
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

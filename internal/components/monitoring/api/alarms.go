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

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
)

// testPromQL runs the query within the organization and returns how many series answer.
func (s *API) testPromQL(ctx context.Context, orgID, query string) (int, error) {
	if s.Cfg.VictoriaMetricsURL == "" {
		return 0, web.APIErr{Status: http.StatusServiceUnavailable, Msg: "metrics are not enabled on this platform"}
	}
	form := url.Values{"query": {query}, "extra_label": {"org_id=" + orgID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Cfg.VictoriaMetricsURL+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return 0, web.APIErr{Status: http.StatusBadGateway, Msg: "the metrics store did not answer"}
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
		return 0, web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "query: " + web.TruncateStr(msg, 200)}
	}
	if out.Data.ResultType != "vector" && out.Data.ResultType != "scalar" {
		return 0, web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "query: it must return a number or an instant vector (use an aggregation such as max(...) or rate(...))"}
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

func (s *API) createAlarmTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, in alarmInput) error {
	in.Name, in.Query = strings.TrimSpace(in.Name), strings.TrimSpace(in.Query)
	switch {
	case in.Name == "" || len(in.Name) > 80:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "name: 1 to 80 characters"}
	case in.Query == "" || len(in.Query) > 1000:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "query: 1 to 1000 characters"}
	case in.Op != ">" && in.Op != ">=" && in.Op != "<" && in.Op != "<=":
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "op: one of >, >=, <, <="}
	case math.IsNaN(in.Threshold) || math.IsInf(in.Threshold, 0):
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "threshold: a number"}
	case in.ForMinutes < 0 || in.ForMinutes > 120:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "for_minutes: 0 to 120"}
	}
	if _, err := s.testPromQL(ctx, o.ID, in.Query); err != nil {
		return err
	}
	var n, max int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM alarm_rules), max_alarms FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return err
	}
	if n >= max {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: fmt.Sprintf("your plan allows %d alarms", max)}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO alarm_rules (org_id, name, query, op, threshold, for_minutes) VALUES ($1, $2, $3, $4, $5, $6)`,
		o.ID, in.Name, in.Query, in.Op, in.Threshold, in.ForMinutes); err != nil {
		if web.IsUniqueViolation(err) {
			return web.APIErr{Status: http.StatusConflict, Msg: "an alarm with this name already exists"}
		}
		return err
	}
	s.Audit(r, tx, o.ID, "alarm.created", "alarm:"+in.Name, map[string]any{"query": in.Query, "op": in.Op, "threshold": in.Threshold, "for_minutes": in.ForMinutes})
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'alarms')`)
	return err
}

// ---------- console ----------

func (s *API) createAlarm(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	th, terr := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("threshold")), 64)
	in := alarmInput{Name: r.PostFormValue("name"), Query: r.PostFormValue("query"), Op: r.PostFormValue("op"), Threshold: th, ForMinutes: web.AtoiDefault(r.PostFormValue("for_minutes"), 5)}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		if terr != nil {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "threshold: a number"}
		}
		return s.createAlarmTx(r.Context(), r, tx, o, in)
	})
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/monitoring#alarms", "", "Alarm not created. "+ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, o.Path()+"/monitoring#alarms", "Alarm "+strings.TrimSpace(in.Name)+" created; it is evaluated every minute.", "")
	}
}

func (s *API) alarmAction(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	action := r.PostFormValue("action")
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
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
			return web.APIErr{Status: http.StatusBadRequest, Msg: "unknown action"}
		}
		if err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "alarm."+action+map[bool]string{true: "d", false: ""}[action != "delete"], "alarm:"+name, nil)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'alarms')`)
		return err
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	var ae web.APIErr
	if errors.As(err, &ae) {
		web.Back(w, r, o.Path()+"/monitoring#alarms", "", ae.Msg)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/monitoring#alarms", "Alarm "+map[string]string{"delete": "deleted", "pause": "paused", "resume": "resumed"}[action]+".", "")
}

// ---------- REST API ----------

func (s *API) apiAlarms(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []web.AlarmView
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		out, err = web.LoadAlarms(r.Context(), tx)
		return err
	})
	if web.APITx(w, s.Server, err) {
		if out == nil {
			out = []web.AlarmView{}
		}
		web.WriteJSON(w, out)
	}
}

func (s *API) apiCreateAlarm(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in alarmInput
	in.ForMinutes = 5
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&in); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"name": "...", "query": "...", "op": ">", "threshold": 0.8, "for_minutes": 5}`)
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.createAlarmTx(r.Context(), r, tx, o, in) })
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *API) apiDeleteAlarm(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(r.Context(), `DELETE FROM alarm_rules WHERE id::text = $1 OR name = $1 RETURNING name`, r.PathValue("id")).Scan(&name); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "alarm.deleted", "alarm:"+name, nil)
		return nil
	})
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/cron"
)

// Scheduled scaling: a service set to a number of instances at given times (cron, UTC),
// for example more instances during office hours. The primary agent applies them.

const maxSchedulesPerService = 10

type scheduleView struct {
	ID        string     `json:"id"`
	Cron      string     `json:"cron"`
	Instances int        `json:"instances"`
	Label     string     `json:"label"`
	LastRun   *time.Time `json:"last_run"`
	LastError string     `json:"last_error"`
}

// NextRun is the next time the schedule applies (for the page).
func (v scheduleView) NextRun() time.Time {
	if s, err := cron.Parse(v.Cron); err == nil {
		return s.Next(time.Now().UTC())
	}
	return time.Time{}
}

func loadSchedules(ctx context.Context, tx pgx.Tx, serviceID string) ([]scheduleView, error) {
	rows, err := tx.Query(ctx, `SELECT id, cron, instances, label, last_run, last_error FROM scaling_schedules WHERE service_id = $1 ORDER BY created_at`, serviceID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[scheduleView])
	if out == nil {
		out = []scheduleView{}
	}
	return out, err
}

type scheduleInput struct {
	Cron      string `json:"cron"`
	Instances int    `json:"instances"`
	Label     string `json:"label"`
}

func (s *API) addScheduleTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, in scheduleInput) error {
	in.Cron, in.Label = strings.Join(strings.Fields(in.Cron), " "), strings.TrimSpace(in.Label)
	switch {
	case sv.IsPreview():
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "previews run a single instance"}
	case sv.VolumePath != "" || len(sv.Ports) > 0:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a service with a persistent folder or public ports runs a single instance"}
	case in.Instances < 1 || in.Instances > 10:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "instances: 1 to 10"}
	case len(in.Label) > 60:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "label: at most 60 characters"}
	}
	sched, err := cron.Parse(in.Cron)
	if err != nil {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "schedule: " + err.Error()}
	}
	if sched.MinInterval(time.Now().UTC()) < 15*time.Minute {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "schedule: at most once every 15 minutes"}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM scaling_schedules WHERE service_id = $1`, sv.ID).Scan(&n); err != nil {
		return err
	}
	if n >= maxSchedulesPerService {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a service can have at most " + strconv.Itoa(maxSchedulesPerService) + " schedules"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO scaling_schedules (org_id, service_id, cron, instances, label) VALUES ($1, $2, $3, $4, $5)`,
		o.ID, sv.ID, in.Cron, in.Instances, in.Label); err != nil {
		if web.IsUniqueViolation(err) {
			return web.APIErr{Status: http.StatusConflict, Msg: "this service already has a schedule at these times"}
		}
		return err
	}
	s.Audit(r, tx, o.ID, "service.schedule_added", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"cron": in.Cron, "instances": in.Instances})
	return nil
}

func (s *API) deleteScheduleTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, id string) error {
	var c string
	if err := tx.QueryRow(r.Context(), `DELETE FROM scaling_schedules WHERE service_id = $1 AND id::text = $2 RETURNING cron`, sv.ID, id).Scan(&c); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "service.schedule_deleted", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"cron": c})
	return nil
}

// ---------- console ----------

func (s *API) addScheduleForm(w http.ResponseWriter, r *http.Request) {
	in := scheduleInput{Cron: r.PostFormValue("cron"), Instances: web.Atoi(r.PostFormValue("instances")), Label: r.PostFormValue("label")}
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		err := s.addScheduleTx(r, tx, o, p, sv, in)
		if ae, isAE := err.(web.APIErr); isAE {
			formErr = "Schedule not added: " + ae.Msg + "."
			return nil
		}
		return err
	})
	if !ok {
		return
	}
	switch {
	case err != nil:
		s.ServerError(w, r, err)
	case formErr != "":
		web.Back(w, r, web.SvcPath(o, p, sv)+"#schedules", "", formErr)
	default:
		web.Back(w, r, web.SvcPath(o, p, sv)+"#schedules", "Schedule added: "+strconv.Itoa(in.Instances)+" instance(s) at "+in.Cron+" (UTC).", "")
	}
}

func (s *API) deleteScheduleForm(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.deleteScheduleTx(r, tx, o, p, sv, r.PathValue("id"))
	})
	if !ok {
		return
	}
	switch {
	case web.IsNoRows(err):
		web.Back(w, r, web.SvcPath(o, p, sv)+"#schedules", "", "Schedule not found.")
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, web.SvcPath(o, p, sv)+"#schedules", "Schedule deleted.", "")
	}
}

// ---------- REST ----------

func (s *API) apiSchedules(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []scheduleView
	if s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		out, err = loadSchedules(r.Context(), tx, sv.ID)
		return err
	}) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiAddSchedule(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in scheduleInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"cron": "0 8 * * 1-5", "instances": 3, "label": "office hours"}`)
		return
	}
	if s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.addScheduleTx(r, tx, o, p, sv, in)
	}) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *API) apiDeleteSchedule(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.deleteScheduleTx(r, tx, o, p, sv, r.PathValue("id"))
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

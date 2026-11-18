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

	"platform/internal/cron"
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

func (s *Server) addScheduleTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, in scheduleInput) error {
	in.Cron, in.Label = strings.Join(strings.Fields(in.Cron), " "), strings.TrimSpace(in.Label)
	switch {
	case sv.isPreview():
		return apiErr{http.StatusUnprocessableEntity, "previews run a single instance"}
	case sv.VolumePath != "" || len(sv.Ports) > 0:
		return apiErr{http.StatusUnprocessableEntity, "a service with a persistent folder or public ports runs a single instance"}
	case in.Instances < 1 || in.Instances > 10:
		return apiErr{http.StatusUnprocessableEntity, "instances: 1 to 10"}
	case len(in.Label) > 60:
		return apiErr{http.StatusUnprocessableEntity, "label: at most 60 characters"}
	}
	sched, err := cron.Parse(in.Cron)
	if err != nil {
		return apiErr{http.StatusUnprocessableEntity, "schedule: " + err.Error()}
	}
	if sched.MinInterval(time.Now().UTC()) < 15*time.Minute {
		return apiErr{http.StatusUnprocessableEntity, "schedule: at most once every 15 minutes"}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM scaling_schedules WHERE service_id = $1`, sv.ID).Scan(&n); err != nil {
		return err
	}
	if n >= maxSchedulesPerService {
		return apiErr{http.StatusUnprocessableEntity, "a service can have at most " + strconv.Itoa(maxSchedulesPerService) + " schedules"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO scaling_schedules (org_id, service_id, cron, instances, label) VALUES ($1, $2, $3, $4, $5)`,
		o.ID, sv.ID, in.Cron, in.Instances, in.Label); err != nil {
		if isUniqueViolation(err) {
			return apiErr{http.StatusConflict, "this service already has a schedule at these times"}
		}
		return err
	}
	s.audit(r, tx, o.ID, "service.schedule_added", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"cron": in.Cron, "instances": in.Instances})
	return nil
}

func (s *Server) deleteScheduleTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, id string) error {
	var c string
	if err := tx.QueryRow(r.Context(), `DELETE FROM scaling_schedules WHERE service_id = $1 AND id::text = $2 RETURNING cron`, sv.ID, id).Scan(&c); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "service.schedule_deleted", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"cron": c})
	return nil
}

// ---------- console ----------

func (s *Server) addScheduleForm(w http.ResponseWriter, r *http.Request) {
	in := scheduleInput{Cron: r.PostFormValue("cron"), Instances: atoi(r.PostFormValue("instances")), Label: r.PostFormValue("label")}
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		err := s.addScheduleTx(r, tx, o, p, sv, in)
		if ae, isAE := err.(apiErr); isAE {
			formErr = "Schedule not added: " + ae.msg + "."
			return nil
		}
		return err
	})
	if !ok {
		return
	}
	switch {
	case err != nil:
		s.serverError(w, r, err)
	case formErr != "":
		back(w, r, svcPath(o, p, sv)+"#schedules", "", formErr)
	default:
		back(w, r, svcPath(o, p, sv)+"#schedules", "Schedule added: "+strconv.Itoa(in.Instances)+" instance(s) at "+in.Cron+" (UTC).", "")
	}
}

func (s *Server) deleteScheduleForm(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		return s.deleteScheduleTx(r, tx, o, p, sv, r.PathValue("id"))
	})
	if !ok {
		return
	}
	switch {
	case isNoRows(err):
		back(w, r, svcPath(o, p, sv)+"#schedules", "", "Schedule not found.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, svcPath(o, p, sv)+"#schedules", "Schedule deleted.", "")
	}
}

// ---------- REST ----------

func (s *Server) apiSchedules(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []scheduleView
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		var err error
		out, err = loadSchedules(r.Context(), tx, sv.ID)
		return err
	}) {
		writeJSON(w, out)
	}
}

func (s *Server) apiAddSchedule(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var in scheduleInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"cron": "0 8 * * 1-5", "instances": 3, "label": "office hours"}`)
		return
	}
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		return s.addScheduleTx(r, tx, o, p, sv, in)
	}) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) apiDeleteSchedule(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		return s.deleteScheduleTx(r, tx, o, p, sv, r.PathValue("id"))
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

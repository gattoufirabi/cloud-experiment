package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/cron"
)

// Scheduled jobs of a service: a command run on a cron schedule (UTC) or on demand, in a
// fresh container made from the service's current deployment. The agent runs them.

const maxJobsPerService = 20

var jobSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,29}$`)

type jobView struct {
	ID         string     `json:"-"`
	Slug       string     `json:"id"`
	Schedule   string     `json:"schedule"`
	Command    string     `json:"command"`
	Timeout    int        `json:"timeout_seconds"`
	Enabled    bool       `json:"enabled"`
	NextRun    *time.Time `json:"next_run_at"`
	Requested  bool       `json:"run_requested"`
	LastStatus string     `json:"last_status"`
	LastRun    *time.Time `json:"last_run_at"`
}

type jobRunView struct {
	ID       string     `json:"id"`
	Job      string     `json:"job"`
	Trigger  string     `json:"trigger"`
	Status   string     `json:"status"`
	ExitCode *int       `json:"exit_code"`
	Started  time.Time  `json:"started_at"`
	Finished *time.Time `json:"finished_at"`
	Log      string     `json:"log,omitempty"`
}

// ExitNote is "exit N" for a non-zero exit code, empty otherwise.
func (r jobRunView) ExitNote() string {
	if r.ExitCode == nil || *r.ExitCode == 0 {
		return ""
	}
	return "exit " + strconv.Itoa(*r.ExitCode)
}

func (r jobRunView) Duration() string {
	if r.Finished == nil {
		return ""
	}
	d := r.Finished.Sub(r.Started).Round(time.Second)
	if d < time.Second {
		return "<1s"
	}
	return d.String()
}

func loadJobs(r *http.Request, tx pgx.Tx, serviceID string) ([]jobView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT j.id, j.slug, j.schedule, j.command, j.timeout_seconds, j.enabled, j.next_run_at, j.run_requested,
		       coalesce(lr.status, ''), lr.started_at
		FROM jobs j
		LEFT JOIN LATERAL (SELECT status, started_at FROM job_runs WHERE job_id = j.id ORDER BY started_at DESC LIMIT 1) lr ON true
		WHERE j.service_id = $1 ORDER BY j.slug`, serviceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[jobView])
}

// loadJobRuns lists recent runs of a service's jobs (or one job), logs included when withLog.
func loadJobRuns(r *http.Request, tx pgx.Tx, serviceID, jobSlug string, limit int, withLog bool) ([]jobRunView, error) {
	logExpr := "''"
	if withLog {
		logExpr = "right(r.log, 4000)"
	}
	rows, err := tx.Query(r.Context(), `
		SELECT r.id, j.slug, r.trigger, r.status, r.exit_code, r.started_at, r.finished_at, `+logExpr+`
		FROM job_runs r JOIN jobs j ON j.id = r.job_id
		WHERE j.service_id = $1 AND ($2 = '' OR j.slug = $2)
		ORDER BY r.started_at DESC LIMIT $3`, serviceID, jobSlug, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[jobRunView])
}

type jobInput struct {
	Slug, Schedule, Command string
	Timeout                 int
	Enabled                 bool
}

// check validates a job; the message is shown to the user.
func (in *jobInput) check() string {
	in.Schedule = strings.Join(strings.Fields(in.Schedule), " ")
	in.Command = strings.TrimSpace(in.Command)
	if in.Timeout == 0 {
		in.Timeout = 600
	}
	switch {
	case !jobSlugRe.MatchString(in.Slug):
		return "Job IDs use 2 to 30 lowercase letters, digits and dashes (for example nightly-report)."
	case in.Command == "" || len(in.Command) > 2000 || strings.ContainsRune(in.Command, 0):
		return "The command must be between 1 and 2000 characters."
	case in.Timeout < 10 || in.Timeout > 3600:
		return "The time limit must be between 10 and 3600 seconds."
	}
	sched, err := cron.Parse(in.Schedule)
	if err != nil {
		return "Schedule: " + err.Error()
	}
	if sched.Next(time.Now()).IsZero() {
		return "This schedule never runs (check the day and month)."
	}
	return ""
}

// saveJob creates or updates a job; a changed schedule or re-enabling recomputes the next run.
func (s *API) saveJob(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, in jobInput) (string, error) {
	var exists bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM jobs WHERE service_id = $1 AND slug = $2)`, sv.ID, in.Slug).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		if own, err := web.OnOwnServer(r.Context(), tx, p.ID); err != nil || own {
			if own {
				return web.OwnServerOnlyMsg, nil
			}
			return "", err
		}
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM jobs WHERE service_id = $1`, sv.ID).Scan(&n); err != nil {
			return "", err
		}
		if n >= maxJobsPerService {
			return "A service can have at most " + strconv.Itoa(maxJobsPerService) + " jobs.", nil
		}
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO jobs (org_id, service_id, slug, schedule, command, timeout_seconds, enabled) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (service_id, slug) DO UPDATE SET schedule = EXCLUDED.schedule, command = EXCLUDED.command,
		    timeout_seconds = EXCLUDED.timeout_seconds, enabled = EXCLUDED.enabled, next_run_at = NULL`,
		o.ID, sv.ID, in.Slug, in.Schedule, in.Command, in.Timeout, in.Enabled); err != nil {
		return "", err
	}
	s.Audit(r, tx, o.ID, "job.saved", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"job": in.Slug, "schedule": in.Schedule, "enabled": in.Enabled})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'jobs')`)
	return "", err
}

func (s *API) requestJobRun(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, slug string) (bool, error) {
	tag, err := tx.Exec(r.Context(), `UPDATE jobs SET run_requested = true WHERE service_id = $1 AND slug = $2`, sv.ID, slug)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	s.Audit(r, tx, o.ID, "job.run_requested", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"job": slug})
	_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'jobs')`)
	return true, err
}

func (s *API) removeJob(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, slug string) (bool, error) {
	tag, err := tx.Exec(r.Context(), `DELETE FROM jobs WHERE service_id = $1 AND slug = $2`, sv.ID, slug)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	s.Audit(r, tx, o.ID, "job.deleted", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"job": slug})
	return true, nil
}

// ---------- console ----------

func (s *API) saveJobForm(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		in := jobInput{Slug: strings.TrimSpace(r.PostFormValue("slug")), Schedule: r.PostFormValue("schedule"),
			Command: r.PostFormValue("command"), Timeout: web.Atoi(r.PostFormValue("timeout_seconds")), Enabled: true}
		if formErr = in.check(); formErr != "" {
			return nil
		}
		var err error
		formErr, err = s.saveJob(r, tx, o, p, sv, in)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#jobs", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#jobs", "Job saved.", "")
}

func (s *API) jobAction(w http.ResponseWriter, r *http.Request) {
	slug, action := r.PathValue("job"), r.PathValue("action")
	msg := ""
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var found bool
		var err error
		switch action {
		case "run":
			found, err = s.requestJobRun(r, tx, o, p, sv, slug)
			msg = "Job " + slug + " started. Its run appears below in a few seconds."
		case "pause", "resume":
			var tag interface{ RowsAffected() int64 }
			tag, err = tx.Exec(r.Context(), `UPDATE jobs SET enabled = $3, next_run_at = NULL WHERE service_id = $1 AND slug = $2`, sv.ID, slug, action == "resume")
			if err == nil {
				found = tag.RowsAffected() > 0
				s.Audit(r, tx, o.ID, "job."+action+"d", "service:"+p.Slug+"/"+sv.Slug, map[string]string{"job": slug})
				_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'jobs')`)
			}
			msg = "Job " + slug + " " + action + "d."
		case "delete":
			found, err = s.removeJob(r, tx, o, p, sv, slug)
			msg = "Job " + slug + " deleted."
		default:
			return pgx.ErrNoRows
		}
		if err == nil && !found {
			return pgx.ErrNoRows
		}
		return err
	})
	if !ok {
		return
	}
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#jobs", msg, "")
}

// jobRunLog shows one run's full log as plain text.
func (s *API) jobRunLog(w http.ResponseWriter, r *http.Request) {
	var text string
	_, _, _, err, ok := s.WithService(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return tx.QueryRow(r.Context(), `
			SELECT r.log FROM job_runs r JOIN jobs j ON j.id = r.job_id WHERE j.service_id = $1 AND r.id::text = $2`,
			sv.ID, r.PathValue("run")).Scan(&text)
	})
	if !ok {
		return
	}
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, text)
}

// ---------- API ----------

func (s *API) apiJobs(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var jobs []jobView
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		jobs, err = loadJobs(r, tx, sv.ID)
		return err
	}) {
		return
	}
	if jobs == nil {
		jobs = []jobView{}
	}
	web.WriteJSON(w, jobs)
}

func (s *API) apiPutJob(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var body struct {
		Schedule string `json:"schedule"`
		Command  string `json:"command"`
		Timeout  int    `json:"timeout_seconds"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"schedule": "0 3 * * *", "command": "...", "timeout_seconds": 600}`)
		return
	}
	in := jobInput{Slug: r.PathValue("job"), Schedule: body.Schedule, Command: body.Command, Timeout: body.Timeout, Enabled: body.Enabled == nil || *body.Enabled}
	if msg := in.check(); msg != "" {
		web.APIError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		msg, err := s.saveJob(r, tx, o, p, sv, in)
		if err == nil && msg != "" {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: msg}
		}
		return err
	}) {
		return
	}
	web.WriteJSON(w, map[string]any{"id": in.Slug, "schedule": in.Schedule, "enabled": in.Enabled, "status": "saved"})
}

func (s *API) apiDeleteJob(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		found, err := s.removeJob(r, tx, o, p, sv, r.PathValue("job"))
		if err == nil && !found {
			return web.APIErr{Status: http.StatusNotFound, Msg: "job not found"}
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *API) apiRunJob(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		found, err := s.requestJobRun(r, tx, o, p, sv, r.PathValue("job"))
		if err == nil && !found {
			return web.APIErr{Status: http.StatusNotFound, Msg: "job not found"}
		}
		return err
	}) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "requested"})
}

func (s *API) apiJobRuns(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var runs []jobRunView
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM jobs WHERE service_id = $1 AND slug = $2`, sv.ID, r.PathValue("job")).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return web.APIErr{Status: http.StatusNotFound, Msg: "job not found"}
		}
		var err error
		runs, err = loadJobRuns(r, tx, sv.ID, r.PathValue("job"), 20, false)
		return err
	}) {
		return
	}
	if runs == nil {
		runs = []jobRunView{}
	}
	web.WriteJSON(w, runs)
}

func (s *API) apiJobRun(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var run jobRunView
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		err := tx.QueryRow(r.Context(), `
			SELECT r.id, j.slug, r.trigger, r.status, r.exit_code, r.started_at, r.finished_at, r.log
			FROM job_runs r JOIN jobs j ON j.id = r.job_id
			WHERE j.service_id = $1 AND j.slug = $2 AND r.id::text = $3`, sv.ID, r.PathValue("job"), r.PathValue("run")).
			Scan(&run.ID, &run.Job, &run.Trigger, &run.Status, &run.ExitCode, &run.Started, &run.Finished, &run.Log)
		if errors.Is(err, pgx.ErrNoRows) {
			return web.APIErr{Status: http.StatusNotFound, Msg: "run not found"}
		}
		return err
	}) {
		return
	}
	web.WriteJSON(w, run)
}

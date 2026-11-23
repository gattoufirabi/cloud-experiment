package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/cron"
	"platform/internal/db"
	"platform/internal/functions"
)

// REST API for functions: create or update (code as a base64 zip or a single file),
// list, invoke (synchronously, or asynchronously with "X-Invocation-Type: Event", as
// AWS Lambda's Invoke), recent invocations, delete.

type apiFunction struct {
	Name            string            `json:"name"`
	Project         string            `json:"project"`
	Runtime         string            `json:"runtime"`
	Handler         string            `json:"handler"`
	MemoryMB        int               `json:"memory_mb"`
	TimeoutSeconds  int               `json:"timeout_seconds"`
	Concurrency     int               `json:"concurrency"`
	Env             map[string]string `json:"env"`
	URL             string            `json:"url,omitempty"`
	URLAuth         string            `json:"url_auth"`
	Schedule        string            `json:"schedule,omitempty"`
	ScheduleEnabled bool              `json:"schedule_enabled"`
	WebCommand      string            `json:"web_command,omitempty"`
	WebPort         int               `json:"web_port,omitempty"`
	CodeSHA256      string            `json:"code_sha256"`
	Updated         time.Time         `json:"updated_at"`
}

func (s *Server) apiFunctionView(o *orgInfo, p *projectInfo, f *functionInfo) apiFunction {
	v := apiFunction{Name: f.Slug, Project: p.Slug, Runtime: f.Runtime, Handler: f.Handler, MemoryMB: f.MemoryMB, TimeoutSeconds: f.TimeoutSeconds,
		Concurrency: f.Concurrency, Env: f.Env, URLAuth: f.URLAuth, Schedule: f.Schedule, ScheduleEnabled: f.ScheduleEnabled, CodeSHA256: f.CodeSHA, Updated: f.Updated}
	if f.URLEnabled {
		v.URL = s.functionURL(o, p, f)
	}
	return v
}

func apiTx(w http.ResponseWriter, s *Server, err error) bool {
	var ae apiErr
	switch {
	case err == nil:
		return true
	case errors.As(err, &ae):
		apiError(w, ae.status, ae.msg)
	case isNoRows(err):
		apiError(w, http.StatusNotFound, "not found")
	default:
		s.log.Error("functions api", "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	}
	return false
}

func (s *Server) apiFunctions(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	out := []apiFunction{}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT `+fnCols+` FROM functions f WHERE f.project_id = $1 ORDER BY f.slug`, p.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			f, err := scanFunction(rows)
			if err != nil {
				return err
			}
			out = append(out, s.apiFunctionView(o, p, f))
		}
		return rows.Err()
	})
	if apiTx(w, s, err) {
		writeJSON(w, out)
	}
}

// apiPutFunction creates or updates a function. Fields left out keep their value (or
// the default for a new function).
func (s *Server) apiPutFunction(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Runtime         *string            `json:"runtime"`
		Handler         *string            `json:"handler"`
		ZipBase64       *string            `json:"zip_base64"`
		Filename        *string            `json:"filename"`
		Code            *string            `json:"code"`
		MemoryMB        *int               `json:"memory_mb"`
		TimeoutSeconds  *int               `json:"timeout_seconds"`
		Concurrency     *int               `json:"concurrency"`
		Env             *map[string]string `json:"env"`
		URLEnabled      *bool              `json:"url_enabled"`
		URLAuth         *string            `json:"url_auth"`
		Schedule        *string            `json:"schedule"`
		ScheduleEnabled *bool              `json:"schedule_enabled"`
		WebCommand      *string            `json:"web_command"`
		WebPort         *int               `json:"web_port"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 15<<20)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"runtime": "nodejs22", "code": "export const handler = async () => 'hi'"}`)
		return
	}
	name := r.PathValue("fn")
	var view apiFunction
	created := false
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		f, err := scanFunction(tx.QueryRow(r.Context(), `SELECT `+fnCols+` FROM functions f WHERE f.project_id = $1 AND f.slug = $2`, p.ID, name))
		if isNoRows(err) {
			if !fnSlugRe.MatchString(name) {
				return apiErr{http.StatusUnprocessableEntity, "function name: 2 to 20 lowercase letters, digits and dashes"}
			}
			rtID := "nodejs22"
			if body.Runtime != nil {
				rtID = *body.Runtime
			}
			rt, ok := functions.FindRuntime(rtID)
			if !ok {
				return apiErr{http.StatusUnprocessableEntity, "runtime: nodejs22 or python3.13"}
			}
			var n, max int
			if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM functions), max_functions FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
				return err
			}
			if n >= max {
				return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d functions", max)}
			}
			f = &functionInfo{Slug: name, Runtime: rt.ID, Handler: rt.DefaultHandler, Code: functions.StarterZip(rt), MemoryMB: 128, TimeoutSeconds: 10,
				Concurrency: 5, Env: map[string]string{}, URLAuth: "none"}
			created = true
		} else if err != nil {
			return err
		} else if body.Runtime != nil && *body.Runtime != f.Runtime {
			return apiErr{http.StatusUnprocessableEntity, "the runtime of an existing function cannot change; create a new function"}
		}
		rt, _ := functions.FindRuntime(f.Runtime)
		if body.Handler != nil {
			f.Handler = *body.Handler
		}
		if !functions.ValidHandler(f.Handler) {
			return apiErr{http.StatusUnprocessableEntity, "handler: file.function, such as index.handler"}
		}
		switch {
		case body.ZipBase64 != nil:
			z, err := base64.StdEncoding.DecodeString(*body.ZipBase64)
			if err != nil {
				return apiErr{http.StatusUnprocessableEntity, "zip_base64 is not valid base64"}
			}
			if _, err := functions.ReadZip(z); err != nil {
				return apiErr{http.StatusUnprocessableEntity, "code archive: " + err.Error()}
			}
			f.Code = z
		case body.Code != nil:
			files, err := functions.ReadZip(f.Code)
			if err != nil {
				files = map[string][]byte{}
			}
			fname := functions.MainFile(rt, f.Handler, files)
			if f.WebCommand != "" {
				fname = functions.WebMainFile(rt, f.WebCommand, files)
			}
			if body.Filename != nil && *body.Filename != "" {
				fname = *body.Filename
			}
			if strings.Contains(fname, "..") || strings.HasPrefix(fname, "/") {
				return apiErr{http.StatusUnprocessableEntity, "invalid filename"}
			}
			files[fname] = []byte(*body.Code)
			if f.Code, err = functions.BuildZip(files); err != nil {
				return err
			}
		}
		if body.MemoryMB != nil {
			f.MemoryMB = *body.MemoryMB
		}
		if body.TimeoutSeconds != nil {
			f.TimeoutSeconds = *body.TimeoutSeconds
		}
		if body.Concurrency != nil {
			f.Concurrency = *body.Concurrency
		}
		if body.Env != nil {
			var lines []string
			for k, v := range *body.Env {
				lines = append(lines, k+"="+v)
			}
			env, err := parseEnvText(strings.Join(lines, "\n"))
			if err != nil {
				return apiErr{http.StatusUnprocessableEntity, "env: " + err.Error()}
			}
			f.Env = env
		}
		if body.URLEnabled != nil {
			f.URLEnabled = *body.URLEnabled
		}
		if body.URLAuth != nil {
			f.URLAuth = *body.URLAuth
		}
		if body.Schedule != nil {
			f.Schedule = strings.TrimSpace(*body.Schedule)
		}
		if body.ScheduleEnabled != nil {
			f.ScheduleEnabled = *body.ScheduleEnabled
		}
		if body.WebCommand != nil {
			f.WebCommand = strings.TrimSpace(*body.WebCommand)
		}
		if body.WebPort != nil {
			f.WebPort = *body.WebPort
		}
		if f.WebPort == 0 {
			f.WebPort = 8080
		}
		if !validWebCommand(f.WebCommand) || f.WebPort < 1024 || f.WebPort > 65535 {
			return apiErr{http.StatusUnprocessableEntity, "web_command: one line, 300 characters at most; web_port: 1024 to 65535"}
		}
		switch {
		case f.MemoryMB < 128 || f.MemoryMB > 2048:
			return apiErr{http.StatusUnprocessableEntity, "memory_mb: 128 to 2048"}
		case f.TimeoutSeconds < 1 || f.TimeoutSeconds > 300:
			return apiErr{http.StatusUnprocessableEntity, "timeout_seconds: 1 to 300"}
		case f.Concurrency < 1 || f.Concurrency > 50:
			return apiErr{http.StatusUnprocessableEntity, "concurrency: 1 to 50"}
		case f.URLAuth != "none" && f.URLAuth != "token":
			return apiErr{http.StatusUnprocessableEntity, `url_auth: "none" or "token"`}
		case f.ScheduleEnabled && f.Schedule == "":
			return apiErr{http.StatusUnprocessableEntity, "schedule_enabled needs a schedule"}
		}
		if f.Schedule != "" {
			sch, err := cron.Parse(f.Schedule)
			if err != nil {
				return apiErr{http.StatusUnprocessableEntity, "schedule: " + err.Error()}
			}
			if sch.MinInterval(time.Now()) < time.Minute {
				return apiErr{http.StatusUnprocessableEntity, "schedule: at most once a minute"}
			}
		}
		sum := sha256.Sum256(f.Code)
		f.CodeSHA = hex.EncodeToString(sum[:])
		envJSON, _ := json.Marshal(f.Env)
		if created {
			if own, err := onOwnServer(r.Context(), tx, p.ID); err != nil || own {
				if own {
					return apiErr{http.StatusConflict, ownServerOnlyMsg}
				}
				return err
			}
			err = tx.QueryRow(r.Context(), `
				INSERT INTO functions (org_id, project_id, slug, runtime, handler, code, code_sha256, memory_mb, timeout_seconds, concurrency, env,
				                       url_enabled, url_auth, schedule, schedule_enabled, web_command, web_port)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17) RETURNING id, created_at, updated_at`,
				o.ID, p.ID, f.Slug, f.Runtime, f.Handler, f.Code, f.CodeSHA, f.MemoryMB, f.TimeoutSeconds, f.Concurrency, envJSON,
				f.URLEnabled, f.URLAuth, f.Schedule, f.ScheduleEnabled, f.WebCommand, f.WebPort).Scan(&f.ID, &f.Created, &f.Updated)
		} else {
			err = tx.QueryRow(r.Context(), `
				UPDATE functions SET handler = $2, code = $3, code_sha256 = $4, memory_mb = $5, timeout_seconds = $6, concurrency = $7, env = $8,
				       url_enabled = $9, url_auth = $10, next_run_at = CASE WHEN schedule = $11 AND schedule_enabled = $12 THEN next_run_at END,
				       schedule = $11, schedule_enabled = $12, web_command = $13, web_port = $14, updated_at = now()
				WHERE id = $1 RETURNING updated_at`,
				f.ID, f.Handler, f.Code, f.CodeSHA, f.MemoryMB, f.TimeoutSeconds, f.Concurrency, envJSON, f.URLEnabled, f.URLAuth, f.Schedule, f.ScheduleEnabled,
				f.WebCommand, f.WebPort).Scan(&f.Updated)
		}
		if err != nil {
			return err
		}
		action := "function.updated"
		if created {
			action = "function.created"
		}
		s.audit(r, tx, o.ID, action, "function:"+p.Slug+"/"+f.Slug, map[string]any{"code_sha256": f.CodeSHA[:16], "memory_mb": f.MemoryMB})
		view = s.apiFunctionView(o, p, f)
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'fn')`)
		return err
	})
	if !apiTx(w, s, err) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	}
	_ = json.NewEncoder(w).Encode(view)
}

func (s *Server) apiDeleteFunction(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(r.Context(), `DELETE FROM functions WHERE project_id = $1 AND slug = $2 RETURNING id`, p.ID, r.PathValue("fn")).Scan(&id); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "function.deleted", "function:"+p.Slug+"/"+r.PathValue("fn"), nil)
		return nil
	})
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiInvoke runs the function with the request body as its event. The answer is the
// handler's result; a handler error answers 200 with X-Platform-Function-Error, as AWS
// Lambda's Invoke does. "X-Invocation-Type: Event" queues it and answers 202.
func (s *Server) apiInvoke(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	event, err := io.ReadAll(io.LimitReader(r.Body, functions.MaxPayloadBytes+1))
	if err != nil || len(event) > functions.MaxPayloadBytes {
		apiError(w, http.StatusRequestEntityTooLarge, "the event must be smaller than 6 MB")
		return
	}
	if len(strings.TrimSpace(string(event))) == 0 {
		event = []byte("{}")
	}
	if !json.Valid(event) {
		apiError(w, http.StatusBadRequest, "the event must be JSON")
		return
	}
	async := strings.EqualFold(r.Header.Get("X-Invocation-Type"), "Event")
	var f *functionInfo
	err = db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		f, err = scanFunction(tx.QueryRow(r.Context(), `SELECT `+fnCols+` FROM functions f WHERE f.project_id = $1 AND f.slug = $2`, p.ID, r.PathValue("fn")))
		return err
	})
	if !apiTx(w, s, err) {
		return
	}
	id, err := s.queueInvocation(r.Context(), o.ID, f.ID, "api", event, async)
	if errors.Is(err, errThrottled) {
		w.Header().Set("Retry-After", "1")
		apiError(w, http.StatusTooManyRequests, "too many invocations waiting for this function (concurrency limit)")
		return
	}
	if !apiTx(w, s, err) {
		return
	}
	w.Header().Set("X-Platform-Invocation-Id", id)
	if async {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"invocation_id": id, "status": "queued"})
		return
	}
	inv, err := s.awaitInvocation(r.Context(), o.ID, id, invokeWait(f))
	if err != nil {
		apiError(w, http.StatusGatewayTimeout, "the function did not answer in time; see its invocations")
		return
	}
	if r.Header.Get("X-Log-Type") == "Tail" {
		tail := inv.Log
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		w.Header().Set("X-Platform-Log-Result", base64.StdEncoding.EncodeToString([]byte(tail)))
	}
	if inv.DurationMS != nil {
		w.Header().Set("X-Platform-Duration-Ms", fmt.Sprint(*inv.DurationMS))
	}
	w.Header().Set("Content-Type", "application/json")
	var res struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if inv.Status == "succeeded" && json.Unmarshal(inv.Response, &res) == nil && res.OK {
		_, _ = w.Write(res.Result)
		return
	}
	w.Header().Set("X-Platform-Function-Error", "Unhandled")
	errType, msg := inv.ErrorType, inv.Error
	if inv.Status == "timed_out" && errType == "" {
		errType = "Sandbox.Timedout"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"errorType": errType, "errorMessage": msg})
}

func (s *Server) apiInvocations(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	type row struct {
		ID         string    `json:"id"`
		Trigger    string    `json:"trigger"`
		Status     string    `json:"status"`
		ErrorType  string    `json:"error_type,omitempty"`
		Error      string    `json:"error,omitempty"`
		Log        string    `json:"log"`
		DurationMS *int      `json:"duration_ms"`
		Cold       bool      `json:"cold"`
		Created    time.Time `json:"created_at"`
	}
	out := []row{}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `
			SELECT i.id, i.trigger, i.status, i.error_type, i.error, i.log, i.duration_ms, i.cold, i.created_at
			FROM function_invocations i JOIN functions f ON f.id = i.function_id
			WHERE f.project_id = $1 AND f.slug = $2 ORDER BY i.created_at DESC LIMIT 50`, p.ID, r.PathValue("fn"))
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByPos[row])
		return err
	})
	if apiTx(w, s, err) {
		writeJSON(w, out)
	}
}

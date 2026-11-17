package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"platform/internal/cron"
	"platform/internal/db"
	"platform/internal/functions"
	"platform/internal/model"
	"platform/internal/secure"
)

// Serverless functions in the API: the console pages, the REST API, and the gateway
// behind function URLs (https://<function>-<project>-<org>.fn.<apps domain>/). An
// invocation is queued in function_invocations; the agent of the project's node runs it
// and writes the result back, then notifies "fn_done", which wakes the waiting request.

var fnSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,19}$`)

type functionInfo struct {
	ID, Slug, Runtime, Handler, CodeSHA, URLAuth, Schedule string
	Code                                                   []byte
	MemoryMB, TimeoutSeconds, Concurrency                  int
	Env                                                    map[string]string
	URLEnabled, ScheduleEnabled, HasToken                  bool
	NextRun                                                *time.Time
	Created, Updated                                       time.Time
}

func (f *functionInfo) RuntimeName() string {
	if rt, ok := functions.FindRuntime(f.Runtime); ok {
		return rt.Name
	}
	return f.Runtime
}

// EnvText is the variables as KEY=VALUE lines, for the settings form.
func (f *functionInfo) EnvText() string {
	keys := make([]string, 0, len(f.Env))
	for k := range f.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + f.Env[k] + "\n")
	}
	return b.String()
}

const fnCols = `f.id, f.slug, f.runtime, f.handler, f.code_sha256, f.url_auth, f.schedule, f.code, f.memory_mb, f.timeout_seconds, f.concurrency,
	f.env, f.url_enabled, f.schedule_enabled, f.url_token_hash IS NOT NULL, f.next_run_at, f.created_at, f.updated_at`

func scanFunction(row pgx.Row) (*functionInfo, error) {
	var f functionInfo
	var env []byte
	err := row.Scan(&f.ID, &f.Slug, &f.Runtime, &f.Handler, &f.CodeSHA, &f.URLAuth, &f.Schedule, &f.Code, &f.MemoryMB, &f.TimeoutSeconds,
		&f.Concurrency, &env, &f.URLEnabled, &f.ScheduleEnabled, &f.HasToken, &f.NextRun, &f.Created, &f.Updated)
	_ = json.Unmarshal(env, &f.Env)
	if f.Env == nil {
		f.Env = map[string]string{}
	}
	return &f, err
}

func (s *Server) functionURL(o *orgInfo, p *projectInfo, f *functionInfo) string {
	scheme, port := "http", ""
	if s.cfg.TLSMode == "acme" {
		scheme = "https"
	} else if _, pt, ok := strings.Cut(strings.TrimPrefix(s.cfg.BaseURL, "http://"), ":"); ok {
		port = ":" + pt // development: same port as the console
	}
	return scheme + "://" + f.Slug + "-" + p.Slug + "-" + o.Slug + ".fn." + s.cfg.AppsDomain + port + "/"
}

func fnPath(o *orgInfo, p *projectInfo, f *functionInfo) string {
	return o.Path() + "/p/" + p.Slug + "/f/" + f.Slug
}

func (s *Server) withFunction(w http.ResponseWriter, r *http.Request, minRole string,
	fn func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error) (*orgInfo, *projectInfo, *functionInfo, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, nil, false
	}
	var p *projectInfo
	var f *functionInfo
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.loadProject(r, tx); err != nil {
			return err
		}
		if f, err = scanFunction(tx.QueryRow(r.Context(), `SELECT `+fnCols+` FROM functions f WHERE f.project_id = $1 AND f.slug = $2`, p.ID, r.PathValue("fn"))); err != nil {
			return err
		}
		return fn(tx, o, p, f)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, nil, false
	}
	return o, p, f, err, true
}

// ---------- invocations ----------

type invocation struct {
	ID, Trigger, Status, ErrorType, Error, Log string
	Response                                   []byte
	DurationMS                                 *int
	Cold                                       bool
	Created                                    time.Time
}

// Pretty is the handler's result (or error) formatted for the console.
func (i *invocation) Pretty() string {
	var res struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(i.Response, &res) == nil && res.OK {
		var v any
		if json.Unmarshal(res.Result, &v) == nil {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		}
	}
	if len(i.Response) > 0 {
		var v any
		if json.Unmarshal(i.Response, &v) == nil {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		}
	}
	return ""
}

var errThrottled = errors.New("too many invocations waiting for this function")

// queueInvocation records an invocation and wakes the agent. It refuses when the
// function already has four times its concurrency queued or running (throttling).
func (s *Server) queueInvocation(ctx context.Context, orgID, fnID, trigger string, event []byte, async bool) (string, error) {
	var id string
	err := db.WithOrg(ctx, s.pool, orgID, func(tx pgx.Tx) error {
		var backlog, conc int
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM function_invocations WHERE function_id = $1 AND status IN ('queued', 'running')), concurrency
			FROM functions WHERE id = $1`, fnID).Scan(&backlog, &conc); err != nil {
			return err
		}
		if backlog >= conc*4 {
			return errThrottled
		}
		if err := tx.QueryRow(ctx, `INSERT INTO function_invocations (org_id, function_id, trigger, async, event) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			orgID, fnID, trigger, async, event).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'fn')`)
		return err
	})
	return id, err
}

// fnWaiters wakes requests waiting for an invocation when the agent notifies fn_done.
type fnWaiters struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (fw *fnWaiters) add(id string) chan struct{} {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.m == nil {
		fw.m = map[string]chan struct{}{}
	}
	ch := make(chan struct{}, 1)
	fw.m[id] = ch
	return ch
}

func (fw *fnWaiters) done(id string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if ch, ok := fw.m[id]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (fw *fnWaiters) remove(id string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	delete(fw.m, id)
}

// listenFnDone keeps a connection listening for finished invocations.
func (s *Server) listenFnDone(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := s.pool.Acquire(ctx)
		if err == nil {
			if _, err = conn.Exec(ctx, `LISTEN fn_done`); err == nil {
				for {
					n, werr := conn.Conn().WaitForNotification(ctx)
					if werr != nil {
						err = werr
						break
					}
					s.fnWait.done(n.Payload)
				}
			}
			conn.Release()
		}
		if ctx.Err() == nil {
			s.log.Warn("function result listener restarting", "err", err)
			time.Sleep(2 * time.Second)
		}
	}
}

// awaitInvocation waits until the invocation finishes (or wait elapses) and returns it.
func (s *Server) awaitInvocation(ctx context.Context, orgID, id string, wait time.Duration) (*invocation, error) {
	ch := s.fnWait.add(id)
	defer s.fnWait.remove(id)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(time.Second) // in case a notification is missed
	defer poll.Stop()
	for {
		inv, err := s.loadInvocation(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		if inv.Status != "queued" && inv.Status != "running" {
			_ = db.WithOrg(context.WithoutCancel(ctx), s.pool, orgID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE function_invocations SET response = NULL WHERE id = $1`, id)
				return err
			})
			return inv, nil
		}
		select {
		case <-ctx.Done():
			return inv, ctx.Err()
		case <-deadline.C:
			return inv, context.DeadlineExceeded
		case <-ch:
		case <-poll.C:
		}
	}
}

func (s *Server) loadInvocation(ctx context.Context, orgID, id string) (*invocation, error) {
	var inv invocation
	err := db.WithOrg(ctx, s.pool, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, trigger, status, error_type, error, log, coalesce(response, '\x'::bytea), duration_ms, cold, created_at
			FROM function_invocations WHERE id = $1`, id).
			Scan(&inv.ID, &inv.Trigger, &inv.Status, &inv.ErrorType, &inv.Error, &inv.Log, &inv.Response, &inv.DurationMS, &inv.Cold, &inv.Created)
	})
	return &inv, err
}

// invokeWait is how long a synchronous caller waits: the timeout plus room for a cold
// start (the first one may pull the runtime image).
func invokeWait(f *functionInfo) time.Duration {
	return time.Duration(f.TimeoutSeconds)*time.Second + 90*time.Second
}

// ---------- function URLs ----------

// functionHosts sends requests for function URLs to the gateway; everything else goes
// to the console.
func (s *Server) functionHosts(next http.Handler) http.Handler {
	suffix := ".fn." + s.cfg.AppsDomain
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
		if label, ok := strings.CutSuffix(host, suffix); ok && !strings.Contains(label, ".") && label != "" {
			s.functionGateway(w, r, label, host)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func fnJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"Message": msg})
}

func textual(ctype string) bool {
	mt, _, _ := mime.ParseMediaType(ctype)
	return mt == "" || strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json") ||
		mt == "application/xml" || strings.HasSuffix(mt, "+xml") || mt == "application/x-www-form-urlencoded" || mt == "application/javascript"
}

// functionGateway turns an HTTP request into a Lambda function-URL event (payload
// format 2.0), invokes the function and turns its result into the HTTP response.
func (s *Server) functionGateway(w http.ResponseWriter, r *http.Request, label, host string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var orgID, fnID, auth string
	var tokenHash []byte
	err := s.pool.QueryRow(r.Context(), `SELECT org_id, function_id, url_auth, url_token_hash FROM function_by_host($1)`, label).Scan(&orgID, &fnID, &auth, &tokenHash)
	if err != nil {
		fnJSONError(w, http.StatusNotFound, "Not Found")
		return
	}
	if auth == "token" {
		tok := r.Header.Get("X-Function-Token")
		if tok == "" || tokenHash == nil || subtle.ConstantTimeCompare(secure.HashToken(tok), tokenHash) != 1 {
			fnJSONError(w, http.StatusForbidden, "Forbidden")
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, functions.MaxPayloadBytes+1))
	if err != nil || len(body) > functions.MaxPayloadBytes {
		fnJSONError(w, http.StatusRequestEntityTooLarge, "Request must be smaller than 6 MB")
		return
	}
	headers := map[string]string{}
	var cookies []string
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "cookie" {
			for _, v := range vs {
				for _, c := range strings.Split(v, ";") {
					if c = strings.TrimSpace(c); c != "" {
						cookies = append(cookies, c)
					}
				}
			}
			continue
		}
		if lk == "x-function-token" {
			continue
		}
		headers[lk] = strings.Join(vs, ",")
	}
	headers["host"] = host
	qs := map[string]string{}
	for k, vs := range r.URL.Query() {
		qs[k] = strings.Join(vs, ",")
	}
	now := time.Now().UTC()
	reqID := newRequestID()
	ev := map[string]any{
		"version": "2.0", "routeKey": "$default", "rawPath": r.URL.EscapedPath(), "rawQueryString": r.URL.RawQuery,
		"headers": headers, "requestContext": map[string]any{
			"accountId": "anonymous", "apiId": label, "domainName": host, "domainPrefix": label,
			"http":      map[string]any{"method": r.Method, "path": r.URL.Path, "protocol": r.Proto, "sourceIp": clientIP(r), "userAgent": r.UserAgent()},
			"requestId": reqID, "routeKey": "$default", "stage": "$default", "time": now.Format("02/Jan/2006:15:04:05 -0700"), "timeEpoch": now.UnixMilli(),
		},
		"isBase64Encoded": false,
	}
	if len(cookies) > 0 {
		ev["cookies"] = cookies
	}
	if len(qs) > 0 {
		ev["queryStringParameters"] = qs
	}
	if len(body) > 0 {
		if textual(r.Header.Get("Content-Type")) && utf8.Valid(body) {
			ev["body"] = string(body)
		} else {
			ev["body"], ev["isBase64Encoded"] = base64.StdEncoding.EncodeToString(body), true
		}
	}
	event, _ := json.Marshal(ev)
	id, err := s.queueInvocation(r.Context(), orgID, fnID, "url", event, false)
	switch {
	case errors.Is(err, errThrottled):
		fnJSONError(w, http.StatusTooManyRequests, "Rate Exceeded.")
		return
	case err != nil:
		s.log.Error("queue function invocation", "err", err)
		fnJSONError(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	inv, err := s.awaitInvocation(r.Context(), orgID, id, 390*time.Second)
	w.Header().Set("X-Platform-Request-Id", id)
	if err != nil {
		fnJSONError(w, http.StatusGatewayTimeout, "Endpoint request timed out")
		return
	}
	writeFunctionHTTP(w, inv)
}

// writeFunctionHTTP maps a function's result to an HTTP response, as function URLs do:
// a result with statusCode is a response; anything else is a 200 JSON body.
func writeFunctionHTTP(w http.ResponseWriter, inv *invocation) {
	if inv.Status == "timed_out" {
		fnJSONError(w, http.StatusGatewayTimeout, "Task timed out")
		return
	}
	var res struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if inv.Status != "succeeded" || json.Unmarshal(inv.Response, &res) != nil || !res.OK {
		fnJSONError(w, http.StatusBadGateway, "Internal Server Error")
		return
	}
	var structured struct {
		StatusCode      *int           `json:"statusCode"`
		Headers         map[string]any `json:"headers"`
		Cookies         []string       `json:"cookies"`
		Body            *string        `json:"body"`
		IsBase64Encoded bool           `json:"isBase64Encoded"`
	}
	if len(res.Result) > 0 && res.Result[0] == '{' && json.Unmarshal(res.Result, &structured) == nil && structured.StatusCode != nil {
		for k, v := range structured.Headers {
			w.Header().Set(k, fmt.Sprint(v))
		}
		for _, c := range structured.Cookies {
			w.Header().Add("Set-Cookie", c)
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		code := *structured.StatusCode
		if code < 100 || code > 599 {
			code = http.StatusBadGateway
		}
		w.WriteHeader(code)
		if structured.Body != nil {
			if structured.IsBase64Encoded {
				b, err := base64.StdEncoding.DecodeString(*structured.Body)
				if err == nil {
					_, _ = w.Write(b)
				}
				return
			}
			_, _ = io.WriteString(w, *structured.Body)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	var str string
	if json.Unmarshal(res.Result, &str) == nil {
		_, _ = io.WriteString(w, str)
		return
	}
	_, _ = w.Write(res.Result)
}

func newRequestID() string {
	return strings.ToLower(secure.NewToken()[:24])
}

// ---------- console ----------

type functionRow struct {
	Project, ProjectName, Slug, Runtime string
	MemoryMB                            int
	URLEnabled, ScheduleEnabled         bool
	Invocations, Errors                 int
	Updated                             time.Time
}

func (s *Server) functionsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	scope := s.scopeProject(r, o)
	var rows []functionRow
	var projects []projectOption
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if projects, err = loadProjectOptions(r, tx); err != nil {
			return err
		}
		q, err := tx.Query(r.Context(), `
			SELECT p.slug, p.name, f.slug, f.runtime, f.memory_mb, f.url_enabled, f.schedule_enabled,
			       (SELECT count(*) FROM function_invocations i WHERE i.function_id = f.id AND i.created_at > now() - interval '24 hours'),
			       (SELECT count(*) FROM function_invocations i WHERE i.function_id = f.id AND i.created_at > now() - interval '24 hours' AND i.status IN ('failed', 'timed_out')),
			       f.updated_at
			FROM functions f JOIN projects p ON p.id = f.project_id
			WHERE $1 = '' OR p.slug = $1 ORDER BY p.name, f.slug`, scope)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(q, pgx.RowToStructByPos[functionRow])
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "functions", http.StatusOK, data{"Org": o, "Functions": rows, "Projects": projects, "Scope": scope,
		"Runtimes": functions.Runtimes, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func runtimeLabel(id string) string {
	if rt, ok := functions.FindRuntime(id); ok {
		return rt.Name
	}
	return id
}

func (s *Server) createFunction(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	project := r.PostFormValue("project")
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	rt, rtOK := functions.FindRuntime(r.PostFormValue("runtime"))
	back0 := o.Path() + "/functions#create"
	if !fnSlugRe.MatchString(slug) || !rtOK {
		back(w, r, back0, "", "Function name: 2 to 20 lowercase letters, digits and dashes; choose a runtime.")
		return
	}
	var formErr string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var projectID string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE slug = $1`, project).Scan(&projectID); err != nil {
			formErr = "Choose a project."
			return nil
		}
		var n, max int
		if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM functions), max_functions FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
			return err
		}
		if n >= max {
			formErr = fmt.Sprintf("Your plan allows %d functions.", max)
			return nil
		}
		code := functions.StarterZip(rt)
		sum := sha256.Sum256(code)
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO functions (org_id, project_id, slug, runtime, handler, code, code_sha256) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			o.ID, projectID, slug, rt.ID, rt.DefaultHandler, code, hex.EncodeToString(sum[:])); err != nil {
			if isUniqueViolation(err) {
				formErr = "A function with this name already exists in the project."
				return nil
			}
			return err
		}
		s.audit(r, tx, o.ID, "function.created", "function:"+project+"/"+slug, map[string]string{"runtime": rt.ID})
		return nil
	})
	switch {
	case err != nil:
		s.serverError(w, r, err)
	case formErr != "":
		back(w, r, back0, "", formErr)
	default:
		back(w, r, o.Path()+"/p/"+project+"/f/"+slug, "Function created with starter code. Edit it on the Code tab and test it.", "")
	}
}

func (s *Server) functionPage(w http.ResponseWriter, r *http.Request) {
	var invs []invocation
	type stats struct {
		Count, Errors, Cold int
		AvgMS               *float64
	}
	var st stats
	o, p, f, err, ok := s.withFunction(w, r, "viewer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		rows, err := tx.Query(r.Context(), `
			SELECT id, trigger, status, error_type, error, log, '\x'::bytea, duration_ms, cold, created_at
			FROM function_invocations WHERE function_id = $1 ORDER BY created_at DESC LIMIT 50`, f.ID)
		if err != nil {
			return err
		}
		if invs, err = pgx.CollectRows(rows, pgx.RowToStructByPos[invocation]); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `
			SELECT count(*), count(*) FILTER (WHERE status IN ('failed', 'timed_out')), count(*) FILTER (WHERE cold), avg(duration_ms)::float8
			FROM function_invocations WHERE function_id = $1 AND created_at > now() - interval '24 hours'`, f.ID).Scan(&st.Count, &st.Errors, &st.Cold, &st.AvgMS)
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderFunction(w, r, o, p, f, data{"Invocations": invs, "Stats": st})
}

func (s *Server) renderFunction(w http.ResponseWriter, r *http.Request, o *orgInfo, p *projectInfo, f *functionInfo, d data) {
	rt, _ := functions.FindRuntime(f.Runtime)
	files, err := functions.ReadZip(f.Code)
	var mainFile, source string
	var names []string
	editable := false
	if err == nil {
		mainFile = functions.MainFile(rt, f.Handler, files)
		if b, ok := files[mainFile]; ok && utf8.Valid(b) && len(b) <= 256<<10 {
			source, editable = string(b), true
		} else if !ok {
			editable = true // the handler's file does not exist yet: the editor creates it
		}
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
	}
	if _, ok := d["TestEvent"]; !ok {
		d["TestEvent"] = "{\n  \"key\": \"value\"\n}"
	}
	d["Org"], d["Project"], d["Function"], d["Path"] = o, p, f, fnPath(o, p, f)
	d["MainFile"], d["Source"], d["Files"], d["Editable"], d["CodeSize"] = mainFile, source, names, editable, len(f.Code)
	d["URL"], d["CanEdit"], d["Area"] = s.functionURL(o, p, f), model.RoleAtLeast(o.Role, "developer"), "functions"
	s.render(w, r, "function", http.StatusOK, d)
}

// saveFunctionCode stores the editor's content as the handler's file, or replaces the
// whole archive with an uploaded zip.
func (s *Server) saveFunctionCode(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		var code []byte
		if up, hdr, ferr := r.FormFile("zip"); ferr == nil && hdr.Size > 0 {
			defer up.Close()
			b, err := io.ReadAll(io.LimitReader(up, functions.MaxZipBytes+1))
			if err != nil {
				return err
			}
			if _, err := functions.ReadZip(b); err != nil {
				formErr = "Code archive: " + err.Error()
				return nil
			}
			code = b
		} else {
			rt, _ := functions.FindRuntime(f.Runtime)
			files, err := functions.ReadZip(f.Code)
			if err != nil {
				files = map[string][]byte{}
			}
			src := strings.ReplaceAll(r.PostFormValue("code"), "\r\n", "\n")
			if len(src) > 256<<10 {
				formErr = "The editor holds up to 256 KB; upload a zip archive for larger code."
				return nil
			}
			files[functions.MainFile(rt, f.Handler, files)] = []byte(src)
			if code, err = functions.BuildZip(files); err != nil {
				return err
			}
		}
		sum := sha256.Sum256(code)
		if _, err := tx.Exec(r.Context(), `UPDATE functions SET code = $2, code_sha256 = $3, updated_at = now() WHERE id = $1`, f.ID, code, hex.EncodeToString(sum[:])); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "function.code_updated", "function:"+p.Slug+"/"+f.Slug, map[string]any{"bytes": len(code), "sha256": hex.EncodeToString(sum[:8])})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, fnPath(o, p, f)+"#code", "", formErr)
		return
	}
	back(w, r, fnPath(o, p, f)+"#code", "Code deployed. The next invocation runs it.", "")
}

// testFunction invokes the function with the event typed in the console and shows the
// result, the log and the duration.
func (s *Server) testFunction(w http.ResponseWriter, r *http.Request) {
	event := strings.TrimSpace(r.PostFormValue("event"))
	if event == "" {
		event = "{}"
	}
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error { return nil })
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !json.Valid([]byte(event)) {
		s.renderFunction(w, r, o, p, f, data{"TestEvent": event, "TestError": "The test event must be valid JSON.", "Tab": "test"})
		return
	}
	id, err := s.queueInvocation(r.Context(), o.ID, f.ID, "console", []byte(event), false)
	if errors.Is(err, errThrottled) {
		s.renderFunction(w, r, o, p, f, data{"TestEvent": event, "TestError": "Too many invocations are waiting for this function; try again in a moment.", "Tab": "test"})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	inv, err := s.awaitInvocation(r.Context(), o.ID, id, invokeWait(f))
	if err != nil && inv == nil {
		s.serverError(w, r, err)
		return
	}
	s.renderFunction(w, r, o, p, f, data{"TestEvent": event, "TestResult": inv, "Tab": "test"})
}

var reservedEnvPrefixes = []string{"PLATFORM_", "AWS_LAMBDA_", "LAMBDA_", "_HANDLER"}

func parseEnvText(text string) (map[string]string, error) {
	env := map[string]string{}
	size := 0
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !model.SecretNameRe.MatchString(k) {
			return nil, fmt.Errorf("%q: use NAME=value, names in uppercase letters, digits and underscores", line)
		}
		for _, p := range reservedEnvPrefixes {
			if strings.HasPrefix(k, p) {
				return nil, fmt.Errorf("%s is reserved by the platform", k)
			}
		}
		env[k] = v
		size += len(k) + len(v)
	}
	if size > 4096 {
		return nil, errors.New("environment variables are limited to 4 KB in total")
	}
	return env, nil
}

func (s *Server) functionSettings(w http.ResponseWriter, r *http.Request) {
	mem, tmo, conc := atoi(r.PostFormValue("memory_mb")), atoi(r.PostFormValue("timeout_seconds")), atoi(r.PostFormValue("concurrency"))
	handler := strings.TrimSpace(r.PostFormValue("handler"))
	urlOn, auth := r.PostFormValue("url_enabled") == "1", r.PostFormValue("url_auth")
	schedule, schedOn := strings.TrimSpace(r.PostFormValue("schedule")), r.PostFormValue("schedule_enabled") == "1"
	var formErr string
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		env, eerr := parseEnvText(r.PostFormValue("env"))
		switch {
		case mem < 128 || mem > 2048:
			formErr = "Memory: 128 to 2048 MB."
		case tmo < 1 || tmo > 300:
			formErr = "Timeout: 1 to 300 seconds."
		case conc < 1 || conc > 50:
			formErr = "Concurrency: 1 to 50."
		case !functions.ValidHandler(handler):
			formErr = "Handler: file and function, such as index.handler or lambda_function.lambda_handler."
		case auth != "none" && auth != "token":
			formErr = "Choose who may call the function URL."
		case eerr != nil:
			formErr = "Environment: " + eerr.Error()
		}
		if formErr == "" && schedule != "" {
			sch, err := cron.Parse(schedule)
			switch {
			case err != nil:
				formErr = "Schedule: " + err.Error()
			case sch.MinInterval(time.Now()) < time.Minute:
				formErr = "Schedule: at most once a minute."
			}
		}
		if formErr == "" && schedOn && schedule == "" {
			formErr = "Give a schedule (cron, UTC) to turn it on."
		}
		if formErr != "" {
			return nil
		}
		envJSON, _ := json.Marshal(env)
		if _, err := tx.Exec(r.Context(), `
			UPDATE functions SET memory_mb = $2, timeout_seconds = $3, concurrency = $4, handler = $5, env = $6, url_enabled = $7, url_auth = $8,
			       schedule = $9, schedule_enabled = $10, next_run_at = CASE WHEN schedule = $9 AND schedule_enabled = $10 THEN next_run_at END, updated_at = now()
			WHERE id = $1`, f.ID, mem, tmo, conc, handler, envJSON, urlOn, auth, schedule, schedOn); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "function.updated", "function:"+p.Slug+"/"+f.Slug, map[string]any{
			"memory_mb": mem, "timeout_seconds": tmo, "concurrency": conc, "url": urlOn, "url_auth": auth, "schedule": schedule, "schedule_enabled": schedOn})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'fn')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, fnPath(o, p, f)+"#configuration", "", formErr)
		return
	}
	back(w, r, fnPath(o, p, f)+"#configuration", "Configuration saved.", "")
}

// newFunctionToken creates the token callers send in X-Function-Token (shown once).
func (s *Server) newFunctionToken(w http.ResponseWriter, r *http.Request) {
	token := "fnt_" + secure.NewToken()
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		if _, err := tx.Exec(r.Context(), `UPDATE functions SET url_token_hash = $2, url_auth = 'token', updated_at = now() WHERE id = $1`, f.ID, secure.HashToken(token)); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "function.url_token_created", "function:"+p.Slug+"/"+f.Slug, nil)
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	f.URLAuth, f.HasToken = "token", true
	s.renderFunction(w, r, o, p, f, data{"NewToken": token, "Tab": "configuration"})
}

func (s *Server) deleteFunction(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, f, err, ok := s.withFunction(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, f *functionInfo) error {
		if r.PostFormValue("confirm") != f.Slug {
			formErr = "Type the function name exactly to confirm deletion."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM functions WHERE id = $1`, f.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "function.deleted", "function:"+p.Slug+"/"+f.Slug, nil)
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, fnPath(o, p, f)+"#configuration", "", formErr)
		return
	}
	back(w, r, o.Path()+"/functions", "Function "+f.Slug+" deleted.", "")
}

func itoaPtr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

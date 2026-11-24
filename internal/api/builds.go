package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/secure"
)

// Builds from git. The API stores the source settings and queues builds; the agent
// clones, builds in a sandbox, pushes and deploys (internal/agent/builds.go).

var (
	repoRe   = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	dirRe    = regexp.MustCompile(`^([A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*)?$`)
)

const maxActiveBuildsPerOrg = 5

func (s *Server) validRepo(raw string) error { return s.validRepoFor(raw, false) }

// validRepoFor checks a repository address. Builds on the organization's own servers
// may use private addresses: an on-premises git server is reached from those servers.
func (s *Server) validRepoFor(raw string, onServers bool) error {
	u, err := url.Parse(raw)
	if err != nil || !repoRe.MatchString(raw) || len(raw) > 300 {
		return errors.New("Repository: an https address such as https://github.com/acme/web.git (no credentials in it).")
	}
	if u.Scheme != "https" && !s.cfg.BuildAllowInsecure {
		return errors.New("Repository: the address must use https.")
	}
	if !s.cfg.BuildAllowInsecure && !onServers {
		h := u.Hostname()
		if ip, err := netip.ParseAddr(h); (err == nil && isPrivate(ip)) || strings.EqualFold(h, "localhost") || strings.HasSuffix(h, ".localhost") {
			return errors.New("Repository: private and internal addresses are not allowed.")
		}
	}
	return nil
}

func isPrivate(ip netip.Addr) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

type sourceInfo struct {
	Repo, Branch, Dir             string
	HasToken, HasHook, PRPreviews bool
	BuildOn                       string   // platform or servers
	BuildLabels                   []string // labels a build server must have
}

func (si sourceInfo) LabelText() string { return strings.Join(si.BuildLabels, ", ") }

func loadSource(r *http.Request, tx pgx.Tx, serviceID string) (sourceInfo, error) {
	var si sourceInfo
	err := tx.QueryRow(r.Context(), `
		SELECT source_repo, source_branch, source_dir, source_token_enc IS NOT NULL, build_hook_hash IS NOT NULL, pr_previews, build_on, build_labels
		FROM services WHERE id = $1`, serviceID).Scan(&si.Repo, &si.Branch, &si.Dir, &si.HasToken, &si.HasHook, &si.PRPreviews, &si.BuildOn, &si.BuildLabels)
	return si, err
}

type buildView struct {
	Number                  int
	Status, Trigger, Commit string
	Image, Error, Log, By   string
	Deploy                  bool
	Branch                  string
	PR                      *int
	Created                 time.Time
	Started, Finished       *time.Time
	Runner, Server          string // where it runs: platform, or servers (and which one)
}

func (b buildView) Duration() string {
	if b.Started == nil {
		return ""
	}
	end := time.Now()
	if b.Finished != nil {
		end = *b.Finished
	}
	return end.Sub(*b.Started).Round(time.Second).String()
}

func (b buildView) ShortCommit() string {
	if len(b.Commit) > 12 {
		return b.Commit[:12]
	}
	return b.Commit
}

const buildCols = `b.number, b.status, b.trigger, b.commit, b.image, b.error, b.log, coalesce(u.email, ''), b.deploy, b.branch, b.pr_number, b.created_at, b.started_at, b.finished_at, b.runner, coalesce((SELECT n.name FROM nodes n WHERE n.id = b.node_id), '')`

func loadBuilds(r *http.Request, tx pgx.Tx, serviceID string, limit int) ([]buildView, error) {
	rows, err := tx.Query(r.Context(), `SELECT `+buildCols+` FROM builds b LEFT JOIN users u ON u.id = b.created_by
		WHERE b.service_id = $1 ORDER BY b.number DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[buildView])
}

// queueBuild queues a build of the configured branch, or returns the one already
// waiting for this service. Errors meant for the user are apiErr values.
func (s *Server) queueBuild(r *http.Request, tx pgx.Tx, o *orgInfo, serviceID, trigger string, deploy bool, userID string) (int, bool, error) {
	if !s.cfg.BuildsEnabled || s.cfg.RegistryHost == "" {
		return 0, false, apiErr{http.StatusConflict, "Builds from git are not enabled on this platform."}
	}
	var repo string
	var allowed bool
	if err := tx.QueryRow(r.Context(), `
		SELECT s.source_repo, p.allow_builds FROM services s JOIN orgs o ON o.id = s.org_id JOIN plans p ON p.id = o.plan_id
		WHERE s.id = $1`, serviceID).Scan(&repo, &allowed); err != nil {
		return 0, false, err
	}
	if !allowed {
		return 0, false, apiErr{http.StatusForbidden, "Builds from git are not included in your plan (trial). Push an image instead, or upgrade."}
	}
	if repo == "" {
		return 0, false, apiErr{http.StatusConflict, "Set the git repository first (Builds tab)."}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT number FROM builds WHERE service_id = $1 AND status = 'queued'`, serviceID).Scan(&n); err == nil {
		return n, true, nil
	}
	var active int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM builds WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
		return 0, false, err
	}
	if active >= maxActiveBuildsPerOrg {
		return 0, false, apiErr{http.StatusTooManyRequests, "Too many builds waiting for this organization; try again when one finishes."}
	}
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO builds (org_id, service_id, number, trigger, deploy, created_by)
		VALUES ($1, $2, (SELECT coalesce(max(number), 0) + 1 FROM builds WHERE service_id = $2), $3, $4, nullif($5, '')::uuid)
		RETURNING number`, o.ID, serviceID, trigger, deploy, userID).Scan(&n); err != nil {
		return 0, false, err
	}
	s.audit(r, tx, o.ID, "build.queued", "service:"+serviceID, map[string]any{"build": n, "trigger": trigger})
	s.buildWake.signal() // a build server may take it
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'build')`); err != nil {
		return 0, false, err
	}
	return n, false, nil
}

// ---------- console ----------

// sourceInput is a service's git source and where its builds run.
type sourceInput struct {
	Repo, Branch, Dir, Token string
	ClearToken, PRPreviews   bool
	BuildOn                  string
	BuildLabels              []string
}

// check normalizes the input and returns what is wrong with it, or "".
func (s *Server) checkSource(in *sourceInput) string {
	in.Repo, in.Branch, in.Token = strings.TrimSpace(in.Repo), strings.TrimSpace(in.Branch), strings.TrimSpace(in.Token)
	in.Dir = strings.Trim(strings.TrimSpace(in.Dir), "/")
	if in.Branch == "" {
		in.Branch = "main"
	}
	if in.BuildOn == "" {
		in.BuildOn = "platform"
	}
	if in.BuildLabels == nil {
		in.BuildLabels = []string{}
	}
	switch {
	case in.BuildOn != "platform" && in.BuildOn != "servers":
		return "Build on: platform or servers."
	case in.Repo != "" && s.validRepoFor(in.Repo, in.BuildOn == "servers") != nil:
		return s.validRepoFor(in.Repo, in.BuildOn == "servers").Error()
	case !branchRe.MatchString(in.Branch) || strings.Contains(in.Branch, ".."):
		return "Branch: letters, digits, dots, dashes, underscores and slashes."
	case !dirRe.MatchString(in.Dir) || strings.Contains(in.Dir, ".."):
		return "Folder: a path inside the repository such as services/web, or empty for the root."
	case len(in.Token) > 500:
		return "The token is too long."
	}
	return ""
}

// storeSource saves a checked source; "" or why it cannot be saved.
func (s *Server) storeSource(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, in sourceInput) (string, error) {
	if sv.isPreview() {
		return "Previews are deployed from images; set the repository on the service.", nil
	}
	prPreviews := in.PRPreviews && in.Repo != ""
	if _, err := tx.Exec(r.Context(), `UPDATE services SET source_repo = $2, source_branch = $3, source_dir = $4, pr_previews = $5, build_on = $6, build_labels = $7 WHERE id = $1`,
		sv.ID, in.Repo, in.Branch, in.Dir, prPreviews, in.BuildOn, in.BuildLabels); err != nil {
		return "", err
	}
	// A build still waiting goes where the service now builds.
	if _, err := tx.Exec(r.Context(), `UPDATE builds SET runner = $2, runner_labels = $3 WHERE service_id = $1 AND status = 'queued'`,
		sv.ID, in.BuildOn, in.BuildLabels); err != nil {
		return "", err
	}
	s.buildWake.signal()
	switch {
	case in.Token != "":
		enc, err := s.box.Seal([]byte(in.Token), "source:"+sv.ID)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET source_token_enc = $2 WHERE id = $1`, sv.ID, enc); err != nil {
			return "", err
		}
	case in.ClearToken || in.Repo == "":
		if _, err := tx.Exec(r.Context(), `UPDATE services SET source_token_enc = NULL WHERE id = $1`, sv.ID); err != nil {
			return "", err
		}
	}
	s.audit(r, tx, o.ID, "service.source", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"repo": in.Repo, "branch": in.Branch, "dir": in.Dir, "token_set": in.Token != "", "pr_previews": prPreviews, "build_on": in.BuildOn, "build_labels": in.BuildLabels})
	return "", nil
}

func (s *Server) saveSource(w http.ResponseWriter, r *http.Request) {
	in := sourceInput{Repo: r.PostFormValue("repo"), Branch: r.PostFormValue("branch"), Dir: r.PostFormValue("dir"),
		Token: r.PostFormValue("token"), ClearToken: r.PostFormValue("clear_token") == "1", PRPreviews: r.PostFormValue("pr_previews") == "1",
		BuildOn: "platform"}
	if r.PostFormValue("build_on") == "servers" {
		in.BuildOn = "servers"
	}
	labels, labelErr := parseLabels(r.PostFormValue("build_labels"))
	in.BuildLabels = labels
	formErr := s.checkSource(&in)
	if labelErr != nil && formErr == "" {
		formErr = labelErr.Error()
	}
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if formErr != "" {
			return nil
		}
		var err error
		formErr, err = s.storeSource(r, tx, o, p, sv, in)
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
		back(w, r, svcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#builds", "Source saved.", "")
}

func (s *Server) startBuildForm(w http.ResponseWriter, r *http.Request) {
	var n int
	var existing bool
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		var err error
		n, existing, err = s.queueBuild(r, tx, o, sv.ID, "console", r.PostFormValue("deploy") == "1", sessionFrom(r).UserID)
		var ae apiErr
		if errors.As(err, &ae) {
			formErr = ae.msg
			return nil
		}
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
		back(w, r, svcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	msg := "Build #" + itoa(n) + " queued."
	if existing {
		msg = "Build #" + itoa(n) + " is already waiting; it will use the latest commit."
	}
	back(w, r, svcPath(o, p, sv)+"/builds/"+itoa(n), msg, "")
}

func (s *Server) buildPage(w http.ResponseWriter, r *http.Request) {
	var b buildView
	ahead := -1
	o, p, sv, err, ok := s.withService(w, r, "viewer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		var id string
		if err := tx.QueryRow(r.Context(), `SELECT b.id, `+buildCols+` FROM builds b LEFT JOIN users u ON u.id = b.created_by
			WHERE b.service_id = $1 AND b.number = $2`, sv.ID, atoi(r.PathValue("n"))).
			Scan(&id, &b.Number, &b.Status, &b.Trigger, &b.Commit, &b.Image, &b.Error, &b.Log, &b.By, &b.Deploy, &b.Branch, &b.PR, &b.Created, &b.Started, &b.Finished, &b.Runner, &b.Server); err != nil {
			return err
		}
		if b.Status == "queued" { // builds run one at a time: say how many are before this one
			return tx.QueryRow(r.Context(), `SELECT build_queue_ahead($1)`, id).Scan(&ahead)
		}
		return nil
	})
	if !ok {
		return
	}
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "build", http.StatusOK, data{"Org": o, "Project": p, "Service": sv, "Build": b, "Path": svcPath(o, p, sv),
		"Refresh": b.Status == "queued" || b.Status == "running", "Ahead": ahead})
}

// rotateBuildHook creates (or replaces) the webhook token; the URL is shown once.
func (s *Server) rotateBuildHook(w http.ResponseWriter, r *http.Request) {
	token := "bh_" + secure.NewToken()
	var formErr string
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if sv.isPreview() {
			formErr = "Previews have no webhook."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET build_hook_hash = $2 WHERE id = $1`, sv.ID, secure.HashToken(token)); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "build.hook_rotated", "service:"+p.Slug+"/"+sv.Slug, nil)
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
		back(w, r, svcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#builds", "New webhook address (shown once, copy it now): "+s.cfg.BaseURL+"/hooks/build/"+token, "")
}

// ---------- webhook ----------

// buildHook: POST /hooks/build/{token}, for GitHub, Gitea, Forgejo and GitLab push
// events (or any POST). Pushes to other branches and GitHub "ping" events are ignored.
func (s *Server) buildHook(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow("build-hook:"+ip, 60, time.Minute) {
		apiError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	token := r.PathValue("token")
	var orgID, serviceID, branch string
	var prPreviews bool
	err := s.pool.QueryRow(r.Context(), `SELECT org_id, service_id, branch, pr_previews FROM build_hook_service($1)`, secure.HashToken(token)).
		Scan(&orgID, &serviceID, &branch, &prPreviews)
	if err != nil {
		if !s.limiter.allow("build-hook-fail:"+ip, 20, 10*time.Minute) {
			apiError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		apiError(w, http.StatusNotFound, "unknown webhook")
		return
	}
	switch hookEvent(r) {
	case "ping":
		writeJSON(w, map[string]string{"status": "pong"})
		return
	case "pull_request":
		s.prHook(w, r, orgID, serviceID, prPreviews)
		return
	}
	var payload struct {
		Ref string `json:"ref"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload)
	if payload.Ref != "" && payload.Ref != "refs/heads/"+branch {
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]string{"status": "ignored", "reason": "push to another branch"})
		return
	}
	o := &orgInfo{ID: orgID}
	r = r.WithContext(context.WithValue(r.Context(), sessKey, &session{})) // audited as a system action
	var n int
	err = db.WithOrg(r.Context(), s.pool, orgID, func(tx pgx.Tx) error {
		var err error
		n, _, err = s.queueBuild(r, tx, o, serviceID, "webhook", true, "")
		return err
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		apiError(w, ae.status, ae.msg)
	case err != nil:
		s.log.Error("build hook", "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "queued", "build": n})
	}
}

// ---------- API v1 ----------

type apiBuild struct {
	Number   int        `json:"number"`
	Status   string     `json:"status"`
	Commit   string     `json:"commit,omitempty"`
	Image    string     `json:"image,omitempty"`
	Error    string     `json:"error,omitempty"`
	Log      string     `json:"log,omitempty"`
	Created  time.Time  `json:"created_at"`
	Finished *time.Time `json:"finished_at,omitempty"`
	Runner   string     `json:"runs_on"`          // platform or servers
	Server   string     `json:"server,omitempty"` // the build server that took it
}

// apiStartBuild: POST .../builds {"deploy": true}
func (s *Server) apiStartBuild(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	body := struct {
		Deploy *bool `json:"deploy"`
	}{}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
			apiError(w, http.StatusBadRequest, `body must be JSON, for example {"deploy": true}`)
			return
		}
	}
	deploy := body.Deploy == nil || *body.Deploy
	var n int
	var path string
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		var err error
		n, _, err = s.queueBuild(r, tx, o, sv.ID, "api", deploy, sessionFrom(r).UserID)
		path = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/builds/" + itoa(n)
		return err
	}) {
		return
	}
	w.Header().Set("Location", path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "status": "queued", "status_url": path})
}

func (s *Server) apiBuild(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var b buildView
	if !s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		return tx.QueryRow(r.Context(), `SELECT `+buildCols+` FROM builds b LEFT JOIN users u ON u.id = b.created_by
			WHERE b.service_id = $1 AND b.number = $2`, sv.ID, atoi(r.PathValue("n"))).
			Scan(&b.Number, &b.Status, &b.Trigger, &b.Commit, &b.Image, &b.Error, &b.Log, &b.By, &b.Deploy, &b.Branch, &b.PR, &b.Created, &b.Started, &b.Finished, &b.Runner, &b.Server)
	}) {
		return
	}
	writeJSON(w, apiBuild{b.Number, b.Status, b.Commit, b.Image, b.Error, b.Log, b.Created, b.Finished, b.Runner, b.Server})
}

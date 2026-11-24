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

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/secure"
)

// Builds from git. The API stores the source settings and queues builds; the agent
// clones, builds in a sandbox, pushes and deploys (internal/components/builds/agent/builds.go).

var (
	repoRe   = regexp.MustCompile(`^https?://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	dirRe    = regexp.MustCompile(`^([A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*)?$`)
)

func (s *API) validRepo(raw string) error { return s.validRepoFor(raw, false) }

// validRepoFor checks a repository address. Builds on the organization's own servers
// may use private addresses: an on-premises git server is reached from those servers.
func (s *API) validRepoFor(raw string, onServers bool) error {
	u, err := url.Parse(raw)
	if err != nil || !repoRe.MatchString(raw) || len(raw) > 300 {
		return errors.New("Repository: an https address such as https://github.com/acme/web.git (no credentials in it).")
	}
	if u.Scheme != "https" && !s.Cfg.BuildAllowInsecure {
		return errors.New("Repository: the address must use https.")
	}
	if !s.Cfg.BuildAllowInsecure && !onServers {
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

// queueBuild queues a build of the configured branch, or returns the one already
// waiting for this service. Errors meant for the user are apiErr values.
func (s *API) queueBuild(r *http.Request, tx pgx.Tx, o *web.OrgInfo, serviceID, trigger string, deploy bool, userID string) (int, bool, error) {
	if !s.Cfg.BuildsEnabled || s.Cfg.RegistryHost == "" {
		return 0, false, web.APIErr{Status: http.StatusConflict, Msg: "Builds from git are not enabled on this platform."}
	}
	var repo string
	var allowed bool
	if err := tx.QueryRow(r.Context(), `
		SELECT s.source_repo, p.allow_builds FROM services s JOIN orgs o ON o.id = s.org_id JOIN plans p ON p.id = o.plan_id
		WHERE s.id = $1`, serviceID).Scan(&repo, &allowed); err != nil {
		return 0, false, err
	}
	if !allowed {
		return 0, false, web.APIErr{Status: http.StatusForbidden, Msg: "Builds from git are not included in your plan (trial). Push an image instead, or upgrade."}
	}
	if repo == "" {
		return 0, false, web.APIErr{Status: http.StatusConflict, Msg: "Set the git repository first (Builds tab)."}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT number FROM builds WHERE service_id = $1 AND status = 'queued'`, serviceID).Scan(&n); err == nil {
		return n, true, nil
	}
	var active int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM builds WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
		return 0, false, err
	}
	if active >= web.MaxActiveBuildsPerOrg {
		return 0, false, web.APIErr{Status: http.StatusTooManyRequests, Msg: "Too many builds waiting for this organization; try again when one finishes."}
	}
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO builds (org_id, service_id, number, trigger, deploy, created_by)
		VALUES ($1, $2, (SELECT coalesce(max(number), 0) + 1 FROM builds WHERE service_id = $2), $3, $4, nullif($5, '')::uuid)
		RETURNING number`, o.ID, serviceID, trigger, deploy, userID).Scan(&n); err != nil {
		return 0, false, err
	}
	s.Audit(r, tx, o.ID, "build.queued", "service:"+serviceID, map[string]any{"build": n, "trigger": trigger})
	s.BuildWake.Signal() // a build server may take it
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
func (s *API) checkSource(in *sourceInput) string {
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
func (s *API) storeSource(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, in sourceInput) (string, error) {
	if sv.IsPreview() {
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
	s.BuildWake.Signal()
	switch {
	case in.Token != "":
		enc, err := s.Box.Seal([]byte(in.Token), "source:"+sv.ID)
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
	s.Audit(r, tx, o.ID, "service.source", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"repo": in.Repo, "branch": in.Branch, "dir": in.Dir, "token_set": in.Token != "", "pr_previews": prPreviews, "build_on": in.BuildOn, "build_labels": in.BuildLabels})
	return "", nil
}

func (s *API) saveSource(w http.ResponseWriter, r *http.Request) {
	in := sourceInput{Repo: r.PostFormValue("repo"), Branch: r.PostFormValue("branch"), Dir: r.PostFormValue("dir"),
		Token: r.PostFormValue("token"), ClearToken: r.PostFormValue("clear_token") == "1", PRPreviews: r.PostFormValue("pr_previews") == "1",
		BuildOn: "platform"}
	if r.PostFormValue("build_on") == "servers" {
		in.BuildOn = "servers"
	}
	labels, labelErr := web.ParseLabels(r.PostFormValue("build_labels"))
	in.BuildLabels = labels
	formErr := s.checkSource(&in)
	if labelErr != nil && formErr == "" {
		formErr = labelErr.Error()
	}
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
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
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#builds", "Source saved.", "")
}

func (s *API) startBuildForm(w http.ResponseWriter, r *http.Request) {
	var n int
	var existing bool
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		n, existing, err = s.queueBuild(r, tx, o, sv.ID, "console", r.PostFormValue("deploy") == "1", web.SessionFrom(r).UserID)
		var ae web.APIErr
		if errors.As(err, &ae) {
			formErr = ae.Msg
			return nil
		}
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
		web.Back(w, r, web.SvcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	msg := "Build #" + web.Itoa(n) + " queued."
	if existing {
		msg = "Build #" + web.Itoa(n) + " is already waiting; it will use the latest commit."
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"/builds/"+web.Itoa(n), msg, "")
}

func (s *API) buildPage(w http.ResponseWriter, r *http.Request) {
	var b web.BuildView
	ahead := -1
	o, p, sv, err, ok := s.WithService(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var id string
		if err := tx.QueryRow(r.Context(), `SELECT b.id, `+web.BuildCols+` FROM builds b LEFT JOIN users u ON u.id = b.created_by
			WHERE b.service_id = $1 AND b.number = $2`, sv.ID, web.Atoi(r.PathValue("n"))).
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
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "build", http.StatusOK, web.Data{"Org": o, "Project": p, "Service": sv, "Build": b, "Path": web.SvcPath(o, p, sv),
		"Refresh": b.Status == "queued" || b.Status == "running", "Ahead": ahead})
}

// rotateBuildHook creates (or replaces) the webhook token; the URL is shown once.
func (s *API) rotateBuildHook(w http.ResponseWriter, r *http.Request) {
	token := "bh_" + secure.NewToken()
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if sv.IsPreview() {
			formErr = "Previews have no webhook."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET build_hook_hash = $2 WHERE id = $1`, sv.ID, secure.HashToken(token)); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "build.hook_rotated", "service:"+p.Slug+"/"+sv.Slug, nil)
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#builds", "New webhook address (shown once, copy it now): "+s.Cfg.BaseURL+"/hooks/build/"+token, "")
}

// ---------- webhook ----------

// buildHook: POST /hooks/build/{token}, for GitHub, Gitea, Forgejo and GitLab push
// events (or any POST). Pushes to other branches and GitHub "ping" events are ignored.
func (s *API) buildHook(w http.ResponseWriter, r *http.Request) {
	ip := web.ClientIP(r)
	if !s.Limiter.Allow("build-hook:"+ip, 60, time.Minute) {
		web.APIError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	token := r.PathValue("token")
	var orgID, serviceID, branch string
	var prPreviews bool
	err := s.Pool.QueryRow(r.Context(), `SELECT org_id, service_id, branch, pr_previews FROM build_hook_service($1)`, secure.HashToken(token)).
		Scan(&orgID, &serviceID, &branch, &prPreviews)
	if err != nil {
		if !s.Limiter.Allow("build-hook-fail:"+ip, 20, 10*time.Minute) {
			web.APIError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		web.APIError(w, http.StatusNotFound, "unknown webhook")
		return
	}
	switch hookEvent(r) {
	case "ping":
		web.WriteJSON(w, map[string]string{"status": "pong"})
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
		web.WriteJSON(w, map[string]string{"status": "ignored", "reason": "push to another branch"})
		return
	}
	o := &web.OrgInfo{ID: orgID}
	r = r.WithContext(context.WithValue(r.Context(), web.SessKey, &web.Session{})) // audited as a system action
	var n int
	err = db.WithOrg(r.Context(), s.Pool, orgID, func(tx pgx.Tx) error {
		var err error
		n, _, err = s.queueBuild(r, tx, o, serviceID, "webhook", true, "")
		return err
	})
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.APIError(w, ae.Status, ae.Msg)
	case err != nil:
		s.Log.Error("build hook", "err", err)
		web.APIError(w, http.StatusInternalServerError, "internal error")
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
func (s *API) apiStartBuild(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	body := struct {
		Deploy *bool `json:"deploy"`
	}{}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil && err != io.EOF {
			web.APIError(w, http.StatusBadRequest, `body must be JSON, for example {"deploy": true}`)
			return
		}
	}
	deploy := body.Deploy == nil || *body.Deploy
	var n int
	var path string
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		n, _, err = s.queueBuild(r, tx, o, sv.ID, "api", deploy, web.SessionFrom(r).UserID)
		path = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/builds/" + web.Itoa(n)
		return err
	}) {
		return
	}
	w.Header().Set("Location", path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "status": "queued", "status_url": path})
}

func (s *API) apiBuild(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var b web.BuildView
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return tx.QueryRow(r.Context(), `SELECT `+web.BuildCols+` FROM builds b LEFT JOIN users u ON u.id = b.created_by
			WHERE b.service_id = $1 AND b.number = $2`, sv.ID, web.Atoi(r.PathValue("n"))).
			Scan(&b.Number, &b.Status, &b.Trigger, &b.Commit, &b.Image, &b.Error, &b.Log, &b.By, &b.Deploy, &b.Branch, &b.PR, &b.Created, &b.Started, &b.Finished, &b.Runner, &b.Server)
	}) {
		return
	}
	web.WriteJSON(w, apiBuild{b.Number, b.Status, b.Commit, b.Image, b.Error, b.Log, b.Created, b.Finished, b.Runner, b.Server})
}

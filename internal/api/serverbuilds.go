package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/nodeproto"
	"platform/internal/secure"
)

// Build servers (migration 069): an organization's own servers can run its builds, like
// self-hosted CI agents. A service set to build on "servers" queues builds that only the
// organization's servers with builds turned on, and every label the service asks for,
// can claim. The server gets what the build needs and nothing more: the source (and its
// read token), and a push token for that build's repository that works while it runs.
// It reports the log as it goes and the result at the end; the API then records the
// image and queues the deployment, as the platform's builder does.

var labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,29}$`)

// parseLabels reads "gpu, arm64 large" into a sorted, de-duplicated list.
func parseLabels(raw string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if !labelRe.MatchString(l) {
			return nil, errors.New("Labels: lowercase letters, digits, dots, dashes and underscores, up to 30 characters each.")
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) > 10 {
		return nil, errors.New("Labels: at most 10.")
	}
	sort.Strings(out)
	return out, nil
}

// serverBuilds is what a server does with builds.
type serverBuilds struct {
	Enabled   bool     `json:"enabled"`
	Labels    []string `json:"labels"`
	MaxBuilds int      `json:"max_builds"`
	Clean     bool     `json:"clean"`
}

// saveServerBuilds stores a server's build settings; the server's name, or no rows.
func (s *Server) saveServerBuilds(r *http.Request, tx pgx.Tx, o *orgInfo, id string, b serverBuilds) (string, error) {
	var name string
	if err := tx.QueryRow(r.Context(), `UPDATE nodes SET builds = $2, labels = $3, max_builds = $4, clean_builds = $5 WHERE id = $1 AND role = 'customer' RETURNING name`,
		id, b.Enabled, b.Labels, b.MaxBuilds, b.Clean).Scan(&name); err != nil {
		return "", err
	}
	s.audit(r, tx, o.ID, "server.builds", "server:"+name, map[string]any{"id": id, "builds": b.Enabled, "labels": b.Labels, "max_builds": b.MaxBuilds, "clean": b.Clean})
	s.buildWake.signal()
	return name, nil
}

const maxBuildsErr = "Builds at once: 1 to 8."

// serverBuildSettings turns builds on or off for a server and sets its labels, how many
// builds it runs at once and whether each starts clean (console).
func (s *Server) serverBuildSettings(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	id := r.PathValue("id")
	labels, lerr := parseLabels(r.PostFormValue("labels"))
	if lerr != nil {
		back(w, r, o.Path()+"/servers", "", lerr.Error())
		return
	}
	b := serverBuilds{Enabled: r.PostFormValue("builds") == "1", Labels: labels, MaxBuilds: 1, Clean: r.PostFormValue("clean") == "1"}
	if v := strings.TrimSpace(r.PostFormValue("max_builds")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 8 {
			back(w, r, o.Path()+"/servers", "", maxBuildsErr)
			return
		}
		b.MaxBuilds = n
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		_, err := s.saveServerBuilds(r, tx, o, id, b)
		return err
	})
	switch {
	case isNoRows(err):
		back(w, r, o.Path()+"/servers", "", "Unknown server.")
	case err != nil:
		s.serverError(w, r, err)
	case b.Enabled:
		back(w, r, o.Path()+"/servers", "Saved. This server now runs builds of services set to build on your servers.", "")
	default:
		back(w, r, o.Path()+"/servers", "Saved. This server no longer takes builds.", "")
	}
}

// ---------- node API ----------

// buildWake wakes the build servers' held claim requests when a build may be waiting.
type buildWake struct {
	mu sync.Mutex
	ch chan struct{}
}

func newBuildWake() *buildWake { return &buildWake{ch: make(chan struct{})} }

func (b *buildWake) wait() <-chan struct{} {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ch
}

// signal wakes the waiting claims shortly, once the caller's transaction committed.
func (b *buildWake) signal() {
	if b == nil {
		return
	}
	time.AfterFunc(300*time.Millisecond, func() {
		b.mu.Lock()
		close(b.ch)
		b.ch = make(chan struct{})
		b.mu.Unlock()
	})
}

// nodeBuildClaim hands the oldest waiting build this server may run, or 204. The agent
// says which builds it is running (a build marked running here but not in its list
// was interrupted) and how long it can wait: the request is held until a build comes
// or the time is up (long polling), so a build starts within a second or so.
func (s *Server) nodeBuildClaim(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	if n.Suspended || !s.cfg.BuildsEnabled || s.cfg.RegistryHost == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req nodeproto.BuildClaim
	_ = readJSON(r, 1<<16, &req) // older agents send {}
	wait := time.Duration(min(max(req.WaitSec, 0), 25)) * time.Second
	deadline := time.Now().Add(wait)
	for {
		woken := s.buildWake.wait()
		job, more, err := s.claimBuild(r.Context(), n, req.Running)
		if err != nil {
			s.log.Error("claim build", "node", n.ID, "err", err)
			apiError(w, http.StatusInternalServerError, "could not claim a build")
			return
		}
		if job != nil {
			writeJSON(w, job)
			return
		}
		left := time.Until(deadline)
		if !more || left <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t := time.NewTimer(min(left, 3*time.Second))
		select {
		case <-r.Context().Done():
			t.Stop()
			return
		case <-woken:
			t.Stop()
		case <-t.C:
		}
	}
}

// claimBuild claims a build for the server; more is false when it cannot take one now
// (builds turned off, or as many running as it may run), so the request is not held.
func (s *Server) claimBuild(ctx context.Context, n *nodeInfo, running []string) (job *nodeproto.BuildJob, more bool, err error) {
	if running == nil {
		running = []string{}
	}
	err = db.WithOrg(ctx, s.pool, n.OrgID, func(tx pgx.Tx) error {
		var enabled, clean bool
		var labels []string
		var maxBuilds int
		// The operating system and architecture its agent reports count as labels.
		if err := tx.QueryRow(ctx, `
			SELECT builds, labels || array_remove(ARRAY[nullif(lower(os), ''), nullif(lower(arch), '')], NULL), max_builds, clean_builds
			FROM nodes WHERE id = $1`, n.ID).Scan(&enabled, &labels, &maxBuilds, &clean); err != nil || !enabled {
			return err
		}
		// A build still marked running here that the agent is not running was
		// interrupted (the agent restarted).
		var busy int
		if err := tx.QueryRow(ctx, `
			WITH failed AS (
				UPDATE builds SET status = 'failed', error = 'interrupted: the build server restarted; start the build again',
				       finished_at = now(), push_token_hash = NULL
				WHERE node_id = $1 AND status = 'running' AND NOT (id::text = ANY($2::text[]))
				RETURNING id)
			SELECT count(*) FROM builds WHERE node_id = $1 AND status = 'running' AND id NOT IN (SELECT id FROM failed)`, n.ID, running).Scan(&busy); err != nil {
			return err
		}
		if busy >= maxBuilds {
			return nil
		}
		more = true
		var j nodeproto.BuildJob
		var tokenEnc []byte
		var orgSlug, svcSlug string
		err := tx.QueryRow(ctx, `
			SELECT b.id, b.number, s.id, s.slug, o.slug, s.source_repo, coalesce(nullif(b.branch, ''), s.source_branch), s.source_dir,
			       s.source_token_enc, b.source_zip IS NOT NULL, b.source_name, b.source_sha256
			FROM builds b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = b.org_id
			WHERE b.status = 'queued' AND b.runner = 'servers' AND b.runner_labels <@ $1::text[]
			ORDER BY b.created_at LIMIT 1 FOR UPDATE OF b SKIP LOCKED`, labels).
			Scan(&j.ID, &j.Number, &j.ServiceID, &svcSlug, &orgSlug, &j.Repo, &j.Branch, &j.Dir, &tokenEnc, &j.Zip, &j.SourceName, &j.SourceSHA)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if j.Zip {
			j.Repo, j.Branch, j.Dir = "", "", ""
		} else if len(tokenEnc) > 0 {
			plain, err := s.box.Open(tokenEnc, "source:"+j.ServiceID)
			if err != nil {
				_, err = tx.Exec(ctx, `UPDATE builds SET status = 'failed', error = 'cannot read the repository token', finished_at = now() WHERE id = $1`, j.ID)
				return err
			}
			j.Token = string(plain)
		}
		push := "plb_" + randomAlnum(40)
		if _, err := tx.Exec(ctx, `
			UPDATE builds SET status = 'running', started_at = now(), heartbeat_at = now(), node_id = $2, push_token_hash = $3
			WHERE id = $1`, j.ID, n.ID, secure.HashToken(push)); err != nil {
			return err
		}
		j.ImageRepo = s.cfg.RegistryHost + "/" + orgSlug + "/" + svcSlug
		j.PushToken = push
		j.BuilderImage, j.BuildkitImage, j.RailpackFrontend = s.cfg.GitImage, s.cfg.BuildkitImage, s.cfg.RailpackFrontend
		j.TimeoutSec, j.MaxImageMB = int(s.cfg.BuildTimeout.Seconds()), s.cfg.BuildMaxImageMB
		j.Clean = clean
		job = &j
		return db.Audit(ctx, tx, n.OrgID, "", "build.claimed", "service:"+j.ServiceID, map[string]any{"build": j.Number, "server": n.ID}, "")
	})
	return job, more, err
}

// runningBuild locks a build this server is running.
func runningBuild(ctx context.Context, tx pgx.Tx, id, nodeID string) (serviceID string, number int, ok bool, err error) {
	if !uuidRe.MatchString(id) {
		return "", 0, false, nil
	}
	err = tx.QueryRow(ctx, `SELECT service_id, number FROM builds WHERE id = $1 AND node_id = $2 AND status = 'running' FOR UPDATE`, id, nodeID).
		Scan(&serviceID, &number)
	if isNoRows(err) {
		return "", 0, false, nil
	}
	return serviceID, number, err == nil, err
}

func (s *Server) nodeBuildLog(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	var rep nodeproto.BuildLogReport
	if err := readJSON(r, 1<<20, &rep); err != nil {
		apiError(w, http.StatusBadRequest, "bad log report")
		return
	}
	var ok bool
	err := db.WithOrg(r.Context(), s.pool, n.OrgID, func(tx pgx.Tx) error {
		var err error
		if _, _, ok, err = runningBuild(r.Context(), tx, r.PathValue("id"), n.ID); !ok || err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), `UPDATE builds SET log = $2, heartbeat_at = now() WHERE id = $1`, r.PathValue("id"), trimBuildLog(rep.Log))
		return err
	})
	switch {
	case err != nil:
		s.log.Error("build log", "node", n.ID, "err", err)
		apiError(w, http.StatusInternalServerError, "could not store the log")
	case !ok:
		apiError(w, http.StatusConflict, "this build is no longer running on this server")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// nodeBuildSecrets gives the server running a build the service's secrets its pipeline
// steps name. Audited: which secrets, never their values.
func (s *Server) nodeBuildSecrets(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	var req nodeproto.BuildSecretsRequest
	if err := readJSON(r, 1<<16, &req); err != nil || len(req.Names) > 50 {
		apiError(w, http.StatusBadRequest, "bad secrets request")
		return
	}
	out := map[string]string{}
	var ok bool
	err := db.WithOrg(r.Context(), s.pool, n.OrgID, func(tx pgx.Tx) error {
		serviceID, number, running, err := runningBuild(r.Context(), tx, r.PathValue("id"), n.ID)
		if !running || err != nil {
			return err
		}
		ok = true
		rows, err := tx.Query(r.Context(), `SELECT name, value_enc FROM secrets WHERE service_id = $1 AND name = ANY($2)`, serviceID, req.Names)
		if err != nil {
			return err
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name string
			var enc []byte
			if err := rows.Scan(&name, &enc); err != nil {
				return err
			}
			plain, err := s.box.Open(enc, "secret:"+serviceID+":"+name)
			if err != nil {
				return err
			}
			out[name] = string(plain)
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		return db.Audit(r.Context(), tx, n.OrgID, "", "build.secrets", "service:"+serviceID, map[string]any{"build": number, "server": n.ID, "secrets": names}, "")
	})
	switch {
	case err != nil:
		s.log.Error("build secrets", "node", n.ID, "err", err)
		apiError(w, http.StatusInternalServerError, "could not read the secrets")
	case !ok:
		apiError(w, http.StatusConflict, "this build is no longer running on this server")
	default:
		writeJSON(w, out)
	}
}

// nodeBuildSource serves the uploaded archive of a zip build to the server running it.
func (s *Server) nodeBuildSource(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	var zip []byte
	var ok bool
	err := db.WithOrg(r.Context(), s.pool, n.OrgID, func(tx pgx.Tx) error {
		var err error
		if _, _, ok, err = runningBuild(r.Context(), tx, r.PathValue("id"), n.ID); !ok || err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT source_zip FROM builds WHERE id = $1`, r.PathValue("id")).Scan(&zip)
	})
	switch {
	case err != nil:
		s.log.Error("build source", "node", n.ID, "err", err)
		apiError(w, http.StatusInternalServerError, "could not read the archive")
	case !ok || zip == nil:
		apiError(w, http.StatusNotFound, "no archive for this build")
	default:
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zip)
	}
}

var builtTagRe = regexp.MustCompile(`^(git|zip)-[0-9a-f]{12}$`)

func (s *Server) nodeBuildFinish(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	var res nodeproto.BuildResult
	if err := readJSON(r, 1<<20, &res); err != nil || (res.Status != "done" && res.Status != "failed") {
		apiError(w, http.StatusBadRequest, "bad build result")
		return
	}
	id := r.PathValue("id")
	var ok bool
	var deployErr error
	err := db.WithOrg(r.Context(), s.pool, n.OrgID, func(tx pgx.Tx) error {
		serviceID, number, found, err := runningBuild(r.Context(), tx, id, n.ID)
		if !found || err != nil {
			return err
		}
		ok = true
		var orgSlug, svcSlug, zipSHA string
		var deploy bool
		var previewID *string
		var pr *int
		var createdBy *string
		if err := tx.QueryRow(r.Context(), `
			SELECT o.slug, s.slug, b.deploy, b.preview_service_id, b.pr_number, b.created_by, b.source_sha256
			FROM builds b JOIN services s ON s.id = b.service_id JOIN orgs o ON o.id = b.org_id WHERE b.id = $1`, id).
			Scan(&orgSlug, &svcSlug, &deploy, &previewID, &pr, &createdBy, &zipSHA); err != nil {
			return err
		}
		log := res.Log
		fail := func(msg string) error {
			if _, err := tx.Exec(r.Context(), `
				UPDATE builds SET status = 'failed', error = $2, log = $3, finished_at = now(), push_token_hash = NULL, source_zip = NULL
				WHERE id = $1`, id, truncateStr(msg, 2000), trimBuildLog(log)); err != nil {
				return err
			}
			return db.Audit(r.Context(), tx, n.OrgID, "", "build.failed", "service:"+serviceID,
				map[string]any{"build": number, "error": truncateStr(msg, 300), "server": n.ID}, "")
		}
		if res.Status == "failed" {
			msg := strings.TrimSpace(res.Error)
			if msg == "" {
				msg = "the build failed on your server (see the log)"
			}
			return fail(msg)
		}
		// The image must be the one this build was allowed to push.
		repo := s.cfg.RegistryHost + "/" + orgSlug + "/" + svcSlug
		tag, found := strings.CutPrefix(res.Image, repo+":")
		if !found || !builtTagRe.MatchString(tag) || !strings.HasPrefix(res.Commit, tag[4:]) {
			return fail("the build server reported an unexpected image")
		}
		if zipSHA != "" && res.Commit != zipSHA {
			return fail("the build server reported an unexpected archive checksum")
		}
		switch {
		case pr != nil && previewID == nil:
			log += "==> the preview was deleted meanwhile (pull request closed): nothing to deploy\n"
		case deploy && res.NoDeploy:
			log += "==> staysa.yml says deploy: false: the image is pushed, not deployed\n"
		case deploy:
			target := serviceID
			if previewID != nil {
				target = *previewID
			}
			if deployErr = queueBuiltDeployment(r.Context(), tx, n.OrgID, target, res.Image, createdBy); deployErr != nil {
				return fail("deploy: " + deployErr.Error())
			}
			log += "==> deployment started\n"
		}
		if _, err := tx.Exec(r.Context(), `
			UPDATE builds SET status = 'done', commit = $2, image = $3, log = $4, finished_at = now(), push_token_hash = NULL, source_zip = NULL
			WHERE id = $1`, id, res.Commit, res.Image, trimBuildLog(log)); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`); err != nil {
			return err
		}
		return db.Audit(r.Context(), tx, n.OrgID, "", "build.done", "service:"+serviceID,
			map[string]any{"build": number, "image": res.Image, "server": n.ID}, "")
	})
	switch {
	case err != nil:
		s.log.Error("finish build", "node", n.ID, "err", err)
		apiError(w, http.StatusInternalServerError, "could not record the result")
	case !ok:
		apiError(w, http.StatusConflict, "this build is no longer running on this server")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// queueBuiltDeployment points a service at a built image and queues a deployment with
// its current settings (the API's copy of the agent's deployBuilt).
func queueBuiltDeployment(ctx context.Context, tx pgx.Tx, orgID, serviceID, image string, createdBy *string) error {
	var spec model.Spec
	var envJSON []byte
	if err := tx.QueryRow(ctx, `
		UPDATE services SET image = $2 WHERE id = $1
		RETURNING port, cpu_millis, memory_mb, health_path, volume_path, env, worker, instances, ports`, serviceID, image).
		Scan(&spec.Port, &spec.CPUMillis, &spec.MemoryMB, &spec.HealthPath, &spec.VolumePath, &envJSON, &spec.Worker, &spec.Instances, &spec.Ports); err != nil {
		if isNoRows(err) {
			return errors.New("the service was deleted while building")
		}
		return err
	}
	_ = json.Unmarshal(envJSON, &spec.Env)
	spec.Image = image
	if err := spec.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(spec)
	_, err := tx.Exec(ctx, `
		INSERT INTO deployments (org_id, service_id, revision, spec, created_by)
		VALUES ($1, $2, (SELECT coalesce(max(revision), 0) + 1 FROM deployments WHERE service_id = $2), $3, $4)`,
		orgID, serviceID, raw, createdBy)
	return err
}

func trimBuildLog(s string) string {
	if len(s) > 200_000 {
		s = "… (earlier output trimmed)\n" + s[len(s)-200_000:]
	}
	return s
}

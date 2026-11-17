package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// A preview for each pull request: the build webhook also accepts pull request events
// (GitHub, Gitea and Forgejo use the same payload). Opening, reopening or pushing to a
// pull request builds its branch and deploys it as preview "pr-<number>"; closing or
// merging it deletes the preview. Pull requests from forks are ignored, because a
// preview receives the service's secrets.

const prPreviewHours = 14 * 24 // renewed at every push to the pull request

type prEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
}

// hookEvent names the event of a webhook request ("push", "pull_request", "ping", or "").
func hookEvent(r *http.Request) string {
	for _, h := range []string{"X-GitHub-Event", "X-Gitea-Event", "X-Forgejo-Event", "X-Gogs-Event"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return ""
}

func prPreviewName(n int) string { return fmt.Sprintf("pr-%d", n) }

// loadServiceByID loads a service the way loadService does, by ID (webhooks have no path).
func (s *Server) loadServiceByID(r *http.Request, tx pgx.Tx, id string) (*projectInfo, *serviceInfo, error) {
	var p projectInfo
	var sv serviceInfo
	err := tx.QueryRow(r.Context(), `
		SELECT p.id, p.slug, p.name, p.status_public,
		       s.id, s.slug, s.image, s.health_path, s.volume_path, s.port, s.cpu_millis, s.memory_mb, s.env, s.worker, s.instances, s.ports
		FROM services s JOIN projects p ON p.id = s.project_id WHERE s.id = $1`, id).
		Scan(&p.ID, &p.Slug, &p.Name, &p.StatusPublic,
			&sv.ID, &sv.Slug, &sv.Image, &sv.HealthPath, &sv.VolumePath, &sv.Port, &sv.CPUMillis, &sv.MemoryMB, &sv.Env, &sv.Worker, &sv.Instances, &sv.Ports)
	if sv.Env == nil {
		sv.Env = map[string]string{}
	}
	return &p, &sv, err
}

// prHook handles a pull request event for a service with pull request previews on.
func (s *Server) prHook(w http.ResponseWriter, r *http.Request, orgID, serviceID string, enabled bool) {
	if !enabled {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ignored", "reason": "pull request previews are off for this service"})
		return
	}
	var ev prEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&ev); err != nil || ev.Number < 1 {
		apiError(w, http.StatusBadRequest, "not a pull request event")
		return
	}
	ignore := func(reason string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ignored", "reason": reason})
	}
	head, base := ev.PullRequest.Head.Repo.FullName, ev.PullRequest.Base.Repo.FullName
	if head == "" || !strings.EqualFold(head, base) {
		ignore("pull requests from forks are not previewed: a preview receives the service's secrets")
		return
	}
	var deleting bool
	switch ev.Action {
	case "opened", "reopened", "synchronize", "synchronized", "ready_for_review":
	case "closed":
		deleting = true
	default:
		ignore("action " + ev.Action + " needs no preview change")
		return
	}
	branch := ev.PullRequest.Head.Ref
	if !deleting && (!branchRe.MatchString(branch) || strings.Contains(branch, "..")) {
		ignore("the pull request's branch name cannot be built")
		return
	}

	name := prPreviewName(ev.Number)
	var o orgInfo
	r = r.WithContext(context.WithValue(r.Context(), sessKey, &session{})) // audited as a system action
	result := map[string]any{"preview": name}
	err := db.WithOrg(r.Context(), s.pool, orgID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `SELECT id, slug, name, plan_id FROM orgs WHERE id = $1`, orgID).Scan(&o.ID, &o.Slug, &o.Name, &o.PlanID); err != nil {
			return err
		}
		p, parent, err := s.loadServiceByID(r, tx, serviceID)
		if err != nil {
			return err
		}
		if deleting {
			err := s.deletePreviewTx(r, tx, &o, p, parent, name)
			if isNoRows(err) {
				result["status"] = "no preview to delete"
				return nil
			}
			result["status"] = "deleted"
			return err
		}
		res, err := s.upsertPreview(r, tx, &o, p, parent, previewRequest{Name: name, Hours: prPreviewHours, deferred: true})
		if err != nil {
			return err
		}
		var previewID string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM services WHERE preview_of = $1 AND preview_name = $2`, parent.ID, name).Scan(&previewID); err != nil {
			return err
		}
		n, err := s.queuePRBuild(r, tx, &o, parent.ID, previewID, ev.Number, branch)
		if err != nil {
			return err
		}
		result["status"], result["build"], result["url"] = "queued", n, res.URL
		return nil
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		apiError(w, ae.status, ae.msg)
	case err != nil:
		s.log.Error("pull request hook", "err", err)
		apiError(w, http.StatusInternalServerError, "internal error")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(result)
	}
}

// queuePRBuild queues a build of a pull request's branch for its preview, or updates the
// one already waiting for that preview.
func (s *Server) queuePRBuild(r *http.Request, tx pgx.Tx, o *orgInfo, serviceID, previewID string, pr int, branch string) (int, error) {
	if !s.cfg.BuildsEnabled || s.cfg.RegistryHost == "" {
		return 0, apiErr{http.StatusConflict, "Builds from git are not enabled on this platform."}
	}
	var allowed bool
	if err := tx.QueryRow(r.Context(), `SELECT p.allow_builds FROM orgs o JOIN plans p ON p.id = o.plan_id WHERE o.id = $1`, o.ID).Scan(&allowed); err != nil {
		return 0, err
	}
	if !allowed {
		return 0, apiErr{http.StatusForbidden, "Builds from git are not included in this organization's plan."}
	}
	var n int
	err := tx.QueryRow(r.Context(), `UPDATE builds SET branch = $2 WHERE preview_service_id = $1 AND status = 'queued' RETURNING number`, previewID, branch).Scan(&n)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	var active int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM builds WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
		return 0, err
	}
	if active >= maxActiveBuildsPerOrg {
		return 0, apiErr{http.StatusTooManyRequests, "Too many builds waiting for this organization; try again when one finishes."}
	}
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO builds (org_id, service_id, number, trigger, deploy, branch, preview_service_id, pr_number)
		VALUES ($1, $2, (SELECT coalesce(max(number), 0) + 1 FROM builds WHERE service_id = $2), 'pull_request', true, $3, $4, $5)
		RETURNING number`, o.ID, serviceID, branch, previewID, pr).Scan(&n); err != nil {
		return 0, err
	}
	s.audit(r, tx, o.ID, "build.queued", "service:"+serviceID, map[string]any{"build": n, "trigger": "pull_request", "pull_request": pr, "branch": branch})
	_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'build')`)
	return n, err
}

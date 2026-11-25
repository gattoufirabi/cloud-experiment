package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/components/servers/nodeproto"
	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

const maxServersPerOrg = 20

type joinTokenView struct {
	Name    string
	Expires time.Time
}

// ---------- console ----------

func (s *API) serversPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	s.renderServers(w, r, o, http.StatusOK, web.Data{})
}

func (s *API) renderServers(w http.ResponseWriter, r *http.Request, o *web.OrgInfo, status int, d web.Data) {
	var servers []web.ServerView
	var pending []joinTokenView
	var projects []projectPlacement
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT n.id, n.name, n.os, n.arch, n.docker_version, n.agent_version, n.cpus, n.mem_mb, n.mem_avail_mb, n.disk_free_mb,
			       n.load1::float8, `+web.ServerOnlineSQL+`, n.last_seen, n.created_at,
			       coalesce((SELECT array_agg(p.slug ORDER BY p.slug) FROM projects p WHERE p.node_id = n.id), '{}'),
			       n.builds, n.labels, coalesce((SELECT array_agg(b.number ORDER BY b.number) FROM builds b WHERE b.node_id = n.id AND b.status = 'running'), '{}'),
			       n.max_builds, n.clean_builds
			FROM nodes n WHERE n.role = 'customer' ORDER BY n.name, n.id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v web.ServerView
			if err := rows.Scan(&v.ID, &v.Name, &v.OS, &v.Arch, &v.DockerVersion, &v.AgentVersion, &v.CPUs, &v.MemMB, &v.MemAvailMB,
				&v.DiskFreeMB, &v.Load1, &v.Online, &v.LastSeen, &v.Created, &v.Projects, &v.Builds, &v.Labels, &v.Building, &v.MaxBuilds, &v.Clean); err != nil {
				rows.Close()
				return err
			}
			v.Connected = s.Tunnels.IdleCount(v.ID) > 0
			servers = append(servers, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `SELECT name, expires_at FROM server_join_tokens WHERE used_at IS NULL AND expires_at > now() ORDER BY created_at`)
		if err != nil {
			return err
		}
		if pending, err = pgx.CollectRows(rows, pgx.RowToStructByPos[joinTokenView]); err != nil {
			return err
		}
		projects, err = loadPlacements(r.Context(), tx)
		return err
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	d["Org"] = o
	d["Servers"] = servers
	d["PendingJoins"] = pending
	d["Placements"] = projects
	d["CanAdmin"] = model.RoleAtLeast(o.Role, "admin")
	d["BaseURL"] = s.Cfg.BaseURL
	s.Render(w, r, "servers", status, d)
}

type projectPlacement struct {
	Slug, Name, NodeID string
}

func loadPlacements(ctx context.Context, tx pgx.Tx) ([]projectPlacement, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.slug, p.name, CASE WHEN n.id IS NULL THEN '' ELSE n.id END
		FROM projects p LEFT JOIN nodes n ON n.id = p.node_id AND n.role = 'customer' ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[projectPlacement])
}

// createServerJoin creates a one-time join token and shows the install command once.
func (s *API) createServerJoin(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || len(name) > 60 {
		web.Back(w, r, o.Path()+"/servers", "", "Give the server a name of up to 60 characters (for example: aws-eu-1).")
		return
	}
	token := nodeproto.JoinPrefix + secure.NewToken()
	var tooMany bool
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM nodes WHERE role = 'customer')
			+ (SELECT count(*) FROM server_join_tokens WHERE used_at IS NULL AND expires_at > now())`).Scan(&n); err != nil {
			return err
		}
		if n >= maxServersPerOrg {
			tooMany = true
			return nil
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO server_join_tokens (org_id, name, token_hash, created_by, expires_at)
			VALUES ($1, $2, $3, nullif($4, '')::uuid, now() + interval '1 hour')`,
			o.ID, name, secure.HashToken(token), web.SessionFrom(r).UserID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "server.join_created", "server:"+name, nil)
		return nil
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if tooMany {
		web.Back(w, r, o.Path()+"/servers", "", "An organization can attach up to "+web.Itoa(maxServersPerOrg)+" servers.")
		return
	}
	s.renderServers(w, r, o, http.StatusOK, web.Data{"JoinToken": token, "JoinName": name})
}

func (s *API) deleteServer(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	id := r.PathValue("id")
	var inUse bool
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var name string
		var regToken *string
		if err := tx.QueryRow(r.Context(), `SELECT name, registry_token_id::text FROM nodes WHERE id = $1 AND role = 'customer'`, id).Scan(&name, &regToken); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM projects WHERE node_id = $1)`, id).Scan(&inUse); err != nil || inUse {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			UPDATE builds SET status = 'failed', error = 'the build server was removed', finished_at = now(), push_token_hash = NULL, source_zip = NULL
			WHERE node_id = $1 AND status = 'running'`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM nodes WHERE id = $1`, id); err != nil {
			return err
		}
		if regToken != nil {
			if _, err := tx.Exec(r.Context(), `DELETE FROM registry_tokens WHERE id = $1`, *regToken); err != nil {
				return err
			}
		}
		s.Audit(r, tx, o.ID, "server.removed", "server:"+name, map[string]string{"id": id})
		return nil
	})
	switch {
	case web.IsNoRows(err):
		web.Back(w, r, o.Path()+"/servers", "", "Unknown server.")
		return
	case err != nil:
		s.ServerError(w, r, err)
		return
	case inUse:
		web.Back(w, r, o.Path()+"/servers", "", "Move this server's projects back to the platform or to another server first.")
		return
	}
	s.Tunnels.DropNode(id)
	web.Back(w, r, o.Path()+"/servers", "Server removed. Its token stopped working; you can now uninstall the agent from the machine.", "")
}

// placeProject moves a project onto one of the organization's servers, or back onto the
// platform (empty server). Its running applications are deployed again where it now runs.
func (s *API) placeProject(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	slug := r.PostFormValue("project")
	target := r.PostFormValue("server")
	var formErr, msg string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var projectID, current string
		if err := tx.QueryRow(r.Context(), `SELECT id, coalesce(node_id, '') FROM projects WHERE slug = $1`, slug).Scan(&projectID, &current); err != nil {
			if web.IsNoRows(err) {
				formErr = "Unknown project."
				return nil
			}
			return err
		}
		var currentIsServer bool
		if current != "" {
			if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM nodes WHERE id = $1 AND role = 'customer')`, current).Scan(&currentIsServer); err != nil {
				return err
			}
		}
		if (target == "" && !currentIsServer) || target == current {
			formErr = "The project already runs there."
			return nil
		}
		var serverName string
		if target != "" {
			if err := tx.QueryRow(r.Context(), `SELECT name FROM nodes WHERE id = $1 AND role = 'customer'`, target).Scan(&serverName); err != nil {
				if web.IsNoRows(err) {
					formErr = "Unknown server."
					return nil
				}
				return err
			}
			if why, err := ownServerBlocker(r.Context(), tx, projectID); err != nil || why != "" {
				formErr = why
				return err
			}
			if _, err := tx.Exec(r.Context(), `UPDATE projects SET node_id = $2 WHERE id = $1`, projectID, target); err != nil {
				return err
			}
		} else {
			// Back to the platform: the scheduler places it on a platform server.
			if _, err := tx.Exec(r.Context(), `UPDATE projects SET node_id = NULL WHERE id = $1`, projectID); err != nil {
				return err
			}
		}
		// Deploy every running application again where the project now runs.
		tag, err := tx.Exec(r.Context(), `
			INSERT INTO deployments (org_id, service_id, revision, spec, created_by)
			SELECT d.org_id, d.service_id, (SELECT max(revision) + 1 FROM deployments x WHERE x.service_id = d.service_id), d.spec, nullif($2, '')::uuid
			FROM deployments d JOIN services sv ON sv.id = d.service_id
			WHERE sv.project_id = $1 AND d.status = 'live'`, projectID, web.SessionFrom(r).UserID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`); err != nil {
			return err
		}
		where := "the platform"
		if target != "" {
			where = "server " + serverName
		}
		s.Audit(r, tx, o.ID, "project.placed", "project:"+slug, map[string]any{"server": target, "redeployed": tag.RowsAffected()})
		msg = "Project " + slug + " now runs on " + where + "."
		if n := tag.RowsAffected(); n > 0 {
			msg += " " + web.Itoa(int(n)) + " application(s) are being deployed there."
		}
		return nil
	})
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, o.Path()+"/servers#placement", "", formErr)
		return
	}
	web.Back(w, r, o.Path()+"/servers#placement", msg, "")
}

// ownServerBlocker says why a project cannot run on a customer server ("" = it can).
// Applications and workers run there; managed data services and platform-run compute
// stay on the platform.
func ownServerBlocker(ctx context.Context, tx pgx.Tx, projectID string) (string, error) {
	var dbs, fns, jobs, ports int
	err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM databases WHERE project_id = $1),
		       (SELECT count(*) FROM functions WHERE project_id = $1),
		       (SELECT count(*) FROM jobs j JOIN services sv ON sv.id = j.service_id WHERE sv.project_id = $1),
		       (SELECT count(*) FROM services WHERE project_id = $1 AND ports <> '[]'::jsonb)`, projectID).Scan(&dbs, &fns, &jobs, &ports)
	switch {
	case err != nil:
		return "", err
	case dbs > 0:
		return "This project has managed databases, which run on the platform only. Use a separate project for the applications that run on your server.", nil
	case fns > 0:
		return "This project has functions, which run on the platform only.", nil
	case jobs > 0:
		return "This project has scheduled jobs, which run on the platform only.", nil
	case ports > 0:
		return "An application of this project has public TCP/UDP ports, which are not available on your own servers yet.", nil
	}
	return "", nil
}

// ---------- node API ----------

type nodeInfo struct {
	ID, OrgID, OrgSlug string
	Suspended          bool
}

func (s *API) nodeAuth(h func(http.ResponseWriter, *http.Request, *nodeInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ip := web.ClientIP(r)
		if !s.Limiter.Allow("node:"+ip, 1200, time.Minute) {
			web.APIError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || !strings.HasPrefix(tok, nodeproto.TokenPrefix) {
			web.APIError(w, http.StatusUnauthorized, "missing node token")
			return
		}
		var n nodeInfo
		err := s.Pool.QueryRow(r.Context(), `SELECT node_id, org_id, org_slug, suspended FROM customer_node_check($1)`,
			secure.HashToken(tok)).Scan(&n.ID, &n.OrgID, &n.OrgSlug, &n.Suspended)
		if err != nil {
			if !s.Limiter.Allow("node-fail:"+ip, 30, 10*time.Minute) {
				web.APIError(w, http.StatusTooManyRequests, "too many failed attempts")
				return
			}
			web.APIError(w, http.StatusUnauthorized, "unknown or removed server")
			return
		}
		h(w, r, &n)
	}
}

func readJSON(r *http.Request, max int64, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, max))
	return dec.Decode(v)
}

// nodeJoin exchanges a one-time join token for a node token.
func (s *API) nodeJoin(w http.ResponseWriter, r *http.Request) {
	ip := web.ClientIP(r)
	if !s.Limiter.Allow("node-join:"+ip, 20, 10*time.Minute) {
		web.APIError(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var req nodeproto.JoinRequest
	if err := readJSON(r, 64<<10, &req); err != nil || !strings.HasPrefix(req.Token, nodeproto.JoinPrefix) {
		web.APIError(w, http.StatusBadRequest, "a join token (plj_...) is required")
		return
	}
	var tokenID, orgID, name string
	var createdBy *string
	err := s.Pool.QueryRow(r.Context(), `SELECT token_id, org_id, name, created_by::text FROM server_join_consume($1)`,
		secure.HashToken(req.Token)).Scan(&tokenID, &orgID, &name, &createdBy)
	if err != nil {
		web.APIError(w, http.StatusUnauthorized, "invalid, used or expired join token: create a new one in the console")
		return
	}
	nodeID := "c-" + strings.ToLower(randomAlnum(10))
	nodeToken := nodeproto.TokenPrefix + secure.NewToken()
	regToken := web.RegistryTokenPrefix + secure.NewToken()
	err = db.WithOrg(r.Context(), s.Pool, orgID, func(tx pgx.Tx) error {
		var regID string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO registry_tokens (org_id, name, token_hash, can_push, created_by, expires_at)
			VALUES ($1, $2, $3, false, $4::uuid, NULL) RETURNING id`,
			orgID, web.TruncateStr("server "+name, 60), secure.HashToken(regToken), createdBy).Scan(&regID); err != nil {
			return err
		}
		f := req.Facts
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO nodes (id, name, cluster, role, org_id, token_hash, registry_token_id, created_by, os, arch, cpus, mem_mb, mem_avail_mb,
			                   disk_total_mb, disk_free_mb, docker_version, agent_version, services)
			VALUES ($1, $2, 'default', 'customer', $3, $4, $5, $6::uuid, $7, $8, $9, $10, $11, $12, $13, $14, $15, '{}')`,
			nodeID, name, orgID, secure.HashToken(nodeToken), regID, createdBy, clip(f.OS, 40), clip(f.Arch, 20), f.CPUs, f.MemMB, f.MemAvailMB,
			f.DiskTotalMB, f.DiskFreeMB, clip(f.DockerVersion, 40), clip(f.AgentVersion, 40)); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE server_join_tokens SET node_id = $2 WHERE id = $1`, tokenID, nodeID); err != nil {
			return err
		}
		uid := ""
		if createdBy != nil {
			uid = *createdBy
		}
		return db.Audit(r.Context(), tx, orgID, uid, "server.joined", "server:"+name,
			map[string]string{"id": nodeID, "hostname": clip(f.Hostname, 80), "os": clip(f.OS, 40), "arch": clip(f.Arch, 20)}, ip)
	})
	if err != nil {
		s.Log.Error("server join", "err", err)
		web.APIError(w, http.StatusInternalServerError, "could not register the server")
		return
	}
	web.WriteJSON(w, map[string]string{"node_id": nodeID, "token": nodeToken, "registry_token": regToken})
}

func (s *API) nodeHeartbeat(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	var f nodeproto.Facts
	if err := readJSON(r, 64<<10, &f); err != nil {
		web.APIError(w, http.StatusBadRequest, "bad heartbeat")
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, n.OrgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), `
			UPDATE nodes SET os = $2, arch = $3, cpus = $4, mem_mb = $5, mem_avail_mb = $6, disk_total_mb = $7, disk_free_mb = $8,
			       load1 = $9, containers = $10, docker_version = $11, agent_version = $12, last_seen = now()
			WHERE id = $1`, n.ID, clip(f.OS, 40), clip(f.Arch, 20), f.CPUs, f.MemMB, f.MemAvailMB, f.DiskTotalMB, f.DiskFreeMB,
			min(f.Load1, 999999), f.Containers, clip(f.DockerVersion, 40), clip(f.AgentVersion, 40))
		return err
	})
	if err != nil {
		web.APIError(w, http.StatusInternalServerError, "could not record the heartbeat")
		return
	}
	web.WriteJSON(w, map[string]string{"node_id": n.ID})
}

// nodeDesired returns what the server must run: the latest live deployment of each
// application placed on it, plus the deployments waiting to start, with their complete
// environment (secrets decrypted here: the server never sees the master key).
func (s *API) nodeDesired(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	out := nodeproto.Desired{NodeID: n.ID, Deployments: []nodeproto.Deployment{}}
	if s.Cfg.RegistryHost != "" {
		out.Registry = nodeproto.RegistryAuth{Host: s.Cfg.RegistryHost, Username: "server"}
	}
	if n.Suspended { // a suspended organization runs nothing: the server stops everything
		web.WriteJSON(w, out)
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, n.OrgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT d.id, d.service_id, sv.project_id, coalesce(ps.slug || '-' || sv.preview_name, sv.slug), p.slug, d.revision, d.status, d.spec
			FROM deployments d
			JOIN services sv ON sv.id = d.service_id
			LEFT JOIN services ps ON ps.id = sv.preview_of
			JOIN projects p ON p.id = sv.project_id
			WHERE p.node_id = $1 AND d.status IN ('pending', 'deploying', 'live')
			ORDER BY d.created_at`, n.ID)
		if err != nil {
			return err
		}
		type row struct {
			d    nodeproto.Deployment
			spec model.Spec
		}
		var list []row
		for rows.Next() {
			var x row
			var raw []byte
			if err := rows.Scan(&x.d.ID, &x.d.ServiceID, &x.d.ProjectID, &x.d.Service, &x.d.Project, &x.d.Revision, &x.d.Status, &raw); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal(raw, &x.spec); err != nil {
				rows.Close()
				return err
			}
			list = append(list, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, x := range list {
			env, err := s.deploymentEnv(r.Context(), tx, n.OrgID, x.d.ProjectID, x.d.ServiceID, x.spec)
			if err != nil {
				// The deployment cannot start (a missing secret, for example): fail it with the reason.
				if x.d.Status != "live" {
					_, _ = tx.Exec(r.Context(), `UPDATE deployments SET status = 'failed', message = $2, updated_at = now() WHERE id = $1`, x.d.ID, web.TruncateStr(err.Error(), 2000))
				}
				continue
			}
			d := x.d
			d.Image, d.Port, d.HealthPath, d.CPUMillis, d.MemoryMB = x.spec.Image, x.spec.Port, x.spec.HealthPath, x.spec.CPUMillis, x.spec.MemoryMB
			d.Instances, d.Worker, d.VolumePath, d.Env = x.spec.Count(), x.spec.Worker, x.spec.VolumePath, env
			if d.VolumePath != "" {
				d.Instances = 1
			}
			out.Deployments = append(out.Deployments, d)
		}
		return nil
	})
	if err != nil {
		s.Log.Error("node desired state", "node", n.ID, "err", err)
		web.APIError(w, http.StatusInternalServerError, "could not load the desired state")
		return
	}
	web.WriteJSON(w, out)
}

// deploymentEnv builds an application's environment exactly like the platform agent:
// project variables, then its own variables, then its secrets, then PORT; the last value
// of a repeated name wins; ${secret:NAME} values come from the secrets manager.
func (s *API) deploymentEnv(ctx context.Context, tx pgx.Tx, orgID, projectID, serviceID string, spec model.Spec) ([]string, error) {
	var env []string
	rows, err := tx.Query(ctx, `SELECT name, value_enc FROM project_variables WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	type enc struct {
		Name  string
		Value []byte
	}
	pv, err := pgx.CollectRows(rows, pgx.RowToStructByPos[enc])
	if err != nil {
		return nil, err
	}
	for _, v := range pv {
		plain, err := s.Box.Open(v.Value, web.ProjVarAAD(projectID, v.Name))
		if err != nil {
			return nil, fmt.Errorf("decrypt project variable %s: %w", v.Name, err)
		}
		env = append(env, v.Name+"="+string(plain))
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+spec.Env[k])
	}
	rows, err = tx.Query(ctx, `SELECT name, value_enc FROM secrets WHERE service_id = $1 ORDER BY name`, serviceID)
	if err != nil {
		return nil, err
	}
	sec, err := pgx.CollectRows(rows, pgx.RowToStructByPos[enc])
	if err != nil {
		return nil, err
	}
	for _, v := range sec {
		plain, err := s.Box.Open(v.Value, "secret:"+serviceID+":"+v.Name)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %s: %w", v.Name, err)
		}
		env = append(env, v.Name+"="+string(plain))
	}
	if !spec.Worker {
		env = append(env, "PORT="+strconv.Itoa(spec.Port))
	}
	// Last value of a repeated name, at the position of its first.
	last := map[string]string{}
	var order []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, seen := last[k]; !seen {
			order = append(order, k)
		}
		last[k] = kv
	}
	out := make([]string, 0, len(order))
	cache := map[string]string{}
	for _, k := range order {
		kv := last[k]
		_, v, _ := strings.Cut(kv, "=")
		name, ok := model.SecretRef(v)
		if !ok {
			out = append(out, kv)
			continue
		}
		val, seen := cache[name]
		if !seen {
			var id string
			var version int
			var ct []byte
			if err := tx.QueryRow(ctx, `
				SELECT sc.id, v.version, v.value_enc FROM org_secrets sc JOIN org_secret_versions v ON v.secret_id = sc.id AND v.version = sc.current_version
				WHERE sc.org_id = $1 AND sc.name = $2`, orgID, name).Scan(&id, &version, &ct); err != nil {
				return nil, fmt.Errorf("variable %s references secret %q, which does not exist in the secrets manager", k, name)
			}
			plain, err := s.Box.Open(ct, "orgsecret:"+id+":"+strconv.Itoa(version))
			if err != nil {
				return nil, fmt.Errorf("decrypt secret %q: %w", name, err)
			}
			val = string(plain)
			cache[name] = val
		}
		out = append(out, k+"="+val)
	}
	return out, nil
}

// nodeDeploymentStatus records the progress of a deployment the server runs. Only a
// deployment of a project placed on this server can be changed.
func (s *API) nodeDeploymentStatus(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	var rep nodeproto.StatusReport
	if err := readJSON(r, 256<<10, &rep); err != nil {
		web.APIError(w, http.StatusBadRequest, "bad status report")
		return
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		web.APIError(w, http.StatusNotFound, "unknown deployment")
		return
	}
	msg := web.TruncateStr(rep.Message, 2000)
	var found bool
	err := db.WithOrg(r.Context(), s.Pool, n.OrgID, func(tx pgx.Tx) error {
		var serviceID, status string
		var revision int
		var image string
		err := tx.QueryRow(r.Context(), `
			SELECT d.service_id, d.status, d.revision, d.spec->>'image' FROM deployments d
			JOIN services sv ON sv.id = d.service_id JOIN projects p ON p.id = sv.project_id
			WHERE d.id = $1 AND p.node_id = $2 FOR UPDATE OF d`, id, n.ID).Scan(&serviceID, &status, &revision, &image)
		if web.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		switch rep.Status {
		case "":
			if status == "deploying" {
				_, err = tx.Exec(r.Context(), `UPDATE deployments SET message = $2, updated_at = now() WHERE id = $1`, id, msg)
			}
			return err
		case "deploying":
			if status == "pending" || status == "deploying" {
				_, err = tx.Exec(r.Context(), `UPDATE deployments SET status = 'deploying', message = $2, updated_at = now() WHERE id = $1`, id, msg)
			}
			return err
		case "failed":
			if status != "pending" && status != "deploying" {
				return nil
			}
			if _, err := tx.Exec(r.Context(), `UPDATE deployments SET status = 'failed', message = $2, updated_at = now() WHERE id = $1`, id, msg); err != nil {
				return err
			}
			return db.Audit(r.Context(), tx, n.OrgID, "", "deployment.failed", "service:"+serviceID,
				map[string]any{"revision": revision, "error": web.TruncateStr(msg, 300), "server": n.ID}, "")
		case "live":
			if status != "pending" && status != "deploying" {
				return nil
			}
			if len(rep.Instances) == 0 || len(rep.Instances) > 10 {
				return errBadReport
			}
			type inst struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Upstream string `json:"upstream"`
			}
			insts := make([]inst, 0, len(rep.Instances))
			for _, name := range rep.Instances {
				// The proxy reaches every instance through the tunnel; the server spreads requests.
				insts = append(insts, inst{Name: clip(name, 100), Upstream: "tunnel:" + id})
			}
			raw, _ := json.Marshal(insts)
			if _, err := tx.Exec(r.Context(), `UPDATE deployments SET status = 'superseded', updated_at = now()
				WHERE service_id = $1 AND status = 'live' AND id <> $2`, serviceID, id); err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `
				UPDATE deployments SET status = 'live', message = '', container_id = '', container_name = $2, upstream = $3, node_id = $4,
				       instances = $5, updated_at = now()
				WHERE id = $1`, id, insts[0].Name, "tunnel:"+id, n.ID, raw); err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`); err != nil { // the proxy switches routes
				return err
			}
			return db.Audit(r.Context(), tx, n.OrgID, "", "deployment.live", "service:"+serviceID,
				map[string]any{"revision": revision, "image": image, "server": n.ID}, "")
		default:
			return errBadReport
		}
	})
	switch {
	case errors.Is(err, errBadReport):
		web.APIError(w, http.StatusBadRequest, "bad status report")
	case err != nil:
		s.Log.Error("node status", "node", n.ID, "err", err)
		web.APIError(w, http.StatusInternalServerError, "could not record the status")
	case !found:
		web.APIError(w, http.StatusNotFound, "unknown deployment")
	default:
		web.WriteJSON(w, map[string]bool{"ok": true})
	}
}

var (
	errBadReport = errors.New("bad report")
	uuidRe       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// nodeDeployments maps this server's deployments to their project and service, so
// that logs and statistics are only ever filed under the organization's own resources.
func nodeDeployments(ctx context.Context, tx pgx.Tx, nodeID string) (map[string][2]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT d.id::text, sv.project_id::text, d.service_id::text FROM deployments d
		JOIN services sv ON sv.id = d.service_id JOIN projects p ON p.id = sv.project_id
		WHERE p.node_id = $1 AND (d.status IN ('pending', 'deploying', 'live') OR d.updated_at > now() - interval '10 minutes')`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var id, project, service string
		if err := rows.Scan(&id, &project, &service); err != nil {
			return nil, err
		}
		out[id] = [2]string{project, service}
	}
	return out, rows.Err()
}

// nodeLogs files application log lines in the log store, labelled by the API (never by
// the server) with the organization, project and service of each deployment.
func (s *API) nodeLogs(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	if !s.Limiter.Allow("node-logs:"+n.ID, 240, time.Minute) {
		web.APIError(w, http.StatusTooManyRequests, "too many log batches")
		return
	}
	var batch nodeproto.LogBatch
	if err := readJSON(r, 1<<20, &batch); err != nil || len(batch.Lines) > 2000 {
		web.APIError(w, http.StatusBadRequest, "bad log batch")
		return
	}
	if s.Cfg.VictoriaLogsURL == "" || len(batch.Lines) == 0 {
		web.WriteJSON(w, map[string]int{"accepted": 0})
		return
	}
	var deps map[string][2]string
	if err := db.WithOrg(r.Context(), s.Pool, n.OrgID, func(tx pgx.Tx) (err error) {
		deps, err = nodeDeployments(r.Context(), tx, n.ID)
		return err
	}); err != nil {
		web.APIError(w, http.StatusInternalServerError, "could not file the logs")
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	accepted := 0
	now := time.Now()
	for _, l := range batch.Lines {
		ids, ok := deps[l.Deployment]
		if !ok {
			continue
		}
		t := l.Time
		if t.IsZero() || t.After(now.Add(time.Minute)) || t.Before(now.Add(-24*time.Hour)) {
			t = now
		}
		stream := "stdout"
		if l.Stream == "stderr" {
			stream = "stderr"
		}
		_ = enc.Encode(map[string]string{
			"_time": t.UTC().Format(time.RFC3339Nano), "_msg": clip(l.Msg, 16<<10), "stream": stream,
			"org_id": n.OrgID, "project_id": ids[0], "service_id": ids[1], "deployment_id": l.Deployment,
		})
		accepted++
	}
	if accepted > 0 {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
			s.Cfg.VictoriaLogsURL+"/insert/jsonline?_stream_fields=org_id,project_id,service_id", &buf)
		req.Header.Set("Content-Type", "application/stream+json")
		resp, err := s.HTTP.Do(req)
		if err != nil {
			web.APIError(w, http.StatusBadGateway, "log store unavailable")
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	web.WriteJSON(w, map[string]int{"accepted": accepted})
}

// nodeStats records the measured use of each application, as for platform servers.
func (s *API) nodeStats(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	var rep nodeproto.StatsReport
	if err := readJSON(r, 256<<10, &rep); err != nil || len(rep.Deployments) > 500 {
		web.APIError(w, http.StatusBadRequest, "bad statistics")
		return
	}
	err := db.WithOrg(r.Context(), s.Pool, n.OrgID, func(tx pgx.Tx) error {
		deps, err := nodeDeployments(r.Context(), tx, n.ID)
		if err != nil {
			return err
		}
		for _, st := range rep.Deployments {
			ids, ok := deps[st.Deployment]
			if !ok {
				continue
			}
			cpu := max(0, min(st.CPUPct, 100000))
			mem := max(0, min(st.MemMB, 1<<20))
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO service_stats (service_id, org_id, node_id, cpu_pct, mem_mb, updated_at) VALUES ($1, $2, $3, $4, $5, now())
				ON CONFLICT (service_id) DO UPDATE SET node_id = EXCLUDED.node_id, cpu_pct = EXCLUDED.cpu_pct, mem_mb = EXCLUDED.mem_mb, updated_at = now()`,
				ids[1], n.OrgID, n.ID, cpu, mem); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.Log.Error("node stats", "node", n.ID, "err", err)
		web.APIError(w, http.StatusInternalServerError, "could not record the statistics")
		return
	}
	web.WriteJSON(w, map[string]bool{"ok": true})
}

// ---------- install ----------

// nodeInstallScript is the one-line installer shown in the console:
//
//	curl -fsSL <console>/node/install.sh | sudo sh -s -- plj_...
func (s *API) nodeInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = io.WriteString(w, strings.ReplaceAll(installScript, "{{CONSOLE}}", s.Cfg.BaseURL))
}

// nodeAgentBinary serves the node agent: the platform binary itself (it has a "node"
// mode), for the architecture this API runs on.
func (s *API) nodeAgentBinary(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("platform") != "linux-"+runtime.GOARCH || runtime.GOOS != "linux" {
		http.Error(w, "the node agent is available for linux-"+runtime.GOARCH+" only", http.StatusNotFound)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="platform-node"`)
	http.ServeFile(w, r, exe)
}

const installScript = `#!/bin/sh
# Attaches this server to your organization. Needs root and Docker.
#   curl -fsSL {{CONSOLE}}/node/install.sh | sudo sh -s -- plj_...
set -eu
TOKEN="${1:-}"
CONSOLE="{{CONSOLE}}"
[ -n "$TOKEN" ] || { echo "usage: install.sh JOIN_TOKEN (create one on the Servers page)" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "Docker is required: https://docs.docker.com/engine/install/" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "Docker is installed but not running" >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
echo "Downloading the node agent..."
curl -fsSL "$CONSOLE/node/v1/agent/linux-$ARCH" -o /usr/local/bin/platform-node.new
chmod 755 /usr/local/bin/platform-node.new
mv /usr/local/bin/platform-node.new /usr/local/bin/platform-node
install -d -m 700 /var/lib/platform-node
/usr/local/bin/platform-node node join --server "$CONSOLE" --token "$TOKEN" --state /var/lib/platform-node
cat > /etc/systemd/system/platform-node.service <<'UNIT'
[Unit]
Description=Platform node agent
After=docker.service network-online.target
Requires=docker.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/platform-node node run --state /var/lib/platform-node
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now platform-node
echo "Done: this server appears as online on the Servers page within a minute."
`

// ---------- small helpers ----------

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomAlnum(n int) string {
	out := make([]byte, n)
	for i := range out {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alnum))))
		out[i] = alnum[v.Int64()]
	}
	return string(out)
}

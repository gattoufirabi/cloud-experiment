package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/docker"
	"platform/internal/model"
)

// Managed databases: PostgreSQL and Redis. Each is one container on its project's
// private network (reachable only by that project's services), under the same sandbox
// and limits as client apps, with its data in a volume and a second "dump" volume that
// holds a consistent export for backups (pg_dump, or a Redis RDB snapshot). A settings
// change recreates the container; data is kept.

type managedDB struct {
	ID, OrgID, ProjectID, Engine, Version string
	CPUMillis, MemoryMB                   int
	DBName, Username                      string
	PasswordEnc                           []byte
	Status, ContainerID                   string
	Suspended                             bool
}

func dbContainerName(id string) string { return "pd-" + model.Compact(id)[:12] }
func dbDataVolume(id string) string    { return "pvdb-" + model.Compact(id) }
func dbDumpVolume(id string) string    { return "pbdb-" + model.Compact(id) }

func (d managedDB) specHash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d|%s|%s", d.Engine, d.Version, d.CPUMillis, d.MemoryMB, d.DBName, d.Username)))
	return hex.EncodeToString(sum[:8])
}

const dbColumns = `d.id, d.org_id, d.project_id, d.engine, d.version, d.cpu_millis, d.memory_mb, d.db_name, d.username,
	d.password_enc, d.status, coalesce(d.container_id, ''), o.suspended`

func (a *Agent) reconcileDatabases(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `SELECT `+dbColumns+` FROM databases d JOIN orgs o ON o.id = d.org_id
		JOIN projects p ON p.id = d.project_id WHERE `+mineSQL, a.mineArgs()...)
	if err != nil {
		a.log.Error("load databases", "err", err)
		return
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[managedDB])
	if err != nil {
		a.log.Error("scan databases", "err", err)
		return
	}
	containers, err := a.dk.ContainerList(ctx, "platform.database")
	if err != nil {
		return
	}
	byID := map[string]docker.ContainerSummary{}
	wanted := map[string]bool{}
	for _, d := range dbs {
		wanted[d.ID] = true
	}
	for _, c := range containers {
		id := c.Labels["platform.database"]
		if !a.owns(c.Labels) {
			continue
		}
		if !wanted[id] {
			a.log.Info("removing container of deleted database", "database", id)
			_ = a.dk.ContainerStop(ctx, c.ID, 20)
			_ = a.dk.ContainerRemove(ctx, c.ID) // volumes are kept, as for services
			continue
		}
		byID[id] = c
	}
	for _, d := range dbs {
		c, ok := byID[d.ID]
		if d.Suspended {
			if ok && c.State == "running" {
				_ = a.dk.ContainerStop(ctx, c.ID, 20)
			}
			continue
		}
		if ok && c.Labels["platform.spec"] == d.specHash() {
			if c.State != "running" && !a.restoring(d.ID) {
				_ = a.dk.ContainerStart(ctx, c.ID)
			}
			if d.Status != "running" || d.ContainerID != c.ID {
				a.setDBStatus(ctx, d.ID, "running", "", c.ID)
			}
			continue
		}
		if ok { // settings changed: recreate on the same volumes
			a.log.Info("recreating database with new settings", "database", d.ID)
			_ = a.dk.ContainerStop(ctx, c.ID, 30)
			_ = a.dk.ContainerRemove(ctx, c.ID)
		}
		if err := a.startDatabase(ctx, d); err != nil {
			a.log.Warn("database failed to start", "database", d.ID, "err", err)
			a.setDBStatus(ctx, d.ID, "failed", truncate(err.Error(), 500), "")
		}
	}
}

// restoring reports whether a restore currently holds this database stopped on purpose.
func (a *Agent) restoring(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restoringDB[id]
}

func (a *Agent) setDBStatus(ctx context.Context, id, status, msg, containerID string) {
	var cid any
	if containerID != "" {
		cid = containerID
	}
	_, _ = a.pool.Exec(ctx, `UPDATE databases SET status = $2, message = $3, container_id = $4 WHERE id = $1`, id, status, msg, cid)
}

// engineConfig returns the image, command, environment and data mount for an engine.
func engineConfig(d managedDB, password string) (image string, cmd, env []string, dataPath string) {
	if d.Engine == "redis" {
		// RDB snapshots only (no append-only file) so a restore is one file; a memory cap
		// below the container limit makes a full cache refuse writes instead of being killed.
		maxmem := d.MemoryMB * 8 / 10
		return "redis:" + d.Version + "-alpine",
			[]string{"sh", "-c", fmt.Sprintf(`exec docker-entrypoint.sh redis-server --save 60 1000 --appendonly no --maxmemory %dmb --maxmemory-policy noeviction --requirepass "$REDIS_PASSWORD"`, maxmem)},
			[]string{"REDIS_PASSWORD=" + password},
			"/data"
	}
	return "postgres:" + d.Version + "-alpine", nil,
		[]string{"POSTGRES_USER=" + d.Username, "POSTGRES_PASSWORD=" + password, "POSTGRES_DB=" + d.DBName, "PGDATA=/var/lib/postgresql/data/pgdata"},
		"/var/lib/postgresql/data"
}

func (a *Agent) startDatabase(ctx context.Context, d managedDB) error {
	if d.CPUMillis > a.cfg.MaxCPUMillis || d.MemoryMB > a.cfg.MaxMemoryMB {
		return errors.New("rejected by policy: resources exceed the host maximum")
	}
	if a.cfg.Runtime != "runsc" && !a.cfg.AllowInsecureRuntime {
		return errors.New("tenant containers must run under gVisor")
	}
	pw, err := a.box.Open(d.PasswordEnc, "db:"+d.ID)
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}
	netName, err := a.ensureNetwork(ctx, job{OrgID: d.OrgID, ProjectID: d.ProjectID})
	if err != nil {
		return err
	}
	image, cmd, env, dataPath := engineConfig(d, string(pw))
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err = a.dk.ImagePull(pctx, image, "")
	cancel()
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	name := dbContainerName(d.ID)
	_ = a.dk.ContainerRemove(ctx, name)
	ip, err := a.databaseAddress(ctx, netName, d.ID)
	if err != nil {
		return err
	}
	hc := docker.HostConfig{
		Runtime:       a.cfg.Runtime,
		Memory:        int64(d.MemoryMB) << 20,
		MemorySwap:    int64(d.MemoryMB) << 20,
		NanoCPUs:      int64(d.CPUMillis) * 1_000_000,
		PidsLimit:     256,
		CapDrop:       []string{"ALL"},
		CapAdd:        []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"},
		SecurityOpt:   []string{"no-new-privileges:true"},
		NetworkMode:   netName,
		RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		LogConfig:     docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
		Mounts: []docker.Mount{
			{Type: "volume", Source: dbDataVolume(d.ID), Target: dataPath},
			{Type: "volume", Source: dbDumpVolume(d.ID), Target: "/backup"},
		},
	}
	a.tenantDNS(&hc)
	id, err := a.dk.ContainerCreate(ctx, name, docker.ContainerConfig{
		Image: image,
		Cmd:   cmd,
		Env:   env,
		Labels: map[string]string{
			"platform.database": d.ID, "platform.engine": d.Engine, "platform.org": d.OrgID, "platform.node": a.selfID,
			"platform.project": d.ProjectID, "platform.spec": d.specHash(),
		},
		HostConfig:       hc,
		NetworkingConfig: docker.FixedAddress(netName, ip),
	})
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		_ = a.dk.ContainerRemove(ctx, id)
		return fmt.Errorf("start container: %w", err)
	}
	if err := a.waitReady(ctx, id, d); err != nil {
		return err
	}
	a.setDBStatus(ctx, d.ID, "running", "", id)
	a.refreshDBHosts(ctx, d, netName, ip)
	a.log.Info("database running", "database", d.ID, "engine", d.Engine, "container", name)
	return nil
}

// readyCmd is run inside the container: pg_isready, or redis-cli PING with the password
// taken from the container's own environment (never on the agent's command line).
func readyCmd(d managedDB) []string {
	if d.Engine == "redis" {
		return []string{"sh", "-c", `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning ping | grep -q PONG`}
	}
	return []string{"pg_isready", "-U", d.Username, "-d", d.DBName, "-h", "127.0.0.1"}
}

func (a *Agent) waitReady(ctx context.Context, id string, d managedDB) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		info, err := a.dk.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !info.State.Running {
			return fmt.Errorf("database container stopped (exit code %d)%s", info.State.ExitCode, a.tail(ctx, id))
		}
		if _, _, code, err := a.dk.ContainerExec(ctx, id, readyCmd(d), nil); err == nil && code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("database did not become ready within 90 s%s", a.tail(ctx, id))
}

func (a *Agent) dbForBackup(ctx context.Context, id string) (managedDB, error) {
	var d managedDB
	err := a.pool.QueryRow(ctx, `SELECT `+dbColumns+` FROM databases d JOIN orgs o ON o.id = d.org_id WHERE d.id = $1`, id).
		Scan(&d.ID, &d.OrgID, &d.ProjectID, &d.Engine, &d.Version, &d.CPUMillis, &d.MemoryMB, &d.DBName, &d.Username,
			&d.PasswordEnc, &d.Status, &d.ContainerID, &d.Suspended)
	if err != nil {
		return d, err
	}
	if d.Status != "running" || d.ContainerID == "" {
		return d, errors.New("the database is not running")
	}
	return d, nil
}

// backupDatabase writes a consistent export to the dump volume, then backs it up.
func (a *Agent) backupDatabase(ctx context.Context, j backupJob, tag string) (string, error) {
	d, err := a.dbForBackup(ctx, j.DatabaseID)
	if err != nil {
		return "", err
	}
	cmd := []string{"pg_dump", "-U", d.Username, "-d", d.DBName, "-h", "127.0.0.1", "-Fc", "-f", "/backup/db.dump"}
	tool := "pg_dump"
	if d.Engine == "redis" {
		// --rdb asks the server for a point-in-time snapshot and saves it as dump.rdb.
		cmd = []string{"sh", "-c", `rm -f /backup/* && redis-cli -a "$REDIS_PASSWORD" --no-auth-warning --rdb /backup/dump.rdb >/dev/null`}
		tool = "redis snapshot"
	}
	_, stderr, code, err := a.dk.ContainerExec(ctx, d.ContainerID, cmd, nil)
	if err != nil || code != 0 {
		return "", fmt.Errorf("%s failed (code %d): %s", tool, code, truncate(strings.TrimSpace(string(stderr)), 400))
	}
	return a.backupVolume(ctx, j, dbDumpVolume(d.ID), tag)
}

// restoreDatabase: safety snapshot first, then load the chosen snapshot.
//   - PostgreSQL: restore the dump file, then pg_restore --clean in one transaction.
//   - Redis: stop the server, put the snapshot's dump.rdb in the data volume, start it.
func (a *Agent) restoreDatabase(ctx context.Context, j backupJob) (string, error) {
	if !a.snapshotBelongs(ctx, j) {
		return "", errors.New("this snapshot does not belong to the database")
	}
	if _, err := a.backupDatabase(ctx, j, "pre-restore"); err != nil {
		return "", fmt.Errorf("safety snapshot before restore failed, nothing was changed: %w", err)
	}
	d, err := a.dbForBackup(ctx, j.DatabaseID)
	if err != nil {
		return "", err
	}
	done := "restored snapshot " + short(j.Snapshot) + " (a pre-restore snapshot of the previous data was kept)"
	if d.Engine == "redis" {
		a.mu.Lock()
		a.restoringDB[d.ID] = true
		a.mu.Unlock()
		defer func() {
			a.mu.Lock()
			delete(a.restoringDB, d.ID)
			a.mu.Unlock()
		}()
		if err := a.dk.ContainerStop(ctx, d.ContainerID, 20); err != nil {
			return "", fmt.Errorf("stop redis before restoring: %w", err)
		}
		// The snapshot holds /data/dump.rdb (the dump volume was mounted at /data when it
		// was backed up), so restoring it into the data volume puts it where Redis loads it.
		rerr := a.restoreVolume(ctx, j, dbDataVolume(d.ID))
		if err := a.dk.ContainerStart(ctx, d.ContainerID); err != nil {
			return "", fmt.Errorf("start redis after restoring: %w", err)
		}
		if rerr != nil {
			return "", rerr
		}
		if err := a.waitReady(ctx, d.ContainerID, d); err != nil {
			return "", err
		}
		return done, nil
	}
	if err := a.restoreVolume(ctx, j, dbDumpVolume(d.ID)); err != nil {
		return "", err
	}
	_, stderr, code, err := a.dk.ContainerExec(ctx, d.ContainerID, []string{
		"pg_restore", "-U", d.Username, "-d", d.DBName, "-h", "127.0.0.1",
		"--clean", "--if-exists", "--no-owner", "--single-transaction", "/backup/db.dump"}, nil)
	if err != nil || code != 0 {
		return "", fmt.Errorf("pg_restore failed (code %d): %s", code, truncate(strings.TrimSpace(string(stderr)), 400))
	}
	return done, nil
}

// refreshDBHosts redeploys the project's live services whose hosts entry for this
// database points at an old address (a database recreated before fixed addresses
// existed, or moved after an address collision). Deploys switch with no downtime.
func (a *Agent) refreshDBHosts(ctx context.Context, d managedDB, netName, ip string) {
	want := dbContainerName(d.ID) + ":" + ip
	prefix := dbContainerName(d.ID) + ":"
	rows, err := a.pool.Query(ctx, `
		SELECT dp.service_id, dp.container_id, dp.spec FROM deployments dp JOIN services s ON s.id = dp.service_id
		WHERE dp.status = 'live' AND s.project_id = $1 AND dp.container_id IS NOT NULL`, d.ProjectID)
	if err != nil {
		return
	}
	type live struct {
		serviceID, containerID string
		spec                   []byte
	}
	var stale []live
	var all []live
	for rows.Next() {
		var l live
		if rows.Scan(&l.serviceID, &l.containerID, &l.spec) == nil {
			all = append(all, l)
		}
	}
	rows.Close()
	for _, l := range all {
		info, err := a.dk.ContainerInspect(ctx, l.containerID)
		if err != nil {
			continue
		}
		for _, h := range info.HostConfig.ExtraHosts {
			if strings.HasPrefix(h, prefix) && h != want {
				stale = append(stale, l)
				break
			}
		}
	}
	for _, l := range stale {
		if _, err := a.pool.Exec(ctx, `
			INSERT INTO deployments (org_id, service_id, revision, spec)
			VALUES ($1, $2, (SELECT coalesce(max(revision), 0) + 1 FROM deployments WHERE service_id = $2), $3)`,
			d.OrgID, l.serviceID, l.spec); err != nil {
			a.log.Warn("redeploy after database move", "service", l.serviceID, "err", err)
			continue
		}
		_ = auditAgent(ctx, a, d.OrgID, "deployment.redeployed", "service:"+l.serviceID, map[string]string{"reason": "database address changed"})
		a.log.Info("redeploying after database address change", "service", l.serviceID, "database", d.ID)
	}
	if len(stale) > 0 {
		a.poke()
	}
}

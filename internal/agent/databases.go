package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/docker"
	"platform/internal/model"
)

// Managed PostgreSQL. Each database is one container on its project's private network
// (reachable only by that project's services), under the same sandbox and limits as
// client apps, with its data in a volume and a second volume that holds pg_dump output
// for consistent backups. A settings change recreates the container; data is kept.

type managedDB struct {
	ID, OrgID, ProjectID, Version string
	CPUMillis, MemoryMB           int
	DBName, Username              string
	PasswordEnc                   []byte
	Status, ContainerID           string
	Suspended                     bool
}

func dbContainerName(id string) string { return "pd-" + model.Compact(id)[:12] }
func dbDataVolume(id string) string    { return "pvdb-" + model.Compact(id) }
func dbDumpVolume(id string) string    { return "pbdb-" + model.Compact(id) }

func (d managedDB) specHash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s|%s", d.Version, d.CPUMillis, d.MemoryMB, d.DBName, d.Username)))
	return hex.EncodeToString(sum[:8])
}

func (a *Agent) reconcileDatabases(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.id, d.org_id, d.project_id, d.version, d.cpu_millis, d.memory_mb, d.db_name, d.username,
		       d.password_enc, d.status, coalesce(d.container_id, ''), o.suspended
		FROM databases d JOIN orgs o ON o.id = d.org_id`)
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
			if c.State != "running" {
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

func (a *Agent) setDBStatus(ctx context.Context, id, status, msg, containerID string) {
	var cid any
	if containerID != "" {
		cid = containerID
	}
	_, _ = a.pool.Exec(ctx, `UPDATE databases SET status = $2, message = $3, container_id = $4 WHERE id = $1`, id, status, msg, cid)
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
	image := "postgres:" + d.Version + "-alpine"
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err = a.dk.ImagePull(pctx, image, "")
	cancel()
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	name := dbContainerName(d.ID)
	_ = a.dk.ContainerRemove(ctx, name)
	id, err := a.dk.ContainerCreate(ctx, name, docker.ContainerConfig{
		Image: image,
		Env: []string{
			"POSTGRES_USER=" + d.Username, "POSTGRES_PASSWORD=" + string(pw), "POSTGRES_DB=" + d.DBName,
			"PGDATA=/var/lib/postgresql/data/pgdata",
		},
		Labels: map[string]string{
			"platform.database": d.ID, "platform.org": d.OrgID, "platform.project": d.ProjectID, "platform.spec": d.specHash(),
		},
		HostConfig: docker.HostConfig{
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
				{Type: "volume", Source: dbDataVolume(d.ID), Target: "/var/lib/postgresql/data"},
				{Type: "volume", Source: dbDumpVolume(d.ID), Target: "/backup"},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		_ = a.dk.ContainerRemove(ctx, id)
		return fmt.Errorf("start container: %w", err)
	}
	if err := a.waitPostgres(ctx, id, netName, d); err != nil {
		return err
	}
	a.setDBStatus(ctx, d.ID, "running", "", id)
	a.log.Info("database running", "database", d.ID, "container", name)
	return nil
}

// waitPostgres waits until the server accepts connections (pg_isready inside the container).
func (a *Agent) waitPostgres(ctx context.Context, id, netName string, d managedDB) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		info, err := a.dk.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if !info.State.Running {
			return fmt.Errorf("database container stopped (exit code %d)%s", info.State.ExitCode, a.tail(ctx, id))
		}
		_, _, code, err := a.dk.ContainerExec(ctx, id, []string{"pg_isready", "-U", d.Username, "-d", d.DBName, "-h", "127.0.0.1"}, nil)
		if err == nil && code == 0 {
			if ip := info.NetworkSettings.Networks[netName].IPAddress; ip != "" {
				if conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "5432"), 2*time.Second); err == nil {
					conn.Close()
				}
			}
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
	err := a.pool.QueryRow(ctx, `SELECT id, db_name, username, status, coalesce(container_id, '') FROM databases WHERE id = $1`, id).
		Scan(&d.ID, &d.DBName, &d.Username, &d.Status, &d.ContainerID)
	if err != nil {
		return d, err
	}
	if d.Status != "running" || d.ContainerID == "" {
		return d, errors.New("the database is not running")
	}
	return d, nil
}

// backupDatabase writes a consistent pg_dump to the dump volume, then backs it up.
func (a *Agent) backupDatabase(ctx context.Context, j backupJob, tag string) (string, error) {
	d, err := a.dbForBackup(ctx, j.DatabaseID)
	if err != nil {
		return "", err
	}
	_, stderr, code, err := a.dk.ContainerExec(ctx, d.ContainerID,
		[]string{"pg_dump", "-U", d.Username, "-d", d.DBName, "-h", "127.0.0.1", "-Fc", "-f", "/backup/db.dump"}, nil)
	if err != nil || code != 0 {
		return "", fmt.Errorf("pg_dump failed (code %d): %s", code, truncate(strings.TrimSpace(string(stderr)), 400))
	}
	return a.backupVolume(ctx, j, dbDumpVolume(d.ID), tag)
}

// restoreDatabase: safety dump, restore the snapshot's dump file, then pg_restore --clean.
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
	if err := a.restoreVolume(ctx, j, dbDumpVolume(d.ID)); err != nil {
		return "", err
	}
	_, stderr, code, err := a.dk.ContainerExec(ctx, d.ContainerID, []string{
		"pg_restore", "-U", d.Username, "-d", d.DBName, "-h", "127.0.0.1",
		"--clean", "--if-exists", "--no-owner", "--single-transaction", "/backup/db.dump"}, nil)
	if err != nil || code != 0 {
		return "", fmt.Errorf("pg_restore failed (code %d): %s", code, truncate(strings.TrimSpace(string(stderr)), 400))
	}
	return "restored snapshot " + short(j.Snapshot) + " (a pre-restore snapshot of the previous data was kept)", nil
}

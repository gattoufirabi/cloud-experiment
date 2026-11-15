package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/docker"
	"platform/internal/model"
)

// Per-client backups with restic.
//
//   - One repository per organization, with its own random password (stored encrypted
//     with the master key). Restoring one client can never read or touch another's data.
//   - Inside a repository, snapshots are grouped by service (restic "host" = service ID).
//   - Every job is a short-lived restic container. Local mode: no network at all.
//     Off-site mode (rest-server): only the platform network. No Docker socket, read-only
//     root filesystem, only the capabilities needed (read files for backup; write and
//     chown for restore).
//   - A restore always targets the service's own volume, chosen by the agent, and is
//     preceded by a safety snapshot.

var (
	snapshotRe = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	keepRe     = regexp.MustCompile(`^[a-z0-9 -]+$`)
)

type backupJob struct {
	ID, OrgID, ServiceID, Kind, Snapshot string
}

func volumeName(serviceID string) string { return "pv-" + model.Compact(serviceID) }

func (a *Agent) backupLoop(ctx context.Context) {
	if !a.cfg.BackupsEnabled {
		return
	}
	if !keepRe.MatchString(a.cfg.BackupKeep) {
		a.log.Error("BACKUP_KEEP contains invalid characters; backups disabled")
		return
	}
	_, _ = a.pool.Exec(ctx, `UPDATE backup_jobs SET status = 'error', message = 'interrupted by agent restart', finished_at = now() WHERE status = 'running'`)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		a.scheduleBackups(ctx)
		a.processBackups(ctx)
		select {
		case <-ctx.Done():
			return
		case <-a.backupWake:
		case <-tick.C:
		}
	}
}

func (a *Agent) pokeBackups() {
	select {
	case a.backupWake <- struct{}{}:
	default:
	}
}

// scheduleBackups queues the nightly backup of every service with a volume, once a day
// after its backup hour (UTC). backup_attempted_at prevents retry storms on failure.
func (a *Agent) scheduleBackups(ctx context.Context) {
	_, err := a.pool.Exec(ctx, `
		WITH due AS (
			UPDATE services s SET backup_attempted_at = now()
			FROM orgs o
			WHERE o.id = s.org_id AND NOT o.suspended
			  AND s.volume_path <> '' AND s.backup_enabled
			  AND extract(hour FROM now() AT TIME ZONE 'UTC') >= s.backup_hour
			  AND (s.backup_attempted_at IS NULL OR s.backup_attempted_at <
			       (date_trunc('day', now() AT TIME ZONE 'UTC') + make_interval(hours => s.backup_hour)) AT TIME ZONE 'UTC')
			  AND EXISTS (SELECT 1 FROM deployments d WHERE d.service_id = s.id AND d.status = 'live')
			  AND NOT EXISTS (SELECT 1 FROM backup_jobs j WHERE j.service_id = s.id AND j.status IN ('queued', 'running'))
			RETURNING s.id, s.org_id
		)
		INSERT INTO backup_jobs (org_id, service_id, kind, trigger) SELECT org_id, id, 'backup', 'schedule' FROM due`)
	if err != nil {
		a.log.Error("schedule backups", "err", err)
	}
}

func (a *Agent) processBackups(ctx context.Context) {
	for ctx.Err() == nil {
		var j backupJob
		found := false
		err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `
				SELECT id, org_id, service_id, kind, snapshot_id FROM backup_jobs
				WHERE status = 'queued' ORDER BY created_at LIMIT 1
				FOR UPDATE SKIP LOCKED`).Scan(&j.ID, &j.OrgID, &j.ServiceID, &j.Kind, &j.Snapshot)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			_, err = tx.Exec(ctx, `UPDATE backup_jobs SET status = 'running', message = '' WHERE id = $1`, j.ID)
			return err
		})
		if err != nil {
			a.log.Error("claim backup job", "err", err)
			return
		}
		if !found {
			return
		}
		a.log.Info("backup job", "job", j.ID, "kind", j.Kind, "service", j.ServiceID)
		var msg string
		if j.Kind == "restore" {
			msg, err = a.restore(ctx, j)
		} else {
			msg, err = a.backup(ctx, j, "")
		}
		status, action := "done", "backup."+j.Kind
		if err != nil {
			status, msg, action = "error", truncate(err.Error(), 1500), "backup."+j.Kind+".failed"
			a.log.Warn("backup job failed", "job", j.ID, "err", err)
			a.alertEvent(ctx, j.OrgID, j.ServiceID, "backup_failed",
				fmt.Sprintf("The %s job failed: %s", j.Kind, truncate(firstLine(err.Error()), 300)))
		}
		_, _ = a.pool.Exec(ctx, `UPDATE backup_jobs SET status = $2, message = $3, finished_at = now() WHERE id = $1`, j.ID, status, msg)
		_ = auditAgent(ctx, a, j.OrgID, action, "service:"+j.ServiceID, map[string]string{"job": j.ID, "snapshot": j.Snapshot})
	}
}

// orgBackupKey returns the organization's repository password, creating it once.
func (a *Agent) orgBackupKey(ctx context.Context, orgID string) (string, error) {
	aad := "backup:" + orgID
	var enc []byte
	if err := a.pool.QueryRow(ctx, `SELECT backup_key_enc FROM orgs WHERE id = $1`, orgID).Scan(&enc); err != nil {
		return "", err
	}
	if enc == nil {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		sealed, err := a.box.Seal([]byte(base64.RawURLEncoding.EncodeToString(raw)), aad)
		if err != nil {
			return "", err
		}
		// Only the first writer wins; re-read so concurrent jobs agree on one key.
		if _, err := a.pool.Exec(ctx, `UPDATE orgs SET backup_key_enc = $2 WHERE id = $1 AND backup_key_enc IS NULL`, orgID, sealed); err != nil {
			return "", err
		}
		if err := a.pool.QueryRow(ctx, `SELECT backup_key_enc FROM orgs WHERE id = $1`, orgID).Scan(&enc); err != nil {
			return "", err
		}
	}
	plain, err := a.box.Open(enc, aad)
	return string(plain), err
}

// resticRun runs one shell script in a restic container against the org's repository.
func (a *Agent) resticRun(ctx context.Context, orgID, serviceID, script string, write bool, extraEnv ...string) (string, error) {
	key, err := a.orgBackupKey(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("backup key: %w", err)
	}
	a.mu.Lock()
	pulled := a.resticPulled
	a.mu.Unlock()
	if !pulled {
		if err := a.dk.ImagePull(ctx, a.cfg.ResticImage, ""); err != nil {
			return "", fmt.Errorf("pull restic image: %w", err)
		}
		a.mu.Lock()
		a.resticPulled = true
		a.mu.Unlock()
	}
	vol := volumeName(serviceID)
	mounts := []docker.Mount{{Type: "volume", Source: vol, Target: "/data", ReadOnly: !write}}
	network := "none"
	repo := a.cfg.BackupRestURL + "/" + orgID
	if a.cfg.BackupRestURL == "" {
		mounts = append(mounts, docker.Mount{Type: "volume", Source: a.cfg.BackupVolume, Target: "/repos"})
		repo = "/repos/" + orgID
	} else {
		network = a.cfg.BackupNetwork
	}
	caps := []string{"DAC_READ_SEARCH"}
	if write {
		caps = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "DAC_READ_SEARCH"}
	}
	env := append([]string{
		"RESTIC_REPOSITORY=" + repo, "RESTIC_PASSWORD=" + key, "RESTIC_CACHE_DIR=/tmp/cache",
		"HOME=/tmp", "SVC=" + serviceID, "KEEP=" + a.cfg.BackupKeep,
	}, extraEnv...)
	cfg := docker.ContainerConfig{
		Image:      a.cfg.ResticImage,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{script},
		Env:        env,
		Labels:     map[string]string{"platform.backup": "true", "platform.org": orgID, "platform.service": serviceID},
		HostConfig: docker.HostConfig{
			NetworkMode:    network,
			Memory:         1 << 30,
			MemorySwap:     1 << 30,
			NanoCPUs:       1_000_000_000,
			PidsLimit:      256,
			CapDrop:        []string{"ALL"},
			CapAdd:         caps,
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/tmp": "rw,size=512m"},
			Mounts:         mounts,
			LogConfig:      docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "16m"}},
		},
	}
	suffix := make([]byte, 5)
	_, _ = rand.Read(suffix)
	id, err := a.dk.ContainerCreate(ctx, "backup-"+hex.EncodeToString(suffix), cfg)
	if err != nil {
		return "", fmt.Errorf("create backup container: %w", err)
	}
	defer a.dk.ContainerRemove(context.Background(), id)
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		return "", fmt.Errorf("start backup container: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	code, err := a.dk.ContainerWait(wctx, id)
	cancel()
	if err != nil {
		return "", fmt.Errorf("backup container did not finish: %w", err)
	}
	stdout, stderr, err := a.dk.ContainerOutput(ctx, id)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("restic exited with code %d: %s", code, truncate(lastLines(string(stderr), 5), 800))
	}
	return string(stdout), nil
}

// backup snapshots the service volume, applies retention, and syncs the snapshot list.
func (a *Agent) backup(ctx context.Context, j backupJob, tag string) (string, error) {
	if ok, err := a.dk.VolumeExists(ctx, volumeName(j.ServiceID)); err != nil {
		return "", err
	} else if !ok {
		return "", errors.New("nothing to back up yet: the service has no persistent folder data (deploy it first)")
	}
	if tag == "" {
		tag = "manual"
	}
	script := `set -e
restic cat config >/dev/null 2>&1 || restic init >/dev/null
echo @@BACKUP
restic backup /data --host "$SVC" --tag "$TAG" --json --quiet
echo @@FORGET
restic forget --host "$SVC" --keep-tag pre-restore $KEEP --prune --quiet >/dev/null
echo @@SNAPSHOTS
restic snapshots --host "$SVC" --json`
	out, err := a.resticRun(ctx, j.OrgID, j.ServiceID, script, false, "TAG="+tag)
	if err != nil {
		return "", err
	}
	sections := splitSections(out)
	var summary struct {
		SnapshotID string `json:"snapshot_id"`
		Bytes      int64  `json:"total_bytes_processed"`
		Files      int64  `json:"total_files_processed"`
		Added      int64  `json:"data_added"`
	}
	for _, line := range strings.Split(sections["BACKUP"], "\n") {
		if strings.Contains(line, `"summary"`) {
			_ = json.Unmarshal([]byte(line), &summary)
		}
	}
	if err := a.syncSnapshots(ctx, j, sections["SNAPSHOTS"], summary.SnapshotID, summary.Bytes, summary.Files); err != nil {
		return "", err
	}
	return fmt.Sprintf("snapshot %s: %d files, %s, %s new after deduplication",
		short(summary.SnapshotID), summary.Files, humanBytes(summary.Bytes), humanBytes(summary.Added)), nil
}

func (a *Agent) syncSnapshots(ctx context.Context, j backupJob, raw, newID string, size, files int64) error {
	var snaps []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
		Tags []string  `json:"tags"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &snaps); err != nil {
		return fmt.Errorf("read snapshot list: %w", err)
	}
	return pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		ids := make([]string, 0, len(snaps))
		for _, s := range snaps {
			ids = append(ids, s.ID)
			var sz, fl int64
			if s.ID == newID {
				sz, fl = size, files
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO backup_snapshots (org_id, service_id, snapshot_id, taken_at, size_bytes, files, tags)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (service_id, snapshot_id) DO UPDATE SET tags = EXCLUDED.tags`,
				j.OrgID, j.ServiceID, s.ID, s.Time, sz, fl, strings.Join(s.Tags, ",")); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM backup_snapshots WHERE service_id = $1 AND NOT (snapshot_id = ANY($2))`, j.ServiceID, ids)
		return err
	})
}

// restore replaces the service volume with a snapshot: safety snapshot, stop, restore, start.
func (a *Agent) restore(ctx context.Context, j backupJob) (string, error) {
	if !snapshotRe.MatchString(j.Snapshot) {
		return "", errors.New("invalid snapshot id")
	}
	var known bool
	if err := a.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM backup_snapshots WHERE service_id = $1 AND snapshot_id LIKE $2 || '%')`,
		j.ServiceID, j.Snapshot).Scan(&known); err != nil || !known {
		return "", errors.New("this snapshot does not belong to the service")
	}
	if _, err := a.backup(ctx, j, "pre-restore"); err != nil {
		return "", fmt.Errorf("safety snapshot before restore failed, nothing was changed: %w", err)
	}
	var containerID string
	_ = a.pool.QueryRow(ctx, `SELECT coalesce(container_id, '') FROM deployments WHERE service_id = $1 AND status = 'live'`, j.ServiceID).Scan(&containerID)
	if containerID != "" {
		if err := a.dk.ContainerStop(ctx, containerID, 20); err != nil {
			return "", fmt.Errorf("stop the application before restoring: %w", err)
		}
		defer func() {
			if err := a.dk.ContainerStart(context.Background(), containerID); err != nil {
				a.log.Error("restart after restore", "container", containerID, "err", err)
			}
		}()
	}
	script := `set -e
find /data -mindepth 1 -delete
restic restore "$SNAP" --host "$SVC" --target /`
	if _, err := a.resticRun(ctx, j.OrgID, j.ServiceID, script, true, "SNAP="+j.Snapshot); err != nil {
		return "", err
	}
	return "restored snapshot " + short(j.Snapshot) + " (a pre-restore snapshot of the previous data was kept)", nil
}

func splitSections(out string) map[string]string {
	res := map[string]string{}
	cur := ""
	var b bytes.Buffer
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "@@") {
			if cur != "" {
				res[cur] = b.String()
			}
			cur, b = strings.TrimPrefix(line, "@@"), bytes.Buffer{}
			continue
		}
		b.WriteString(line + "\n")
	}
	if cur != "" {
		res[cur] = b.String()
	}
	return res
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

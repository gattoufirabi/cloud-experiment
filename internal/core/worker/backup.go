package worker

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

	"platform/internal/lib/docker"
	"platform/internal/lib/model"
)

// Per-client backups with restic.
//
//   - One repository per organization, with its own random password (stored encrypted
//     with the master key). Restoring one client can never read or touch another's data.
//   - Inside a repository, snapshots are grouped by target (restic "host" = service or
//     database ID).
//   - Service targets: the persistent folder volume, taken while the app runs.
//   - Database targets: a consistent pg_dump written to a dump volume first, then backed
//     up; restore = restic restore of the dump + pg_restore.
//   - Every restic job is a short-lived container. Local mode: no network at all.
//     Off-site mode (rest-server): only the platform network. No Docker socket, read-only
//     root filesystem, only the capabilities needed.
//   - A restore always targets the job's own volume or database, chosen by the agent, and
//     is preceded by a safety snapshot.

var (
	SnapshotRe = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	KeepRe     = regexp.MustCompile(`^[a-z0-9 -]+$`)
)

type BackupJob struct {
	ID, OrgID, ServiceID, DatabaseID, Kind, Snapshot string
}

// target returns the restic host name and the column that owns snapshots.
func (j BackupJob) Target() (string, string) {
	if j.DatabaseID != "" {
		return j.DatabaseID, "database_id"
	}
	return j.ServiceID, "service_id"
}

func VolumeName(serviceID string) string { return "pv-" + model.Compact(serviceID) }

// orgBackupKey returns the organization's repository password, creating it once.
func (a *Agent) orgBackupKey(ctx context.Context, orgID string) (string, error) {
	aad := "backup:" + orgID
	var enc []byte
	if err := a.Pool.QueryRow(ctx, `SELECT backup_key_enc FROM orgs WHERE id = $1`, orgID).Scan(&enc); err != nil {
		return "", err
	}
	if enc == nil {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		sealed, err := a.Box.Seal([]byte(base64.RawURLEncoding.EncodeToString(raw)), aad)
		if err != nil {
			return "", err
		}
		// Only the first writer wins; re-read so concurrent jobs agree on one key.
		if _, err := a.Pool.Exec(ctx, `UPDATE orgs SET backup_key_enc = $2 WHERE id = $1 AND backup_key_enc IS NULL`, orgID, sealed); err != nil {
			return "", err
		}
		if err := a.Pool.QueryRow(ctx, `SELECT backup_key_enc FROM orgs WHERE id = $1`, orgID).Scan(&enc); err != nil {
			return "", err
		}
	}
	plain, err := a.Box.Open(enc, aad)
	return string(plain), err
}

// resticRun runs one shell script in a restic container against the org's repository,
// with volume mounted at /data (read-only unless write).
func (a *Agent) ResticRun(ctx context.Context, orgID, host, volume, script string, write bool, extraEnv ...string) (string, error) {
	key, err := a.orgBackupKey(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("backup key: %w", err)
	}
	a.Mu.Lock()
	pulled := a.resticPulled
	a.Mu.Unlock()
	if !pulled {
		if err := a.Docker.ImagePull(ctx, a.Cfg.ResticImage, ""); err != nil {
			return "", fmt.Errorf("pull restic image: %w", err)
		}
		a.Mu.Lock()
		a.resticPulled = true
		a.Mu.Unlock()
	}
	mounts := []docker.Mount{{Type: "volume", Source: volume, Target: "/data", ReadOnly: !write}}
	network := "none"
	repo := a.Cfg.BackupRestURL + "/" + orgID
	if a.Cfg.BackupRestURL == "" {
		mounts = append(mounts, docker.Mount{Type: "volume", Source: a.Cfg.BackupVolume, Target: "/repos"})
		repo = "/repos/" + orgID
	} else {
		network = a.Cfg.BackupNetwork
	}
	caps := []string{"DAC_READ_SEARCH"}
	if write {
		caps = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "DAC_READ_SEARCH"}
	}
	env := append([]string{
		"RESTIC_REPOSITORY=" + repo, "RESTIC_PASSWORD=" + key, "RESTIC_CACHE_DIR=/tmp/cache",
		"HOME=/tmp", "SVC=" + host, "KEEP=" + a.Cfg.BackupKeep,
	}, extraEnv...)
	cfg := docker.ContainerConfig{
		Image:      a.Cfg.ResticImage,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{script},
		Env:        env,
		Labels:     map[string]string{"platform.backup": "true", "platform.org": orgID, "platform.target": host},
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
	id, err := a.Docker.ContainerCreate(ctx, "backup-"+hex.EncodeToString(suffix), cfg)
	if err != nil {
		return "", fmt.Errorf("create backup container: %w", err)
	}
	defer a.Docker.ContainerRemove(context.Background(), id)
	if err := a.Docker.ContainerStart(ctx, id); err != nil {
		return "", fmt.Errorf("start backup container: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	code, err := a.Docker.ContainerWait(wctx, id)
	cancel()
	if err != nil {
		return "", fmt.Errorf("backup container did not finish: %w", err)
	}
	stdout, stderr, err := a.Docker.ContainerOutput(ctx, id)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("restic exited with code %d: %s", code, Truncate(lastLines(string(stderr), 5), 800))
	}
	return string(stdout), nil
}

// backupVolume snapshots volume, applies retention, and syncs the snapshot list.
func (a *Agent) BackupVolume(ctx context.Context, j BackupJob, volume, tag string) (string, error) {
	if ok, err := a.Docker.VolumeExists(ctx, volume); err != nil {
		return "", err
	} else if !ok {
		return "", errors.New("nothing to back up yet: there is no data (deploy or start it first)")
	}
	if tag == "" {
		tag = "manual"
	}
	host, _ := j.Target()
	script := `set -e
restic cat config >/dev/null 2>&1 || restic init >/dev/null
echo @@BACKUP
restic backup /data --host "$SVC" --tag "$TAG" --json --quiet
echo @@FORGET
restic forget --host "$SVC" --keep-tag pre-restore $KEEP --prune --quiet >/dev/null
echo @@SNAPSHOTS
restic snapshots --host "$SVC" --json`
	out, err := a.ResticRun(ctx, j.OrgID, host, volume, script, false, "TAG="+tag)
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
		Short(summary.SnapshotID), summary.Files, humanBytes(summary.Bytes), humanBytes(summary.Added)), nil
}

func (a *Agent) syncSnapshots(ctx context.Context, j BackupJob, raw, newID string, size, files int64) error {
	var snaps []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
		Tags []string  `json:"tags"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &snaps); err != nil {
		return fmt.Errorf("read snapshot list: %w", err)
	}
	host, col := j.Target() // col is one of two constants, never user input
	return pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		ids := make([]string, 0, len(snaps))
		for _, s := range snaps {
			ids = append(ids, s.ID)
			var sz, fl int64
			if s.ID == newID {
				sz, fl = size, files
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO backup_snapshots (org_id, `+col+`, snapshot_id, taken_at, size_bytes, files, tags)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (org_id, snapshot_id) DO UPDATE SET tags = EXCLUDED.tags`,
				j.OrgID, host, s.ID, s.Time, sz, fl, strings.Join(s.Tags, ",")); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM backup_snapshots WHERE `+col+` = $1 AND NOT (snapshot_id = ANY($2))`, host, ids)
		return err
	})
}

func (a *Agent) SnapshotBelongs(ctx context.Context, j BackupJob) bool {
	host, col := j.Target()
	var known bool
	err := a.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM backup_snapshots WHERE `+col+` = $1 AND snapshot_id LIKE $2 || '%')`,
		host, j.Snapshot).Scan(&known)
	return err == nil && known && SnapshotRe.MatchString(j.Snapshot)
}

func (a *Agent) RestoreVolume(ctx context.Context, j BackupJob, volume string) error {
	host, _ := j.Target()
	script := `set -e
find /data -mindepth 1 -delete
restic restore "$SNAP" --host "$SVC" --target /`
	_, err := a.ResticRun(ctx, j.OrgID, host, volume, script, true, "SNAP="+j.Snapshot)
	return err
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

func Short(id string) string {
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

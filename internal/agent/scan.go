package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/docker"
	"platform/internal/registry"
)

// Vulnerability scanning with Trivy. Each scan is a short-lived container: no Docker
// socket, read-only filesystem, no capabilities, its own memory and PID limits. It
// fetches the image straight from its registry, with org-scoped credentials when the
// image lives on the platform registry, and asks the Trivy server (which holds the
// vulnerability database) for matches.

type finding struct {
	ID        string `json:"id"`
	Pkg       string `json:"pkg"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed"`
	Severity  string `json:"severity"`
	Title     string `json:"title"`
}

type scanResult struct {
	ImageID                     string
	Critical, High, Medium, Low int
	Findings                    []finding
}

var severityRank = map[string]int{"CRITICAL": 0, "HIGH": 1, "MEDIUM": 2, "LOW": 3}

// pullAuth returns X-Registry-Auth for platform-registry images, scoped to one org.
func (a *Agent) pullAuth(image, orgSlug string) string {
	if !registry.IsPlatformImage(image, a.cfg.RegistryHost) || a.cfg.RegistryInternalSecret == "" {
		return ""
	}
	return docker.RegistryAuth(registry.AgentUserPrefix+orgSlug,
		registry.InternalPassword(a.cfg.RegistryInternalSecret, orgSlug), a.cfg.RegistryHost)
}

func (a *Agent) runTrivy(ctx context.Context, image, orgSlug string) (*scanResult, error) {
	a.mu.Lock()
	pulled := a.trivyPulled
	a.mu.Unlock()
	if !pulled {
		if err := a.dk.ImagePull(ctx, a.cfg.TrivyImage, ""); err != nil {
			return nil, fmt.Errorf("pull scanner image: %w", err)
		}
		a.mu.Lock()
		a.trivyPulled = true
		a.mu.Unlock()
	}
	env := []string{"HOME=/tmp", "TRIVY_SERVER=" + a.cfg.TrivyServerURL, "TRIVY_CACHE_DIR=/tmp/trivy", "TRIVY_QUIET=true"}
	if a.cfg.TrivyToken != "" {
		env = append(env, "TRIVY_TOKEN="+a.cfg.TrivyToken)
	}
	if registry.IsPlatformImage(image, a.cfg.RegistryHost) {
		env = append(env, "TRIVY_USERNAME="+registry.AgentUserPrefix+orgSlug,
			"TRIVY_PASSWORD="+registry.InternalPassword(a.cfg.RegistryInternalSecret, orgSlug))
	}
	if a.cfg.ScannerInsecure {
		env = append(env, "TRIVY_INSECURE=true")
	}
	cfg := docker.ContainerConfig{
		Image:  a.cfg.TrivyImage,
		Cmd:    []string{"image", "--format", "json", "--scanners", "vuln", "--timeout", "5m", "--image-src", "remote", image},
		Env:    env,
		Labels: map[string]string{"platform.scanner": "true"},
		HostConfig: docker.HostConfig{
			NetworkMode:    a.cfg.ScannerNetwork,
			Memory:         1536 << 20,
			MemorySwap:     1536 << 20,
			NanoCPUs:       1_000_000_000,
			PidsLimit:      256,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
			Tmpfs:          map[string]string{"/tmp": "rw,size=1g"},
			LogConfig:      docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "64m"}},
		},
	}
	suffix := make([]byte, 5)
	_, _ = rand.Read(suffix)
	id, err := a.dk.ContainerCreate(ctx, "scan-"+hex.EncodeToString(suffix), cfg)
	if err != nil {
		return nil, fmt.Errorf("create scanner: %w", err)
	}
	defer a.dk.ContainerRemove(context.Background(), id)
	if err := a.dk.ContainerStart(ctx, id); err != nil {
		return nil, fmt.Errorf("start scanner: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	code, err := a.dk.ContainerWait(wctx, id)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("scan did not finish: %w", err)
	}
	stdout, stderr, err := a.dk.ContainerOutput(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read scan output: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("scanner exited with code %d: %s", code, truncate(lastLines(string(stderr), 4), 600))
	}
	return parseTrivy(stdout)
}

func parseTrivy(out []byte) (*scanResult, error) {
	var rep struct {
		Metadata struct {
			ImageID string `json:"ImageID"`
		} `json:"Metadata"`
		Results []struct {
			Vulnerabilities []struct {
				VulnerabilityID  string `json:"VulnerabilityID"`
				PkgName          string `json:"PkgName"`
				InstalledVersion string `json:"InstalledVersion"`
				FixedVersion     string `json:"FixedVersion"`
				Severity         string `json:"Severity"`
				Title            string `json:"Title"`
			} `json:"Vulnerabilities"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("unreadable scan report: %w", err)
	}
	res := &scanResult{ImageID: rep.Metadata.ImageID}
	seen := map[string]bool{}
	for _, r := range rep.Results {
		for _, v := range r.Vulnerabilities {
			key := v.VulnerabilityID + "|" + v.PkgName + "|" + v.InstalledVersion
			if seen[key] {
				continue
			}
			seen[key] = true
			switch v.Severity {
			case "CRITICAL":
				res.Critical++
			case "HIGH":
				res.High++
			case "MEDIUM":
				res.Medium++
			case "LOW":
				res.Low++
			}
			res.Findings = append(res.Findings, finding{
				ID: v.VulnerabilityID, Pkg: v.PkgName, Installed: v.InstalledVersion,
				Fixed: v.FixedVersion, Severity: v.Severity, Title: truncate(v.Title, 160),
			})
		}
	}
	sort.SliceStable(res.Findings, func(i, j int) bool {
		ri, ok := severityRank[res.Findings[i].Severity]
		if !ok {
			ri = 9
		}
		rj, ok := severityRank[res.Findings[j].Severity]
		if !ok {
			rj = 9
		}
		if ri != rj {
			return ri < rj
		}
		return res.Findings[i].ID < res.Findings[j].ID
	})
	if len(res.Findings) > 100 {
		res.Findings = res.Findings[:100]
	}
	return res, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func (a *Agent) storeScan(ctx context.Context, id string, res *scanResult, scanErr error) {
	if scanErr != nil {
		_, _ = a.pool.Exec(ctx, `UPDATE image_scans SET status = 'error', error = $2, scanned_at = now() WHERE id = $1`,
			id, truncate(scanErr.Error(), 1000))
		return
	}
	findings, _ := json.Marshal(res.Findings)
	if res.Findings == nil {
		findings = []byte("[]")
	}
	_, err := a.pool.Exec(ctx, `
		UPDATE image_scans SET status = 'done', error = '', image_id = CASE WHEN $7 <> '' THEN $7 ELSE image_id END,
		       critical = $2, high = $3, medium = $4, low = $5, findings = $6, scanned_at = now()
		WHERE id = $1`, id, res.Critical, res.High, res.Medium, res.Low, findings, res.ImageID)
	if err != nil {
		a.log.Error("store scan", "scan", id, "err", err)
	}
}

// scanForDeploy reuses a successful scan of the same image (by content ID) from the last
// 24 hours, otherwise scans now. It always returns the scan row ID when one exists.
func (a *Agent) scanForDeploy(ctx context.Context, j job, imageID string) (string, *scanResult, error) {
	var id string
	var raw []byte
	res := &scanResult{ImageID: imageID}
	err := a.pool.QueryRow(ctx, `
		SELECT id, critical, high, medium, low, findings FROM image_scans
		WHERE org_id = $1 AND image_id = $2 AND $2 <> '' AND status = 'done' AND scanned_at > now() - interval '24 hours'
		ORDER BY scanned_at DESC LIMIT 1`, j.OrgID, imageID).Scan(&id, &res.Critical, &res.High, &res.Medium, &res.Low, &raw)
	if err == nil {
		_ = json.Unmarshal(raw, &res.Findings)
		return id, res, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, err
	}
	if err := a.pool.QueryRow(ctx, `
		INSERT INTO image_scans (org_id, image, image_id, source, status) VALUES ($1, $2, $3, 'deploy', 'running')
		RETURNING id`, j.OrgID, j.Spec.Image, imageID).Scan(&id); err != nil {
		return "", nil, err
	}
	scanned, serr := a.runTrivy(ctx, j.Spec.Image, j.OrgSlug)
	a.storeScan(ctx, id, scanned, serr)
	return id, scanned, serr
}

// gate scans the image of a deployment and applies the organization's policy.
func (a *Agent) gate(ctx context.Context, j job) error {
	a.setMessage(ctx, j.ID, "scanning image for known vulnerabilities")
	imageID, err := a.dk.ImageID(ctx, j.Spec.Image)
	if err != nil {
		imageID = ""
	}
	scanID, res, serr := a.scanForDeploy(ctx, j, imageID)
	if scanID != "" {
		_, _ = a.pool.Exec(ctx, `UPDATE deployments SET scan_id = $2 WHERE id = $1`, j.ID, scanID)
	}
	block := j.ScanPolicy == "block-critical"
	if serr != nil {
		if block {
			return fmt.Errorf("the vulnerability scan failed and this organization only deploys scanned images: %w", serr)
		}
		a.log.Warn("scan failed, deploying anyway (policy: warn)", "deployment", j.ID, "err", serr)
		return nil
	}
	if block && res.Critical > 0 {
		var ids []string
		for _, f := range res.Findings {
			if f.Severity == "CRITICAL" && len(ids) < 5 {
				ids = append(ids, f.ID+" ("+f.Pkg+")")
			}
		}
		return fmt.Errorf("blocked by security policy: %d critical vulnerabilities: %s. Rebuild on an updated base image",
			res.Critical, strings.Join(ids, ", "))
	}
	return nil
}

// ---------- background scans: pushes and daily rescans ----------

func (a *Agent) scanLoop(ctx context.Context) {
	if !a.cfg.ScannerEnabled || !a.isPrimary() {
		return
	}
	_, _ = a.pool.Exec(ctx, `UPDATE image_scans SET status = 'queued' WHERE status = 'running' AND source <> 'deploy'`)
	_, _ = a.pool.Exec(ctx, `UPDATE image_scans SET status = 'error', error = 'interrupted by agent restart' WHERE status = 'running'`)
	tick := time.NewTicker(time.Minute)
	daily := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	defer daily.Stop()
	for {
		a.processScans(ctx)
		select {
		case <-ctx.Done():
			return
		case <-a.scanWake:
		case <-tick.C:
		case <-daily.C:
			// New vulnerabilities are published every day: rescan what is running.
			if _, err := a.pool.Exec(ctx, `
				INSERT INTO image_scans (org_id, image, source)
				SELECT DISTINCT d.org_id, d.spec->>'image', 'rescan' FROM deployments d WHERE d.status = 'live'`); err != nil {
				a.log.Error("queue rescans", "err", err)
			}
		}
	}
}

func (a *Agent) processScans(ctx context.Context) {
	for ctx.Err() == nil {
		var id, image, slug string
		found := false
		err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `
				SELECT s.id, s.image, o.slug FROM image_scans s JOIN orgs o ON o.id = s.org_id
				WHERE s.status = 'queued' ORDER BY s.created_at LIMIT 1
				FOR UPDATE OF s SKIP LOCKED`).Scan(&id, &image, &slug)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			found = true
			_, err = tx.Exec(ctx, `UPDATE image_scans SET status = 'running' WHERE id = $1`, id)
			return err
		})
		if err != nil {
			a.log.Error("claim scan", "err", err)
			return
		}
		if !found {
			return
		}
		a.log.Info("scanning image", "scan", id, "image", image)
		res, serr := a.runTrivy(ctx, image, slug)
		a.storeScan(ctx, id, res, serr)
		if serr == nil && res.Critical > 0 {
			a.alertRunningImage(ctx, image, res.Critical)
		}
	}
}

func (a *Agent) pokeScans() {
	select {
	case a.scanWake <- struct{}{}:
	default:
	}
}

// alertRunningImage notifies every service currently running image about critical CVEs.
func (a *Agent) alertRunningImage(ctx context.Context, image string, critical int) {
	rows, err := a.pool.Query(ctx, `
		SELECT d.org_id, d.service_id FROM deployments d
		WHERE d.status = 'live' AND d.spec->>'image' = $1`, image)
	if err != nil {
		return
	}
	type svc struct{ OrgID, ServiceID string }
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[svc])
	if err != nil {
		return
	}
	for _, s := range list {
		a.alertEvent(ctx, s.OrgID, s.ServiceID, "critical_vulns",
			fmt.Sprintf("%d critical vulnerabilities found in the running image %s. Rebuild on an updated base image and redeploy.", critical, image))
	}
}

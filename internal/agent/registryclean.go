package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"platform/internal/registry"
)

// Registry cleanup, once a day: a tag in an organization's namespace is deleted when
//   - no deployment among the last 20 revisions of any service uses it,
//   - no service is currently set to it, and
//   - it was not pushed or built in the last REGISTRY_KEEP_DAYS days.
// A tag that shares its manifest with a kept tag is never deleted (deleting a manifest
// removes every tag pointing at it). Freed layers are reclaimed by the weekly
// garbage collection on the host (host/registry-gc.sh).

func (a *Agent) pokeRegistryClean() {
	select {
	case a.cleanWake <- struct{}{}:
	default:
	}
}

func (a *Agent) registryCleanLoop(ctx context.Context) {
	if a.cfg.RegistryHost == "" || a.cfg.RegistryInternalSecret == "" || !a.isPrimary() {
		return
	}
	timer := time.NewTimer(10 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-a.cleanWake:
		}
		a.cleanRegistry(ctx)
		timer.Reset(24 * time.Hour)
	}
}

func (a *Agent) cleanRegistry(ctx context.Context) {
	prefix := a.cfg.RegistryHost + "/"
	rows, err := a.pool.Query(ctx, `
		WITH images AS (
			SELECT org_id, image, created_at FROM image_scans WHERE source = 'push'
			UNION ALL SELECT org_id, image, created_at FROM builds WHERE image <> ''
		), recent AS (
			SELECT d.spec->>'image' AS image FROM (
				SELECT spec, row_number() OVER (PARTITION BY service_id ORDER BY revision DESC) AS n FROM deployments
			) d WHERE d.n <= 20
			UNION SELECT image FROM services
		)
		SELECT o.id, o.slug, i.image, max(i.created_at) > now() - make_interval(days => $2) OR i.image IN (SELECT image FROM recent) AS keep
		FROM images i JOIN orgs o ON o.id = i.org_id
		WHERE i.image LIKE $1 || '%'
		GROUP BY o.id, o.slug, i.image`, prefix, a.cfg.RegistryKeepDays)
	if err != nil {
		a.log.Warn("registry cleanup", "err", err)
		return
	}
	type tagRef struct {
		orgID, slug, repo, tag string
		keep                   bool
	}
	byRepo := map[string][]tagRef{}
	for rows.Next() {
		var t tagRef
		var image string
		if rows.Scan(&t.orgID, &t.slug, &image, &t.keep) != nil {
			continue
		}
		ref := strings.TrimPrefix(image, prefix)
		i := strings.LastIndex(ref, ":")
		if i < 0 || strings.Contains(ref, "@") || !strings.HasPrefix(ref, t.slug+"/") {
			continue // digests, untagged or other namespaces: leave alone
		}
		t.repo, t.tag = ref[:i], ref[i+1:]
		byRepo[t.repo] = append(byRepo[t.repo], t)
	}
	rows.Close()

	deleted := map[string]int{} // org ID -> tags deleted
	for repo, tags := range byRepo {
		slug := tags[0].slug
		token, err := a.registryToken(ctx, slug, repo)
		if err != nil {
			a.log.Warn("registry cleanup token", "repo", repo, "err", err)
			continue
		}
		keptDigests := map[string]bool{}
		for _, t := range tags {
			if t.keep {
				if d, _ := a.manifestDigest(ctx, token, repo, t.tag); d != "" {
					keptDigests[d] = true
				}
			}
		}
		for _, t := range tags {
			if t.keep {
				continue
			}
			d, err := a.manifestDigest(ctx, token, repo, t.tag)
			if err != nil || d == "" || keptDigests[d] {
				continue // already gone, or shared with a kept tag
			}
			if err := a.deleteManifest(ctx, token, repo, d); err != nil {
				a.log.Warn("registry delete", "repo", repo, "tag", t.tag, "err", err)
				continue
			}
			deleted[t.orgID]++
			a.log.Info("registry tag deleted", "repo", repo, "tag", t.tag)
		}
	}
	for orgID, n := range deleted {
		_ = auditAgent(ctx, a, orgID, "registry.cleaned", "registry", map[string]any{"tags_deleted": n, "keep_days": a.cfg.RegistryKeepDays})
	}
}

// registryToken asks the API's token endpoint for pull+delete on one repository, as
// "builder/<org>" (scoped to that organization's namespace).
func (a *Agent) registryToken(ctx context.Context, slug, repo string) (string, error) {
	q := url.Values{"service": {registry.Service}, "scope": {"repository:" + repo + ":pull,delete"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.cfg.APIUpstream+"/token?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(registry.BuilderUserPrefix+slug, registry.BuilderPassword(a.cfg.RegistryInternalSecret, slug))
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Token == "" {
		return "", fmt.Errorf("token endpoint: HTTP %d", resp.StatusCode)
	}
	return out.Token, nil
}

var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// manifestDigest returns the digest a tag points to ("" when the tag does not exist).
func (a *Agent) manifestDigest(ctx context.Context, token, repo, tag string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+a.cfg.RegistryUpstream+"/v2/"+repo+"/manifests/"+url.PathEscape(tag), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", manifestTypes)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Header.Get("Docker-Content-Digest"), nil
}

func (a *Agent) deleteManifest(ctx context.Context, token, repo, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+a.cfg.RegistryUpstream+"/v2/"+repo+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

package agent

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/garage"
	"platform/internal/s3"
)

// Object storage upkeep, on the primary (Garage runs on the control plane):
//   - storage_trash: empty and delete removed buckets, delete removed access keys;
//   - each bucket: refresh its usage, keep its quota, website flag and key permissions
//     as in the database; a suspended organization's keys lose all access and its
//     websites go offline until it is reactivated;
//   - record the stored bytes of each bucket for the current hour (the last sample of
//     the hour counts), for invoices.

func (a *Agent) pokeStorage() {
	select {
	case a.storageWake <- struct{}{}:
	default:
	}
}

func (a *Agent) storageLoop(ctx context.Context) {
	if !a.cfg.StorageEnabled() || !a.isPrimary() {
		return
	}
	g := garage.New(a.cfg.GarageAdminURL, a.cfg.GarageAdminToken)
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for {
		a.emptyStorageTrash(ctx, g)
		a.syncBuckets(ctx, g)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.storageWake:
		}
	}
}

type trashItem struct {
	ID                        int64
	Kind, GarageID, ConsoleID string
	ConsoleSecretEnc          []byte
	SealContext               string
}

func (a *Agent) emptyStorageTrash(ctx context.Context, g *garage.Client) {
	rows, err := a.pool.Query(ctx, `
		SELECT id, kind, garage_id, console_key_id, console_secret_enc, seal_context FROM storage_trash
		WHERE attempts < 20 AND (attempts = 0 OR created_at < now() - make_interval(mins => attempts)) ORDER BY id LIMIT 50`)
	if err != nil {
		a.log.Warn("load storage trash", "err", err)
		return
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[trashItem])
	if err != nil {
		return
	}
	for _, it := range items {
		err := a.removeFromGarage(ctx, g, it)
		if err != nil {
			a.log.Warn("remove from object storage", "kind", it.Kind, "id", it.GarageID, "err", err)
			_, _ = a.pool.Exec(ctx, `UPDATE storage_trash SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, it.ID, truncate(err.Error(), 500))
			continue
		}
		_, _ = a.pool.Exec(ctx, `DELETE FROM storage_trash WHERE id = $1`, it.ID)
		a.log.Info("removed from object storage", "kind", it.Kind, "id", it.GarageID)
	}
}

func (a *Agent) removeFromGarage(ctx context.Context, g *garage.Client, it trashItem) error {
	if it.Kind == "key" {
		if err := g.DeleteKey(ctx, it.GarageID); err != nil && !garage.IsNotFound(err) {
			return err
		}
		return nil
	}
	b, err := g.GetBucket(ctx, it.GarageID)
	if garage.IsNotFound(err) {
		return a.deleteConsoleKey(ctx, g, it)
	}
	if err != nil {
		return err
	}
	if b.Objects > 0 || b.Bytes > 0 { // empty it with the bucket's console key
		secret, err := a.box.Open(it.ConsoleSecretEnc, it.SealContext)
		if err != nil {
			return err
		}
		// The console key may have lost its access (a suspended organization).
		if err := g.SetPerms(ctx, b.ID, it.ConsoleID, garage.Perms{Read: true, Write: true}); err != nil {
			return err
		}
		cl := s3.NewClient("http://"+a.cfg.S3Upstream, s3.Credentials{AccessKey: it.ConsoleID, SecretKey: string(secret), Region: a.cfg.S3Region})
		name := it.GarageID
		if len(b.GlobalAliases) > 0 {
			name = b.GlobalAliases[0]
		}
		if _, err := cl.Empty(ctx, name, 100000); err != nil {
			return err
		}
	}
	if err := g.DeleteBucket(ctx, b.ID); err != nil && !garage.IsNotFound(err) {
		return err
	}
	return a.deleteConsoleKey(ctx, g, it)
}

func (a *Agent) deleteConsoleKey(ctx context.Context, g *garage.Client, it trashItem) error {
	if it.ConsoleID == "" {
		return nil
	}
	if err := g.DeleteKey(ctx, it.ConsoleID); err != nil && !garage.IsNotFound(err) {
		return err
	}
	return nil
}

type bucketRow struct {
	ID, OrgID, FullName, GarageID, IndexDocument, ErrorDocument string
	QuotaMB                                                     int
	Website, Suspended                                          bool
	ConsoleKeyID                                                string
}

func (a *Agent) syncBuckets(ctx context.Context, g *garage.Client) {
	rows, err := a.pool.Query(ctx, `
		SELECT b.id, b.org_id, b.full_name, b.garage_id, b.index_document, b.error_document, b.quota_mb, b.website, o.suspended, b.console_key_id
		FROM buckets b JOIN orgs o ON o.id = b.org_id ORDER BY b.created_at`)
	if err != nil {
		a.log.Warn("load buckets", "err", err)
		return
	}
	buckets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[bucketRow])
	if err != nil {
		return
	}
	// Wanted permissions per bucket: key ID -> write allowed.
	grows, err := a.pool.Query(ctx, `SELECT g.bucket_id, k.access_key_id, g.can_write FROM storage_grants g JOIN storage_keys k ON k.id = g.key_id`)
	if err != nil {
		return
	}
	wanted := map[string]map[string]bool{}
	for grows.Next() {
		var bid, kid string
		var write bool
		if grows.Scan(&bid, &kid, &write) == nil {
			if wanted[bid] == nil {
				wanted[bid] = map[string]bool{}
			}
			wanted[bid][kid] = write
		}
	}
	grows.Close()
	hour := time.Now().UTC().Truncate(time.Hour)
	for _, b := range buckets {
		info, err := g.GetBucket(ctx, b.GarageID)
		if err != nil {
			a.log.Warn("bucket info", "bucket", b.FullName, "err", err)
			continue
		}
		_, _ = a.pool.Exec(ctx, `UPDATE buckets SET objects = $2, bytes = $3, usage_at = now() WHERE id = $1`, b.ID, info.Objects, info.Bytes)
		if _, err := a.pool.Exec(ctx, `
			INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, storage_bytes) VALUES ($1, $2, 'bucket', $3, $4, $5)
			ON CONFLICT (resource_id, hour) DO UPDATE SET storage_bytes = EXCLUDED.storage_bytes`,
			b.OrgID, b.ID, b.FullName, hour, info.Bytes); err != nil {
			a.log.Warn("meter storage", "bucket", b.FullName, "err", err)
		}
		quota := int64(b.QuotaMB) << 20
		if info.Quotas.MaxSize == nil || *info.Quotas.MaxSize != quota {
			if err := g.SetQuota(ctx, b.GarageID, quota); err != nil {
				a.log.Warn("bucket quota", "bucket", b.FullName, "err", err)
			}
		}
		website := b.Website && !b.Suspended
		if info.WebsiteAccess != website {
			if err := g.SetWebsite(ctx, b.GarageID, website, b.IndexDocument, b.ErrorDocument); err != nil {
				a.log.Warn("bucket website", "bucket", b.FullName, "err", err)
			}
		}
		want := map[string]garage.Perms{}
		if !b.Suspended {
			want[b.ConsoleKeyID] = garage.Perms{Read: true, Write: true}
			for kid, write := range wanted[b.ID] {
				want[kid] = garage.Perms{Read: true, Write: write}
			}
		}
		have := map[string]garage.Perms{}
		for _, k := range info.Keys {
			have[k.AccessKeyID] = k.Permissions
		}
		for kid, p := range want {
			if have[kid] != p {
				if err := g.SetPerms(ctx, b.GarageID, kid, p); err != nil {
					a.log.Warn("bucket key access", "bucket", b.FullName, "key", kid, "err", err)
				}
			}
		}
		for kid, p := range have { // keys that should not reach this bucket (any more)
			if _, ok := want[kid]; !ok && (p.Read || p.Write || p.Owner) {
				if err := g.SetPerms(ctx, b.GarageID, kid, garage.Perms{}); err != nil {
					a.log.Warn("bucket key access", "bucket", b.FullName, "key", kid, "err", err)
				}
			}
		}
	}
}

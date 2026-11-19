package agent

import (
	"context"
	"strings"
	"time"

	"platform/internal/model"
)

// The janitor frees the disk space nothing reclaims otherwise, once a day on each node:
//   - build caches (pbcache-*) of services that no longer exist, at once (only caches);
//   - data volumes of deleted services (pv-*) and databases (pvdb-*, pbdb-*), once they
//     have been without an owner for ORPHAN_VOLUME_DAYS (default 7), so an operator can
//     still bring a deletion back for a while;
//   - Docker's build cache beyond its 4 GB used most recently, and untagged images no
//     container uses.
// A volume a container still uses is never touched.

func (a *Agent) janitorLoop(ctx context.Context) {
	timer := time.NewTimer(10 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		a.cleanVolumes(ctx)
		timer.Reset(24 * time.Hour)
	}
}

// owners returns the compact IDs of the existing services and databases.
func (a *Agent) owners(ctx context.Context) (services, dbs map[string]bool, err error) {
	services, dbs = map[string]bool{}, map[string]bool{}
	for q, m := range map[string]map[string]bool{`SELECT id::text FROM services`: services, `SELECT id::text FROM databases`: dbs} {
		rows, err := a.pool.Query(ctx, q)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				m[model.Compact(id)] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return services, dbs, nil
}

func (a *Agent) cleanVolumes(ctx context.Context) {
	vols, err := a.dk.VolumeUsages(ctx)
	if err != nil {
		a.log.Warn("janitor: volumes", "err", err)
		return
	}
	services, dbs, err := a.owners(ctx)
	if err != nil || len(services)+len(dbs) == 0 { // never act on an empty or failed read
		return
	}
	node, _ := a.nodeID(ctx)
	svcPrefix := func(p string) bool { // build caches are named after the first 12 characters
		for id := range services {
			if strings.HasPrefix(id, p) {
				return true
			}
		}
		return false
	}
	var freed int64
	orphans := map[string]int64{}
	for name, u := range vols {
		if u.RefCount > 0 {
			continue
		}
		switch {
		case strings.HasPrefix(name, "pbcache-"):
			if !svcPrefix(strings.TrimPrefix(name, "pbcache-")) && a.dk.VolumeRemove(ctx, name) == nil {
				freed += u.Size
			}
		case strings.HasPrefix(name, "pv-") && len(name) == 35 && !services[strings.TrimPrefix(name, "pv-")]:
			orphans[name] = u.Size
		case (strings.HasPrefix(name, "pvdb-") || strings.HasPrefix(name, "pbdb-")) && len(name) == 37 && !dbs[name[5:]]:
			orphans[name] = u.Size
		}
	}
	// note new orphans, forget those that have an owner again, remove the expired ones
	for name, size := range orphans {
		_, _ = a.pool.Exec(ctx, `INSERT INTO orphan_volumes (node_id, name, size_bytes) VALUES ($1, $2, $3)
			ON CONFLICT (node_id, name) DO UPDATE SET size_bytes = EXCLUDED.size_bytes`, node, name, size)
	}
	rows, err := a.pool.Query(ctx, `SELECT name, first_seen < now() - make_interval(days => $2) FROM orphan_volumes WHERE node_id = $1`, node, max(a.cfg.OrphanVolumeDays, 1))
	if err != nil {
		return
	}
	type entry struct {
		name    string
		expired bool
	}
	var list []entry
	for rows.Next() {
		var e entry
		if rows.Scan(&e.name, &e.expired) == nil {
			list = append(list, e)
		}
	}
	rows.Close()
	removed := 0
	for _, e := range list {
		_, still := orphans[e.name]
		switch {
		case !still: // gone, or in use / owned again
			_, _ = a.pool.Exec(ctx, `DELETE FROM orphan_volumes WHERE node_id = $1 AND name = $2`, node, e.name)
		case e.expired:
			if a.dk.VolumeRemove(ctx, e.name) == nil {
				freed += orphans[e.name]
				removed++
				_, _ = a.pool.Exec(ctx, `DELETE FROM orphan_volumes WHERE node_id = $1 AND name = $2`, node, e.name)
			}
		}
	}
	if n, err := a.dk.BuildCachePrune(ctx, 4<<30); err == nil { // keep the 4 GB used most recently
		freed += n
	}
	if n, err := a.dk.ImagePruneDangling(ctx); err == nil {
		freed += n
	}
	if freed > 0 || removed > 0 {
		a.log.Info("janitor: disk space freed", "mb", freed>>20, "volumes_of_deleted_resources", removed)
	}
}

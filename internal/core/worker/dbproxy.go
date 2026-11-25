package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"platform/internal/lib/docker"
	"platform/internal/lib/model"
)

// Public endpoints of databases opened to the internet: one small proxy container per
// database (the platform's own image, mode "dbproxy"), on the project network, with the
// database's public port published. It lets in only the allowed addresses and ends TLS
// (see internal/dbproxy). Closing public access removes it.

func dbProxyName(id string) string { return "pdx-" + model.Compact(id)[:12] }

func (a *Agent) syncDBProxies(ctx context.Context) {
	type pub struct {
		ID, OrgID, ProjectID, Engine, Status, ContainerID, Cert string
		Public                                                  bool
		Allow                                                   []string
		KeyEnc                                                  []byte
		Port                                                    int
	}
	rows, err := a.Pool.Query(ctx, `
		SELECT d.id, d.org_id, d.project_id, d.engine, d.status, coalesce(d.container_id, ''), d.proxy_cert,
		       d.public, d.allow_cidrs, d.proxy_key_enc, coalesce(pp.public_port, 0)
		FROM databases d JOIN projects p ON p.id = d.project_id JOIN orgs o ON o.id = d.org_id
		LEFT JOIN public_ports pp ON pp.database_id = d.id
		WHERE NOT o.suspended AND `+MineSQL, a.MineArgs()...)
	if err != nil {
		return
	}
	var list []pub
	for rows.Next() {
		var p pub
		if rows.Scan(&p.ID, &p.OrgID, &p.ProjectID, &p.Engine, &p.Status, &p.ContainerID, &p.Cert, &p.Public, &p.Allow, &p.KeyEnc, &p.Port) == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	existing, err := a.Docker.ContainerList(ctx, "platform.dbproxy")
	if err != nil {
		return
	}
	have := map[string]docker.ContainerSummary{}
	for _, c := range existing {
		if c.Labels["platform.node"] == a.SelfID {
			have[c.Labels["platform.dbproxy"]] = c
		}
	}
	keep := map[string]bool{}
	for _, d := range list {
		if !d.Public || d.Status != "running" || d.ContainerID == "" || d.Port == 0 || d.Cert == "" || len(d.KeyEnc) == 0 || len(d.Allow) == 0 {
			continue
		}
		keep[d.ID] = true
		netName := model.NetworkName(d.ProjectID)
		info, err := a.Docker.ContainerInspect(ctx, d.ContainerID)
		if err != nil {
			continue
		}
		ep, ok := info.NetworkSettings.Networks[netName]
		if !ok || ep.IPAddress == "" {
			continue
		}
		upstream := ep.IPAddress + ":" + strconv.Itoa(model.EngineInfo(d.Engine).Port)
		sum := sha256.Sum256([]byte(strings.Join([]string{upstream, strconv.Itoa(d.Port), strings.Join(d.Allow, ","), d.Cert, a.Cfg.DBProxyImage}, "|")))
		hash := hex.EncodeToString(sum[:8])
		if c, ok := have[d.ID]; ok {
			if c.Labels["platform.spec"] == hash && c.State == "running" {
				continue
			}
			_ = a.Docker.ContainerRemove(ctx, c.ID)
		}
		key, err := a.Box.Open(d.KeyEnc, "dbproxy:"+d.ID)
		if err != nil {
			a.Log.Warn("database endpoint key", "database", d.ID, "err", err)
			continue
		}
		name := dbProxyName(d.ID)
		_ = a.Docker.ContainerRemove(ctx, name)
		hc := docker.HostConfig{
			Runtime: a.Cfg.Runtime, NetworkMode: netName,
			Memory: 64 << 20, MemorySwap: 64 << 20, NanoCPUs: 250_000_000, PidsLimit: 64,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, ReadonlyRootfs: true,
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			PortBindings:  map[string][]docker.PortBinding{"6000/tcp": {{HostIP: "0.0.0.0", HostPort: strconv.Itoa(d.Port)}}},
			LogConfig:     docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "5m", "max-file": "2"}},
		}
		id, err := a.Docker.ContainerCreate(ctx, name, docker.ContainerConfig{
			Image: a.Cfg.DBProxyImage, Cmd: []string{"dbproxy"},
			Env: []string{"DBPROXY_LISTEN=:6000", "DBPROXY_UPSTREAM=" + upstream, "DBPROXY_ENGINE=" + d.Engine,
				"DBPROXY_ALLOW=" + strings.Join(d.Allow, ","), "DBPROXY_CERT=" + d.Cert, "DBPROXY_KEY=" + string(key)},
			ExposedPorts: map[string]struct{}{"6000/tcp": {}},
			Labels: map[string]string{"platform.dbproxy": d.ID, "platform.org": d.OrgID, "platform.project": d.ProjectID,
				"platform.node": a.SelfID, "platform.spec": hash},
			HostConfig: hc,
		})
		if err != nil {
			a.Log.Warn("database endpoint", "database", d.ID, "err", err)
			continue
		}
		if err := a.Docker.ContainerStart(ctx, id); err != nil {
			a.Log.Warn("database endpoint start", "database", d.ID, "err", err)
			_ = a.Docker.ContainerRemove(ctx, id)
			continue
		}
		a.Log.Info("database endpoint started", "database", d.ID, "port", d.Port)
	}
	for dbID, c := range have {
		if !keep[dbID] {
			_ = a.Docker.ContainerRemove(ctx, c.ID)
			a.Log.Info("database endpoint removed", "database", dbID)
		}
	}
}

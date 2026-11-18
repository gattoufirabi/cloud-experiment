package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
)

// Project map: what a project is made of and how the pieces connect, drawn on the
// server as SVG (no client library) and refreshed with htmx while something changes.
//
// Columns: where traffic comes from (the internet, custom domains, schedules, webhooks,
// connected projects), what runs (services, workers, functions), and the data it uses
// (databases, queues, buckets, email). Links come from what the platform knows without
// reading any secret value: secrets it wrote itself (secrets.source), host names that
// appear in plain environment variables (database hosts, private names, public
// addresses), domains, jobs, function triggers and project connections.

type mapNode struct {
	ID, Kind, Label, Sub, Status, Href string
	Col, Row                           int
	X, Y                               int // top-left corner (computed)
	DotX, DotY, TX, TY, SX, SY         int // status dot, label and subtitle positions
	Short, ShortSub                    string
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

type mapEdge struct {
	From, To, Label string
	Dashed          bool
	Path            string // SVG path (computed)
	Title           string // "web → main" (tooltip)
	LX, LY          int    // label position
}

type projectMap struct {
	Nodes        []mapNode
	Edges        []mapEdge
	Width        int
	Height       int
	Busy         bool   // something is deploying or starting: poll faster
	Poll         string // htmx trigger
	Path         string // the fragment's own URL
	ColumnTitles []mapColumn
}

type mapColumn struct {
	Title string
	X     int
}

const (
	mapNodeW = 210
	mapNodeH = 52
	mapColW  = 300
	mapRowH  = 68
	mapTop   = 34
	mapLeft  = 14
)

var (
	dbHostRe       = regexp.MustCompile(`\bpd-[0-9a-f]{12}\b`)
	internalNameRe = regexp.MustCompile(`\b([a-z0-9][a-z0-9-]*)\.([a-z0-9][a-z0-9-]*)\.internal\b`)
)

// statusClass maps a platform status to one of the map's colors.
func statusClass(st string) string {
	switch st {
	case "live", "running", "ready", "active", "verified", "ok":
		return "ok"
	case "deploying", "pending", "starting", "building", "queued":
		return "busy"
	case "failed", "error", "stopped", "crashed":
		return "bad"
	}
	return "idle"
}

func (s *Server) loadProjectMap(ctx context.Context, tx pgx.Tx, o *orgInfo, p *projectInfo) (*projectMap, error) {
	m := &projectMap{}
	base := o.Path() + "/p/" + p.Slug
	byKey := map[string]int{}
	add := func(n mapNode) {
		if _, dup := byKey[n.ID]; dup {
			return
		}
		byKey[n.ID] = len(m.Nodes)
		m.Nodes = append(m.Nodes, n)
	}
	edge := func(from, to, label string, dashed bool) {
		for _, e := range m.Edges {
			if e.From == from && e.To == to {
				return
			}
		}
		m.Edges = append(m.Edges, mapEdge{From: from, To: to, Label: label, Dashed: dashed})
	}

	// --- services (with their live settings) ---
	type svcRow struct {
		ID, Slug, Status string
		Spec             []byte
		Instances        int
	}
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.slug,
		       coalesce((SELECT d.status FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), 'not deployed'),
		       coalesce((SELECT d.spec FROM deployments d WHERE d.service_id = s.id AND d.status = 'live' ORDER BY d.revision DESC LIMIT 1),
		                (SELECT d.spec FROM deployments d WHERE d.service_id = s.id ORDER BY d.revision DESC LIMIT 1), '{}'),
		       s.instances
		FROM services s WHERE s.project_id = $1 AND s.preview_of IS NULL ORDER BY s.slug`, p.ID)
	if err != nil {
		return nil, err
	}
	svcs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[svcRow])
	if err != nil {
		return nil, err
	}
	svcBySlug := map[string]string{}
	publicHost := map[string]string{} // default public host -> node ID
	envOf := map[string][]string{}    // node ID -> plain environment values
	hasWeb := false
	for _, sv := range svcs {
		var spec model.Spec
		_ = json.Unmarshal(sv.Spec, &spec)
		id := "svc:" + sv.ID
		kind, sub := "service", "web service"
		if spec.Worker {
			kind, sub = "worker", "worker"
		}
		if n := max(spec.Count(), 1); n > 1 {
			sub += fmt.Sprintf(" · %d instances", n)
		}
		if sv.Status != "live" {
			sub += " · " + sv.Status
		}
		add(mapNode{ID: id, Kind: kind, Label: sv.Slug, Sub: sub, Status: statusClass(sv.Status), Href: base + "/s/" + sv.Slug, Col: 1})
		svcBySlug[sv.Slug] = id
		if !spec.Worker {
			publicHost[model.DefaultHost(sv.Slug, p.Slug, o.Slug, s.cfg.AppsDomain)] = id
			if sv.Status == "live" {
				hasWeb = true
				edge("internet", id, "", false)
			}
		}
		for _, v := range spec.Env {
			envOf[id] = append(envOf[id], v)
		}
		if busy := statusClass(sv.Status) == "busy"; busy {
			m.Busy = true
		}
	}
	if hasWeb {
		add(mapNode{ID: "internet", Kind: "internet", Label: "Internet", Sub: "public addresses, HTTPS", Status: "idle", Col: 0})
	}

	// --- functions ---
	type fnRow struct {
		ID, Slug, Kind string
		URL, Webhook   bool
		Env            []byte
	}
	rows, err = tx.Query(ctx, `
		SELECT id, slug, CASE WHEN coalesce(web_command, '') <> '' THEN 'web' ELSE 'code' END, url_enabled, webhook_token_hash IS NOT NULL, env FROM functions WHERE project_id = $1 ORDER BY slug`, p.ID)
	if err != nil {
		return nil, err
	}
	fns, err := pgx.CollectRows(rows, pgx.RowToStructByPos[fnRow])
	if err != nil {
		return nil, err
	}
	fnByID := map[string]string{}
	for _, f := range fns {
		id := "fn:" + f.ID
		sub := "function"
		if f.Kind == "web" {
			sub = "web app (function)"
		}
		add(mapNode{ID: id, Kind: "function", Label: f.Slug, Sub: sub, Status: "ok", Href: base + "/f/" + f.Slug, Col: 1})
		fnByID[f.ID] = id
		if f.URL {
			add(mapNode{ID: "internet", Kind: "internet", Label: "Internet", Sub: "public addresses, HTTPS", Status: "idle", Col: 0})
			edge("internet", id, "URL", false)
		}
		if f.Webhook {
			add(mapNode{ID: "webhook:" + f.ID, Kind: "webhook", Label: "Webhook", Sub: "for " + f.Slug, Status: "idle", Col: 0, Href: base + "/f/" + f.Slug + "#triggers"})
			edge("webhook:"+f.ID, id, "", false)
		}
		var env map[string]string
		if json.Unmarshal(f.Env, &env) == nil {
			for _, v := range env {
				envOf[id] = append(envOf[id], v)
			}
		}
	}

	// --- databases ---
	type dbRow struct{ ID, Slug, Engine, Version, Status string }
	rows, err = tx.Query(ctx, `SELECT id, slug, engine, version, status FROM databases WHERE project_id = $1 ORDER BY slug`, p.ID)
	if err != nil {
		return nil, err
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dbRow])
	if err != nil {
		return nil, err
	}
	dbByHost := map[string]string{}
	for _, d := range dbs {
		id := "db:" + d.ID
		sub := model.EngineInfo(d.Engine).Name + " " + d.Version
		if d.Status != "running" {
			sub += " · " + d.Status
		}
		add(mapNode{ID: id, Kind: "database", Label: d.Slug, Sub: sub, Status: statusClass(d.Status), Href: base + "/d/" + d.Slug, Col: 2})
		dbByHost["pd-"+model.Compact(d.ID)[:12]] = id
		if statusClass(d.Status) == "busy" {
			m.Busy = true
		}
	}

	// --- secrets the platform wrote: service -> database, queue, bucket, email ---
	rows, err = tx.Query(ctx, `
		SELECT x.service_id, x.source,
		       coalesce(q.name, b.name, ''), coalesce(q.function_id::text, '')
		FROM secrets x JOIN services s ON s.id = x.service_id
		LEFT JOIN queues q ON x.source = 'queue:' || q.id::text
		LEFT JOIN buckets b ON x.source = 'bucket:' || b.id::text
		WHERE s.project_id = $1 AND s.preview_of IS NULL AND x.source <> ''
		GROUP BY 1, 2, 3, 4`, p.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var svcID, source, name, fnID string
		if err := rows.Scan(&svcID, &source, &name, &fnID); err != nil {
			rows.Close()
			return nil, err
		}
		from := "svc:" + svcID
		kind, ref, _ := strings.Cut(source, ":")
		switch kind {
		case "database":
			edge(from, "db:"+ref, "", false)
		case "queue":
			add(mapNode{ID: "queue:" + ref, Kind: "queue", Label: name, Sub: "queue", Status: "ok", Href: o.Path() + "/queues/" + name, Col: 2})
			edge(from, "queue:"+ref, "sends", false)
			if f, ok := fnByID[fnID]; ok && fnID != "" {
				edge("queue:"+ref, f, "triggers", false)
			}
		case "bucket":
			add(mapNode{ID: "bucket:" + ref, Kind: "bucket", Label: name, Sub: "bucket (organization)", Status: "ok", Href: o.Path() + "/storage/b/" + name, Col: 2})
			edge(from, "bucket:"+ref, "", false)
		case "email":
			add(mapNode{ID: "email", Kind: "email", Label: "Email", Sub: "sending API", Status: "ok", Href: o.Path() + "/email", Col: 2})
			edge(from, "email", "", false)
		}
	}
	rows.Close()

	// --- queues and buckets that trigger this project's functions ---
	rows, err = tx.Query(ctx, `
		SELECT 'queue', q.id::text, q.name, q.function_id::text FROM queues q JOIN functions f ON f.id = q.function_id WHERE f.project_id = $1
		UNION ALL
		SELECT 'bucket', b.id::text, b.name, t.function_id::text FROM bucket_triggers t JOIN buckets b ON b.id = t.bucket_id JOIN functions f ON f.id = t.function_id WHERE f.project_id = $1`, p.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, id, name, fnID string
		if err := rows.Scan(&kind, &id, &name, &fnID); err != nil {
			rows.Close()
			return nil, err
		}
		if kind == "queue" {
			add(mapNode{ID: "queue:" + id, Kind: "queue", Label: name, Sub: "queue", Status: "ok", Href: o.Path() + "/queues/" + name, Col: 2})
		} else {
			add(mapNode{ID: "bucket:" + id, Kind: "bucket", Label: name, Sub: "bucket (organization)", Status: "ok", Href: o.Path() + "/storage/b/" + name, Col: 2})
		}
		edge(kind+":"+id, fnByID[fnID], "triggers", false)
	}
	rows.Close()

	// --- domains and scheduled jobs (entry points) ---
	rows, err = tx.Query(ctx, `
		SELECT 'domain', dm.hostname, s.slug, CASE WHEN dm.verified_at IS NULL THEN 'pending' ELSE 'verified' END
		FROM domains dm JOIN services s ON s.id = dm.service_id WHERE s.project_id = $1
		UNION ALL
		SELECT 'job', j.slug || '|' || j.schedule, s.slug, CASE WHEN j.enabled THEN 'ready' ELSE 'paused' END
		FROM jobs j JOIN services s ON s.id = j.service_id WHERE s.project_id = $1
		ORDER BY 1, 2`, p.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, name, svc, st string
		if err := rows.Scan(&kind, &name, &svc, &st); err != nil {
			rows.Close()
			return nil, err
		}
		target, ok := svcBySlug[svc]
		if !ok {
			continue
		}
		if kind == "domain" {
			sub := "custom domain"
			if st != "verified" {
				sub += " · DNS pending"
			}
			dst := "ok"
			if st != "verified" {
				dst = "idle" // waiting for the owner's DNS records, not for the platform
			}
			add(mapNode{ID: "domain:" + name, Kind: "domain", Label: name, Sub: sub, Status: dst, Href: base + "/s/" + svc + "#domains", Col: 0})
			edge("domain:"+name, target, "", false)
		} else {
			slug, sched, _ := strings.Cut(name, "|")
			add(mapNode{ID: "job:" + svc + "/" + slug, Kind: "job", Label: slug, Sub: "schedule " + sched, Status: statusClass(st), Href: base + "/s/" + svc + "#jobs", Col: 0})
			edge("job:"+svc+"/"+slug, target, "runs", true)
		}
	}
	rows.Close()

	// --- connected projects (private network) ---
	peers, err := loadPeers(ctx, tx, p.ID)
	if err != nil {
		return nil, err
	}
	peerOfName := map[string]string{} // "<svc>.<project>.internal" -> peer node
	for _, pr := range peers {
		if o.Scoped && !o.sees(pr.Project) {
			continue
		}
		id := "peer:" + pr.Project
		add(mapNode{ID: id, Kind: "project", Label: pr.Project, Sub: "connected project · private network", Status: "idle", Href: o.Path() + "/p/" + pr.Project, Col: 0})
		for _, n := range pr.Names {
			peerOfName[n] = id
		}
	}

	// --- plain environment values: database hosts, private names, public addresses ---
	vars, err := s.loadProjectVarsPlain(ctx, tx, p.ID)
	if err != nil {
		return nil, err
	}
	for from, vals := range envOf {
		vals = append(vals, vars...)
		for _, v := range vals {
			for _, h := range dbHostRe.FindAllString(v, -1) {
				if to, ok := dbByHost[h]; ok {
					edge(from, to, "", false)
				}
			}
			for _, mm := range internalNameRe.FindAllStringSubmatch(v, -1) {
				if mm[2] == p.Slug {
					if to, ok := svcBySlug[mm[1]]; ok && to != from {
						edge(from, to, "calls", true)
					}
				} else if peer, ok := peerOfName[mm[0]]; ok {
					edge(from, peer, "calls", true)
				}
			}
			for h, to := range publicHost {
				if to != from && strings.Contains(v, h) {
					edge(from, to, "calls", true)
				}
			}
		}
	}
	m.layout()
	m.Poll = "every 15s"
	if m.Busy {
		m.Poll = "every 3s"
	}
	m.Path = base + "/map"
	return m, nil
}

// loadProjectVarsPlain returns the values of the project's non-secret variables.
func (s *Server) loadProjectVarsPlain(ctx context.Context, tx pgx.Tx, projectID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT name, value_enc FROM project_variables WHERE project_id = $1 AND NOT secret`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var enc []byte
		if err := rows.Scan(&name, &enc); err != nil {
			return nil, err
		}
		if plain, err := s.box.Open(enc, projVarAAD(projectID, name)); err == nil {
			out = append(out, string(plain))
		}
	}
	return out, rows.Err()
}

// layout places the nodes in their columns (in a stable order) and draws the edges.
func (m *projectMap) layout() {
	order := map[string]int{"internet": 0, "domain": 1, "webhook": 2, "job": 3, "project": 4,
		"service": 0, "worker": 1, "function": 2, "database": 0, "queue": 1, "bucket": 2, "email": 3}
	sort.SliceStable(m.Nodes, func(i, j int) bool {
		a, b := m.Nodes[i], m.Nodes[j]
		if a.Col != b.Col {
			return a.Col < b.Col
		}
		if order[a.Kind] != order[b.Kind] {
			return order[a.Kind] < order[b.Kind]
		}
		return a.Label < b.Label
	})
	rows := [3]int{}
	idx := map[string]*mapNode{}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		n.Row = rows[n.Col]
		rows[n.Col]++
		n.X = mapLeft + n.Col*mapColW
		n.Y = mapTop + n.Row*mapRowH
		n.DotX, n.DotY = n.X+15, n.Y+19
		n.TX, n.TY = n.X+27, n.Y+23
		n.SX, n.SY = n.X+15, n.Y+41
		n.Short, n.ShortSub = shorten(n.Label, 24), shorten(n.Sub, 33)
		idx[n.ID] = n
	}
	maxRows := max(rows[0], rows[1], rows[2], 1)
	m.Width = mapLeft*2 + 2*mapColW + mapNodeW
	m.Height = mapTop + maxRows*mapRowH
	m.ColumnTitles = []mapColumn{{"Comes in from", mapLeft}, {"Runs", mapLeft + mapColW}, {"Uses", mapLeft + 2*mapColW}}
	kept := m.Edges[:0]
	for _, e := range m.Edges {
		a, b := idx[e.From], idx[e.To]
		if a == nil || b == nil {
			continue
		}
		var x1, y1, x2, y2 int
		y1, y2 = a.Y+mapNodeH/2, b.Y+mapNodeH/2
		switch {
		case a.Col < b.Col:
			x1, x2 = a.X+mapNodeW, b.X
		case a.Col > b.Col:
			x1, x2 = a.X, b.X+mapNodeW
		default: // same column: loop out on the right
			x1, x2 = a.X+mapNodeW, b.X+mapNodeW
		}
		if a.Col == b.Col {
			bend := x1 + 40
			e.Path = fmt.Sprintf("M%d %d C%d %d %d %d %d %d", x1, y1, bend, y1, bend, y2, x2, y2)
			e.LX, e.LY = bend+4, (y1+y2)/2
		} else {
			mid := (x1 + x2) / 2
			e.Path = fmt.Sprintf("M%d %d C%d %d %d %d %d %d", x1, y1, mid, y1, mid, y2, x2, y2)
			// label near the arrow's end, where lines no longer cross
			e.LX, e.LY = x2-(x2-x1)/5, y2-5
		}
		e.Title = a.Label + " → " + b.Label
		if e.Label != "" {
			e.Title += " (" + e.Label + ")"
		}
		kept = append(kept, e)
	}
	m.Edges = kept
}

// projectMapFragment serves the map alone (htmx refreshes it in place).
func (s *Server) projectMapFragment(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, o.Path()+"/p/"+r.PathValue("project")+"#map", http.StatusSeeOther)
		return
	}
	var pm *projectMap
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		pm, err = s.loadProjectMap(r.Context(), tx, o, p)
		return err
	})
	switch {
	case isNoRows(err):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		s.renderPartial(w, r, "projectmap", pm)
	}
}

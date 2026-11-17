package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// Operator console (/operator): the whole platform at a glance, organizations (plan,
// suspend/restore), invoices (paid/void) and the global audit log. Only users with
// is_operator and a full two-step session; everyone else gets "not found". Data comes
// from the operator_* SECURITY DEFINER functions (migration 018); every change is audited.

func (s *Server) operatorOnly(h http.HandlerFunc) http.HandlerFunc {
	return s.auth(authFull, func(w http.ResponseWriter, r *http.Request) {
		if !sessionFrom(r).IsOperator {
			s.notFound(w, r)
			return
		}
		h(w, r)
	})
}

type opOverview struct {
	Orgs, Users, LiveServices, Trials, Suspended, OpenIncidents, OpenInvoices, BuildsToday, DiskUsedMB int64
}

type opOrg struct {
	Slug, Name, PlanID, Owner string
	Services, Databases       int64
	DiskUsedMB, MaxDiskMB     int
	Suspended                 bool
	SuspendReason             string
	TrialEnds                 *time.Time
	Created                   time.Time
}

type opInvoice struct {
	Number, OrgSlug string
	PeriodStart     time.Time
	Total           int64
	Currency        string
	Status          string
	Issued          time.Time
}

type opEvent struct {
	At                               time.Time
	OrgSlug, Who, Action, Target, IP string
}

func (s *Server) operatorPage(w http.ResponseWriter, r *http.Request) {
	var ov opOverview
	if err := s.pool.QueryRow(r.Context(), `SELECT * FROM operator_overview()`).
		Scan(&ov.Orgs, &ov.Users, &ov.LiveServices, &ov.Trials, &ov.Suspended, &ov.OpenIncidents, &ov.OpenInvoices, &ov.BuildsToday, &ov.DiskUsedMB); err != nil {
		s.serverError(w, r, err)
		return
	}
	collect := func(sql string, fn func(pgx.CollectableRow) error) error {
		rows, err := s.pool.Query(r.Context(), sql)
		if err != nil {
			return err
		}
		_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) { return struct{}{}, fn(row) })
		return err
	}
	var orgs []opOrg
	var invoices []opInvoice
	var events []opEvent
	err := collect(`SELECT * FROM operator_orgs()`, func(row pgx.CollectableRow) error {
		var o opOrg
		err := row.Scan(&o.Slug, &o.Name, &o.PlanID, &o.Owner, &o.Services, &o.Databases, &o.DiskUsedMB, &o.MaxDiskMB,
			&o.Suspended, &o.SuspendReason, &o.TrialEnds, &o.Created)
		orgs = append(orgs, o)
		return err
	})
	if err == nil {
		err = collect(`SELECT * FROM operator_invoices()`, func(row pgx.CollectableRow) error {
			var i opInvoice
			err := row.Scan(&i.Number, &i.OrgSlug, &i.PeriodStart, &i.Total, &i.Currency, &i.Status, &i.Issued)
			invoices = append(invoices, i)
			return err
		})
	}
	if err == nil {
		err = collect(`SELECT * FROM operator_audit()`, func(row pgx.CollectableRow) error {
			var e opEvent
			err := row.Scan(&e.At, &e.OrgSlug, &e.Who, &e.Action, &e.Target, &e.IP)
			events = append(events, e)
			return err
		})
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var plans []string
	err = collect(`SELECT id FROM plans ORDER BY max_cpu_millis, id`, func(row pgx.CollectableRow) error {
		var id string
		err := row.Scan(&id)
		plans = append(plans, id)
		return err
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	nodes, err := s.loadNodesWithLoad(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	usage, err := s.loadUsage(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	type opMove struct {
		Org, Project, From, To, Status, Message string
		Updated                                 time.Time
	}
	var moves []opMove
	if rows, err := s.pool.Query(r.Context(), `SELECT * FROM operator_moves()`); err == nil {
		moves, _ = pgx.CollectRows(rows, pgx.RowToStructByPos[opMove])
	}
	s.render(w, r, "operator", http.StatusOK, data{"Overview": ov, "Orgs": orgs, "Invoices": invoices, "Events": events,
		"Plans": plans, "Nodes": nodes, "Usage": usage, "Moves": moves, "MovesEnabled": s.cfg.MovesEnabled()})
}

func (s *Server) operatorSetPlan(w http.ResponseWriter, r *http.Request) {
	slug, plan := r.PathValue("slug"), r.PostFormValue("plan")
	var ok bool
	if err := s.pool.QueryRow(r.Context(), `SELECT operator_set_plan($1, $2)`, slug, plan).Scan(&ok); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		back(w, r, "/operator#op-orgs", "", "Unknown organization or plan.")
		return
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.set_plan", "org:"+slug, map[string]string{"plan": plan}, clientIP(r))
	back(w, r, "/operator#op-orgs", "Plan of "+slug+" set to "+plan+".", "")
}

func (s *Server) operatorSuspend(w http.ResponseWriter, r *http.Request) {
	slug, on := r.PathValue("slug"), r.PostFormValue("on") == "1"
	var ok bool
	if err := s.pool.QueryRow(r.Context(), `SELECT operator_suspend($1, $2)`, slug, on).Scan(&ok); err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		back(w, r, "/operator#op-orgs", "", "Unknown organization.")
		return
	}
	action, msg := "operator.restore", slug+" restored: its applications start again."
	if on {
		action, msg = "operator.suspend", slug+" suspended: its applications stop within seconds."
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, action, "org:"+slug, nil, clientIP(r))
	back(w, r, "/operator#op-orgs", msg, "")
}

func (s *Server) operatorInvoice(w http.ResponseWriter, r *http.Request) {
	number, status := r.PathValue("number"), r.PostFormValue("status")
	var orgID *string
	err := s.pool.QueryRow(r.Context(), `SELECT operator_invoice_set($1, $2)`, number, status).Scan(&orgID)
	if err != nil || orgID == nil {
		back(w, r, "/operator#op-invoices", "", "No open invoice "+number+".")
		return
	}
	_ = db.Audit(r.Context(), s.pool, *orgID, sessionFrom(r).UserID, "invoice."+status, "invoice:"+number, map[string]bool{"by_operator": true}, clientIP(r))
	back(w, r, "/operator#op-invoices", "Invoice "+number+" marked "+status+".", "")
}

// ---------- nodes and usage (no tenant data: names, sizes and measurements only) ----------

type opNode struct {
	ID, Name, Cluster, Role, Address string
	CPUs                             int
	MemMB, MemAvailMB                int64
	Load1                            float64
	DiskTotalMB, DiskFreeMB          int64
	Containers                       int
	DockerVersion, AgentVersion      string
	Services                         map[string]string
	Cordoned                         bool
	Created, LastSeen                time.Time
	// allocation placed on this node
	Projects, CPUAlloc, MemAlloc, Stateful int64
}

func (n opNode) AllocMemPct() int { return pct(n.MemAlloc, n.MemMB) }

func (n opNode) Online() bool { return time.Since(n.LastSeen) < 2*time.Minute }
func (n opNode) MemPct() int  { return pct(n.MemMB-n.MemAvailMB, n.MemMB) }
func (n opNode) DiskPct() int { return pct(n.DiskTotalMB-n.DiskFreeMB, n.DiskTotalMB) }
func (n opNode) LoadPct() int { return pct(int64(n.Load1*100), int64(max(n.CPUs, 1)*100)) }
func (n opNode) ServiceNames() []string {
	var out []string
	for k := range n.Services {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func pct(used, total int64) int {
	if total <= 0 {
		return 0
	}
	p := int(used * 100 / total)
	return max(0, min(p, 100))
}

type opUsage struct {
	Slug                                 string
	CPUAlloc, MemAlloc                   int
	CPUUsedPct                           float64
	MemUsedMB                            int64
	VCPUHours, GBHours, GBOut, BuildMins float64
	DiskUsedMB                           int
}

func (s *Server) loadNodes(r *http.Request) ([]opNode, error) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, name, cluster, role, address, cpus, mem_mb, mem_avail_mb, load1::float8, disk_total_mb, disk_free_mb,
		       containers, docker_version, agent_version, services, cordoned, created_at, last_seen FROM operator_nodes()`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (opNode, error) {
		var n opNode
		var svc []byte
		err := row.Scan(&n.ID, &n.Name, &n.Cluster, &n.Role, &n.Address, &n.CPUs, &n.MemMB, &n.MemAvailMB, &n.Load1,
			&n.DiskTotalMB, &n.DiskFreeMB, &n.Containers, &n.DockerVersion, &n.AgentVersion, &svc, &n.Cordoned, &n.Created, &n.LastSeen)
		_ = json.Unmarshal(svc, &n.Services)
		return n, err
	})
}

func (s *Server) loadNodesWithLoad(r *http.Request) ([]opNode, error) {
	nodes, err := s.loadNodes(r)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(r.Context(), `SELECT * FROM operator_node_load()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	load := map[string][4]int64{}
	for rows.Next() {
		var id string
		var v [4]int64
		if err := rows.Scan(&id, &v[0], &v[1], &v[2], &v[3]); err != nil {
			return nil, err
		}
		load[id] = v
	}
	for i := range nodes {
		v := load[nodes[i].ID]
		nodes[i].Projects, nodes[i].CPUAlloc, nodes[i].MemAlloc, nodes[i].Stateful = v[0], v[1], v[2], v[3]
	}
	return nodes, rows.Err()
}

func (s *Server) operatorDrainNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var moved, kept int64
	// Projects with data move through the backup repository, which every node must reach.
	moveStateful := r.PostFormValue("move_stateful") == "1" && s.cfg.MovesEnabled()
	if err := s.pool.QueryRow(r.Context(), `SELECT * FROM operator_drain_node($1, $2)`, id, moveStateful).Scan(&moved, &kept); err != nil {
		s.serverError(w, r, err)
		return
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.node_drain", "node:"+id,
		map[string]any{"moved": moved, "kept": kept, "move_stateful": moveStateful}, clientIP(r))
	msg := "Node " + id + " drained: " + itoa(int(moved)) + " project(s) are moving to other nodes."
	if moveStateful {
		msg += " Projects with data stop for the time their data is copied (see Moves)."
	}
	if kept > 0 {
		msg += " " + itoa(int(kept)) + " project(s) with persistent data stay on the node."
	}
	back(w, r, "/operator#op-nodes", msg, "")
}

func (s *Server) operatorOrgCluster(w http.ResponseWriter, r *http.Request) {
	slug, cluster := r.PathValue("slug"), r.PostFormValue("cluster")
	var ok bool
	if err := s.pool.QueryRow(r.Context(), `SELECT operator_set_org_cluster($1, $2)`, slug, cluster).Scan(&ok); err != nil || !ok {
		back(w, r, "/operator/orgs/"+slug, "", "Invalid cluster name.")
		return
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.org_cluster", "org:"+slug, map[string]string{"cluster": cluster}, clientIP(r))
	msg := slug + " can run on any node."
	if cluster != "" {
		msg = "New projects of " + slug + " are placed in cluster " + cluster + "."
	}
	back(w, r, "/operator/orgs/"+slug, msg, "")
}

func (s *Server) loadUsage(r *http.Request) ([]opUsage, error) {
	rows, err := s.pool.Query(r.Context(), `
		SELECT slug, cpu_alloc, mem_alloc, cpu_used_pct::float8, mem_used_mb, vcpu_hours::float8, gb_hours::float8,
		       gb_out::float8, build_minutes::float8, disk_used_mb FROM operator_org_usage()`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[opUsage])
}

func (s *Server) operatorSetNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cordon := r.PostFormValue("cordoned") == "1"
	var ok bool
	if err := s.pool.QueryRow(r.Context(), `SELECT operator_set_node($1, $2, $3)`, id, cordon, r.PostFormValue("cluster")).Scan(&ok); err != nil || !ok {
		back(w, r, "/operator#op-nodes", "", "Unknown node or invalid cluster name.")
		return
	}
	action, msg := "operator.node_uncordon", "Node "+id+" accepts new projects again."
	if cordon {
		action, msg = "operator.node_cordon", "Node "+id+" cordoned: no new projects are placed on it; running ones stay."
	}
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, action, "node:"+id, nil, clientIP(r))
	back(w, r, "/operator#op-nodes", msg, "")
}

// ---------- one organization ----------

type opService struct {
	Project, Service, Image, Preview, Status string
	CPUMillis, MemoryMB                      int
	CPUPct                                   *float64
	MemMB                                    *int
	NodeID                                   string
	WAF, HasVolume                           bool
	DeployedAt                               *time.Time
}

type opDatabase struct {
	Project, Name, Engine, Version, Status string
	CPUMillis, MemoryMB                    int
	Created                                time.Time
}

type opDomain struct {
	Hostname, Service string
	Verified          bool
}

type opIncident struct {
	Kind, Status, Service string
	Started               time.Time
	Resolved              *time.Time
}

func (i opIncident) Title() string { return incidentView{Kind: i.Kind}.Title() }

func (s *Server) operatorOrgPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	var org *opOrg
	rows, err := s.pool.Query(r.Context(), `SELECT * FROM operator_orgs() WHERE slug = $1`, slug)
	if err == nil {
		var list []opOrg
		list, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (opOrg, error) {
			var o opOrg
			err := row.Scan(&o.Slug, &o.Name, &o.PlanID, &o.Owner, &o.Services, &o.Databases, &o.DiskUsedMB, &o.MaxDiskMB,
				&o.Suspended, &o.SuspendReason, &o.TrialEnds, &o.Created)
			return o, err
		})
		if len(list) == 1 {
			org = &list[0]
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if org == nil {
		s.notFound(w, r)
		return
	}
	q := func(sql string) (pgx.Rows, error) { return s.pool.Query(r.Context(), sql, slug) }
	var services []opService
	var dbs []opDatabase
	var domains []opDomain
	var incidents []opIncident
	if rows, err = q(`SELECT project, service, image, preview, status, cpu_millis, memory_mb, cpu_pct::float8, mem_mb, node_id, waf, has_volume, deployed_at FROM operator_org_services($1)`); err == nil {
		services, err = pgx.CollectRows(rows, pgx.RowToStructByPos[opService])
	}
	if err == nil {
		if rows, err = q(`SELECT * FROM operator_org_databases($1)`); err == nil {
			dbs, err = pgx.CollectRows(rows, pgx.RowToStructByPos[opDatabase])
		}
	}
	if err == nil {
		if rows, err = q(`SELECT * FROM operator_org_domains($1)`); err == nil {
			domains, err = pgx.CollectRows(rows, pgx.RowToStructByPos[opDomain])
		}
	}
	if err == nil {
		if rows, err = q(`SELECT * FROM operator_org_incidents($1)`); err == nil {
			incidents, err = pgx.CollectRows(rows, pgx.RowToStructByPos[opIncident])
		}
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var usage *opUsage
	if all, err := s.loadUsage(r); err == nil {
		for i := range all {
			if all[i].Slug == slug {
				usage = &all[i]
			}
		}
	}
	var cluster string
	_ = s.pool.QueryRow(r.Context(), `SELECT cluster FROM operator_org_cluster($1)`, slug).Scan(&cluster)
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.viewed_org", "org:"+slug, nil, clientIP(r))
	s.render(w, r, "operator_org", http.StatusOK, data{"O": org, "Services": services, "Databases": dbs, "Domains": domains,
		"Incidents": incidents, "Usage": usage, "Cluster": cluster})
}

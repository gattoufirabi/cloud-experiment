package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// Public status pages: /status/{org}/{project}, opt-in per project. Data comes only from
// SECURITY DEFINER functions that return nothing unless the project opted in, and that
// expose service names, up/down state, uptime counts and outage times, nothing else.

type uptimeCell struct {
	Day   string
	Class string
	Title string
}

type statusService struct {
	Name    string
	State   string
	Uptime  string
	Cells   []uptimeCell
	ok, tot int64
	byDay   map[string][2]int64
}

type statusIncident struct {
	Service  string
	Started  time.Time
	Resolved *time.Time
}

func cellClass(pct float64) string {
	switch {
	case pct >= 100:
		return "c-full"
	case pct >= 99:
		return "c-high"
	case pct >= 95:
		return "c-mid"
	default:
		return "c-low"
	}
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	org, proj := r.PathValue("org"), r.PathValue("project")
	var orgName, projName string
	if err := s.pool.QueryRow(r.Context(), `SELECT org_name, project_name FROM status_page_project($1, $2)`, org, proj).
		Scan(&orgName, &projName); err != nil {
		s.render(w, r, "error", http.StatusNotFound, data{"Title": "Status page not found", "Text": "This status page does not exist or is not public."})
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT service, state, day, ok, total FROM status_page_services($1, $2)`, org, proj)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var order []string
	svcs := map[string]*statusService{}
	for rows.Next() {
		var name, state string
		var day *time.Time
		var ok, total int64
		if err := rows.Scan(&name, &state, &day, &ok, &total); err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		sv, found := svcs[name]
		if !found {
			sv = &statusService{Name: name, State: state, byDay: map[string][2]int64{}}
			svcs[name] = sv
			order = append(order, name)
		}
		if day != nil {
			sv.byDay[day.Format("2006-01-02")] = [2]int64{ok, total}
			sv.ok += ok
			sv.tot += total
		}
	}
	rows.Close()

	today := time.Now().UTC()
	allUp, anyDown := len(order) > 0, false
	var services []statusService
	for _, name := range order {
		sv := svcs[name]
		for i := 29; i >= 0; i-- {
			d := today.AddDate(0, 0, -i).Format("2006-01-02")
			c := uptimeCell{Day: d, Class: "c-none", Title: d + ": no data"}
			if v, ok := sv.byDay[d]; ok && v[1] > 0 {
				pct := float64(v[0]) * 100 / float64(v[1])
				c.Class, c.Title = cellClass(pct), fmt.Sprintf("%s: %.2f%% up", d, pct)
			}
			sv.Cells = append(sv.Cells, c)
		}
		sv.Uptime = "no data yet"
		if sv.tot > 0 {
			sv.Uptime = fmt.Sprintf("%.2f%% uptime", float64(sv.ok)*100/float64(sv.tot))
		}
		if sv.State == "down" {
			anyDown, allUp = true, false
		} else if sv.State != "up" {
			allUp = false
		}
		services = append(services, *sv)
	}

	var incidents []statusIncident
	if irows, err := s.pool.Query(r.Context(), `SELECT service, started_at, resolved_at FROM status_page_incidents($1, $2)`, org, proj); err == nil {
		incidents, _ = pgx.CollectRows(irows, pgx.RowToStructByPos[statusIncident])
	}
	overall := "All systems operational"
	switch {
	case anyDown:
		overall = "Some services are down"
	case len(services) == 0:
		overall = "No services yet"
	case !allUp:
		overall = "Some services are not running"
	}
	s.render(w, r, "status", http.StatusOK, data{
		"OrgName": orgName, "ProjectName": projName, "Services": services, "Incidents": incidents,
		"Overall": overall, "AnyDown": anyDown, "Updated": time.Now().UTC(),
	})
}

// toggleStatusPage turns a project's public status page on or off (admins).
func (s *Server) toggleStatusPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "admin")
	if !ok {
		return
	}
	on := r.PostFormValue("public") == "1"
	slug := r.PathValue("project")
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `UPDATE projects SET status_public = $2 WHERE slug = $1`, slug, on)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		s.audit(r, tx, o.ID, "status_page.updated", "project:"+slug, map[string]bool{"public": on})
		return nil
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	msg := "Public status page turned off."
	if on {
		msg = "Public status page is on: share /status/" + o.Slug + "/" + slug
	}
	back(w, r, o.Path()+"/p/"+slug+"#project-settings", msg, "")
}

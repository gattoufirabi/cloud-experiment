package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
)

// Bulk actions on the lists of databases, functions, buckets and domains, like the
// services list: select rows, choose an action; deleting needs "delete" typed.

type bulkArea struct {
	path    string            // list page, e.g. "/databases"
	noun    string            // "database"
	actions map[string]string // action -> minimum role
	verbs   map[string]string // action -> past tense for the message
	apply   func(s *Server, r *http.Request, tx pgx.Tx, o *orgInfo, item, action string) (skip string, err error)
}

var bulkAreas = map[string]bulkArea{
	"databases": {path: "/databases", noun: "database",
		actions: map[string]string{"backup": "developer", "delete": "admin"},
		verbs:   map[string]string{"backup": "Backing up", "delete": "Deleted"},
		apply: func(s *Server, r *http.Request, tx pgx.Tx, o *orgInfo, item, action string) (string, error) {
			ps, ds, _ := strings.Cut(item, "/")
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT d.id FROM databases d JOIN projects p ON p.id = d.project_id WHERE p.slug = $1 AND d.slug = $2`, ps, ds).Scan(&id); err != nil {
				return item + " (not found)", nil
			}
			if action == "backup" {
				busy, err := s.queueTargetJob(r, tx, o, "database_id", id, ds, "backup", "")
				if busy {
					return item + " (a backup is already running)", err
				}
				return "", err
			}
			if _, err := tx.Exec(r.Context(), `DELETE FROM databases WHERE id = $1`, id); err != nil {
				return "", err
			}
			s.audit(r, tx, o.ID, "database.deleted", "database:"+item, map[string]string{"via": "bulk action"})
			return "", nil
		}},
	"functions": {path: "/functions", noun: "function",
		actions: map[string]string{"url-off": "developer", "delete": "developer"},
		verbs:   map[string]string{"url-off": "Turned off the URL of", "delete": "Deleted"},
		apply: func(s *Server, r *http.Request, tx pgx.Tx, o *orgInfo, item, action string) (string, error) {
			ps, fs, _ := strings.Cut(item, "/")
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT f.id FROM functions f JOIN projects p ON p.id = f.project_id WHERE p.slug = $1 AND f.slug = $2`, ps, fs).Scan(&id); err != nil {
				return item + " (not found)", nil
			}
			if action == "url-off" {
				_, err := tx.Exec(r.Context(), `UPDATE functions SET url_enabled = false, updated_at = now() WHERE id = $1`, id)
				if err == nil {
					s.audit(r, tx, o.ID, "function.updated", "function:"+item, map[string]any{"url": false, "via": "bulk action"})
				}
				return "", err
			}
			if _, err := tx.Exec(r.Context(), `DELETE FROM functions WHERE id = $1`, id); err != nil {
				return "", err
			}
			s.audit(r, tx, o.ID, "function.deleted", "function:"+item, map[string]string{"via": "bulk action"})
			return "", nil
		}},
	"storage": {path: "/storage", noun: "bucket",
		actions: map[string]string{"delete": "admin"},
		verbs:   map[string]string{"delete": "Deleted"},
		apply: func(s *Server, r *http.Request, tx pgx.Tx, o *orgInfo, item, action string) (string, error) {
			var id, full string
			if err := tx.QueryRow(r.Context(), `DELETE FROM buckets WHERE name = $1 RETURNING id, full_name`, item).Scan(&id, &full); err != nil {
				return item + " (not found)", nil
			}
			s.audit(r, tx, o.ID, "bucket.deleted", "bucket:"+full, map[string]string{"via": "bulk action"})
			return "", nil
		}},
	"domains": {path: "/domains", noun: "domain",
		actions: map[string]string{"remove": "developer"},
		verbs:   map[string]string{"remove": "Removed"},
		apply: func(s *Server, r *http.Request, tx pgx.Tx, o *orgInfo, item, action string) (string, error) {
			var host, id string
			if err := tx.QueryRow(r.Context(), `DELETE FROM domains WHERE hostname = $1 RETURNING hostname, id::text`, item).Scan(&host, &id); err != nil {
				return item + " (not found)", nil
			}
			s.audit(r, tx, o.ID, "domain.removed", "domain:"+host, map[string]string{"via": "bulk action"})
			return "", s.removeManagedTx(r.Context(), r, tx, o, "domain:"+id)
		}},
}

func (s *Server) bulkActions(area string) http.HandlerFunc {
	a := bulkAreas[area]
	return func(w http.ResponseWriter, r *http.Request) {
		o, ok := s.org(w, r, "developer")
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			back(w, r, o.Path()+a.path, "", "Invalid form.")
			return
		}
		sel, action := r.PostForm["sel"], r.PostFormValue("action")
		minRole, known := a.actions[action]
		switch {
		case len(sel) == 0:
			back(w, r, o.Path()+a.path, "", "Select at least one "+a.noun+".")
			return
		case !known:
			back(w, r, o.Path()+a.path, "", "Choose an action.")
			return
		case !model.RoleAtLeast(o.Role, minRole):
			back(w, r, o.Path()+a.path, "", "This action needs the "+minRole+" role.")
			return
		case (action == "delete" || action == "remove") && r.PostFormValue("confirm") != "delete":
			back(w, r, o.Path()+a.path, "", `Type "delete" to confirm.`)
			return
		}
		var done, skipped []string
		err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
			for _, item := range sel {
				skip, err := a.apply(s, r, tx, o, item, action)
				if err != nil {
					return err
				}
				if skip != "" {
					skipped = append(skipped, skip)
				} else {
					done = append(done, item)
				}
			}
			_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', ''), pg_notify('deployments', 'storage')`)
			return err
		})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		msg, errMsg := "", ""
		if len(done) > 0 {
			msg = fmt.Sprintf("%s %d %s(s): %s.", a.verbs[action], len(done), a.noun, strings.Join(done, ", "))
		}
		if len(skipped) > 0 {
			errMsg = "Skipped: " + strings.Join(skipped, ", ")
		}
		back(w, r, o.Path()+a.path, msg, errMsg)
	}
}

// addDomainFromList adds a custom domain from the Domains page (service chosen in a list).
func (s *Server) addDomainFromList(w http.ResponseWriter, r *http.Request) {
	ps, ss, ok := strings.Cut(r.PostFormValue("service"), "/")
	if !ok || ps == "" || ss == "" {
		back(w, r, "/o/"+r.PathValue("org")+"/domains#add", "", "Choose a service.")
		return
	}
	r.SetPathValue("project", ps)
	r.SetPathValue("service", ss)
	s.addDomain(w, r)
}

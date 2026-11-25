package api

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
)

// setMemberAccess limits a developer or viewer to chosen projects, each with a role,
// or gives them back the whole organization (no project chosen). Admins only.
func (s *API) setMemberAccess(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "admin")
	if !ok {
		return
	}
	userID := r.PathValue("user")
	var formErr, email string
	var granted []string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var role string
		if err := tx.QueryRow(r.Context(), `SELECT m.role, u.email FROM memberships m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND m.user_id::text = $2`,
			o.ID, userID).Scan(&role, &email); err != nil {
			return err
		}
		if role != "developer" && role != "viewer" {
			formErr = "Owners and admins manage the whole organization; only developers and viewers can be limited to projects."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM member_project_roles WHERE user_id::text = $1`, userID); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT id, slug FROM projects ORDER BY slug`)
		if err != nil {
			return err
		}
		type proj struct{ ID, Slug string }
		projs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[proj])
		if err != nil {
			return err
		}
		for _, p := range projs {
			pr := r.PostFormValue("project_" + p.Slug)
			if pr != "developer" && pr != "viewer" {
				continue
			}
			if pr == "developer" && role == "viewer" {
				pr = "viewer" // a project role never exceeds the member's role
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO member_project_roles (org_id, user_id, project_id, role) VALUES ($1, $2, $3, $4)`,
				o.ID, userID, p.ID, pr); err != nil {
				return err
			}
			granted = append(granted, p.Slug+":"+pr)
		}
		s.Audit(r, tx, o.ID, "member.access_changed", "user:"+userID, map[string]any{"projects": granted})
		return nil
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, o.Path()+"/iam#members", "", formErr)
		return
	}
	msg := email + " has access to the whole organization again."
	if len(granted) > 0 {
		msg = email + " now only sees: " + strings.Join(granted, ", ") + "."
	}
	web.Back(w, r, o.Path()+"/iam#members", msg, "")
}

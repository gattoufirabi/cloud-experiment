package web

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Shared project variables: set once on a project, passed to every service in it (and
// its previews). A service's own variables and secrets override a project variable of
// the same name. Values are encrypted at rest; secret ones are never shown again.

type ProjectVar struct {
	Name    string    `json:"name"`
	Secret  bool      `json:"secret"`
	Value   string    `json:"value,omitempty"` // plain variables only
	Updated time.Time `json:"updated_at"`
}

func ProjVarAAD(projectID, name string) string { return "projvar:" + projectID + ":" + name }

// loadProjectVars lists a project's variables; secret values stay encrypted.
func (s *Server) LoadProjectVars(r *http.Request, tx pgx.Tx, projectID string) ([]ProjectVar, error) {
	rows, err := tx.Query(r.Context(), `SELECT name, secret, value_enc, updated_at FROM project_variables WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectVar
	for rows.Next() {
		var v ProjectVar
		var enc []byte
		if err := rows.Scan(&v.Name, &v.Secret, &enc, &v.Updated); err != nil {
			return nil, err
		}
		if !v.Secret {
			plain, err := s.Box.Open(enc, ProjVarAAD(projectID, v.Name))
			if err != nil {
				return nil, err
			}
			v.Value = string(plain)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

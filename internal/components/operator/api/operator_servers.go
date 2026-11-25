package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
)

// The admin panel's Servers page: the platform's servers (nodes), restarting a platform
// service on the primary, and how to add a server.

var restartable = []string{"api", "caddy", "stalwart", "garage", "registry", "trivy", "victorialogs", "victoriametrics"}

func (s *API) operatorServersPage(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.loadNodesWithLoad(r)
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	type opMove struct {
		Org, Project, From, To, Status, Message string
		Updated                                 time.Time
	}
	var moves []opMove
	if rows, err := s.Pool.Query(r.Context(), `SELECT * FROM operator_moves()`); err == nil {
		moves, _ = pgx.CollectRows(rows, pgx.RowToStructByPos[opMove])
	}
	type restart struct {
		Service   string
		Requested time.Time
		Done      bool
		Error     string
	}
	var restarts []restart
	if rows, err := s.Pool.Query(r.Context(), `SELECT service, requested_at, done_at IS NOT NULL, error FROM platform_restarts ORDER BY id DESC LIMIT 10`); err == nil {
		restarts, _ = pgx.CollectRows(rows, pgx.RowToStructByPos[restart])
	}
	s.Render(w, r, "operator_servers", http.StatusOK, web.Data{"OpSection": "servers", "Nodes": nodes, "Moves": moves,
		"MovesEnabled": s.Cfg.MovesEnabled(), "Restartable": restartable, "Restarts": restarts})
}

func (s *API) operatorRestart(w http.ResponseWriter, r *http.Request) {
	svc := r.PostFormValue("service")
	known := false
	for _, x := range restartable {
		known = known || x == svc
	}
	if !known {
		web.Back(w, r, "/operator/servers#restart", "", "Choose one of the listed services.")
		return
	}
	if _, err := s.Pool.Exec(r.Context(), `INSERT INTO platform_restarts (service, requested_by) VALUES ($1, $2)`, svc, web.SessionFrom(r).UserID); err != nil {
		s.ServerError(w, r, err)
		return
	}
	_, _ = s.Pool.Exec(r.Context(), `SELECT pg_notify('deployments', 'restart')`)
	_ = db.Audit(r.Context(), s.Pool, "", web.SessionFrom(r).UserID, "operator.restart", "service:"+svc, nil, web.ClientIP(r))
	web.Back(w, r, "/operator/servers#restart", "Restarting "+svc+"; reload in a few seconds to see the result.", "")
}

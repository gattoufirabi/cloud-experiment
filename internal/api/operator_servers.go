package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
)

// The admin panel's Servers page: the platform's servers (nodes), restarting a platform
// service on the primary, and how to add a server.

var restartable = []string{"api", "caddy", "stalwart", "garage", "registry", "trivy", "victorialogs", "victoriametrics"}

func (s *Server) operatorServersPage(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.loadNodesWithLoad(r)
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
	type restart struct {
		Service   string
		Requested time.Time
		Done      bool
		Error     string
	}
	var restarts []restart
	if rows, err := s.pool.Query(r.Context(), `SELECT service, requested_at, done_at IS NOT NULL, error FROM platform_restarts ORDER BY id DESC LIMIT 10`); err == nil {
		restarts, _ = pgx.CollectRows(rows, pgx.RowToStructByPos[restart])
	}
	s.render(w, r, "operator_servers", http.StatusOK, data{"OpSection": "servers", "Nodes": nodes, "Moves": moves,
		"MovesEnabled": s.cfg.MovesEnabled(), "Restartable": restartable, "Restarts": restarts})
}

func (s *Server) operatorRestart(w http.ResponseWriter, r *http.Request) {
	svc := r.PostFormValue("service")
	known := false
	for _, x := range restartable {
		known = known || x == svc
	}
	if !known {
		back(w, r, "/operator/servers#restart", "", "Choose one of the listed services.")
		return
	}
	if _, err := s.pool.Exec(r.Context(), `INSERT INTO platform_restarts (service, requested_by) VALUES ($1, $2)`, svc, sessionFrom(r).UserID); err != nil {
		s.serverError(w, r, err)
		return
	}
	_, _ = s.pool.Exec(r.Context(), `SELECT pg_notify('deployments', 'restart')`)
	_ = db.Audit(r.Context(), s.pool, "", sessionFrom(r).UserID, "operator.restart", "service:"+svc, nil, clientIP(r))
	back(w, r, "/operator/servers#restart", "Restarting "+svc+"; reload in a few seconds to see the result.", "")
}

package web

import (
	"net/http"

	"github.com/jackc/pgx/v5"
)

// loadServiceByID loads a service the way loadService does, by ID (webhooks have no path).
func (s *Server) LoadServiceByID(r *http.Request, tx pgx.Tx, id string) (*ProjectInfo, *ServiceInfo, error) {
	var p ProjectInfo
	var sv ServiceInfo
	err := tx.QueryRow(r.Context(), `
		SELECT p.id, p.slug, p.name, p.status_public,
		       s.id, s.slug, s.image, s.health_path, s.volume_path, s.port, s.cpu_millis, s.memory_mb, s.env, s.worker, s.instances, s.ports
		FROM services s JOIN projects p ON p.id = s.project_id WHERE s.id = $1`, id).
		Scan(&p.ID, &p.Slug, &p.Name, &p.StatusPublic,
			&sv.ID, &sv.Slug, &sv.Image, &sv.HealthPath, &sv.VolumePath, &sv.Port, &sv.CPUMillis, &sv.MemoryMB, &sv.Env, &sv.Worker, &sv.Instances, &sv.Ports)
	if sv.Env == nil {
		sv.Env = map[string]string{}
	}
	return &p, &sv, err
}

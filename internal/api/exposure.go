package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Exposure settings of a service: extra public HTTP ports (each on <label>.<address>) and,
// for its custom domains, paths served by other services of the organization. Whether
// the service is public at all is a service setting (services.public).

var (
	portLabelRe  = regexp.MustCompile(`^[a-z][a-z0-9]{0,19}$`)
	pathPrefixRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{1,200}$`)
)

const maxServicePorts = 5

type portView struct {
	Label string `json:"label"`
	Port  int    `json:"port"`
	URL   string `json:"url"`
}

type pathView struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
	Target string `json:"service"` // project/service
	Strip  bool   `json:"strip_prefix"`
}

func (s *Server) loadServicePorts(ctx context.Context, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) ([]portView, error) {
	rows, err := tx.Query(ctx, `SELECT label, port FROM service_ports WHERE service_id = $1 ORDER BY label`, sv.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []portView{}
	for rows.Next() {
		var v portView
		if err := rows.Scan(&v.Label, &v.Port); err != nil {
			return nil, err
		}
		v.URL = s.appURL(v.Label + "." + s.serviceHost(o, p, sv))
		out = append(out, v)
	}
	return out, rows.Err()
}

// loadDomainPaths returns the path rules of the service's domains, by domain ID.
func loadDomainPaths(ctx context.Context, tx pgx.Tx, serviceID string) (map[string][]pathView, error) {
	rows, err := tx.Query(ctx, `
		SELECT dp.domain_id::text, dp.id::text, dp.prefix, p.slug || '/' || t.slug, dp.strip
		FROM domain_paths dp JOIN domains dm ON dm.id = dp.domain_id
		JOIN services t ON t.id = dp.service_id JOIN projects p ON p.id = t.project_id
		WHERE dm.service_id = $1 ORDER BY dp.prefix`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]pathView{}
	for rows.Next() {
		var did string
		var v pathView
		if err := rows.Scan(&did, &v.ID, &v.Prefix, &v.Target, &v.Strip); err != nil {
			return nil, err
		}
		out[did] = append(out[did], v)
	}
	return out, rows.Err()
}

func (s *Server) addServicePortTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, label string, port int) error {
	label = strings.ToLower(strings.TrimSpace(label))
	switch {
	case sv.isPreview():
		return apiErr{http.StatusUnprocessableEntity, "previews publish their main port only"}
	case sv.Worker:
		return apiErr{http.StatusUnprocessableEntity, "a worker has no public address; make it a web service first"}
	case !portLabelRe.MatchString(label) || strings.HasPrefix(label, "pr"):
		return apiErr{http.StatusUnprocessableEntity, "label: a lowercase name of up to 20 letters and digits (not starting with \"pr\"), such as admin"}
	case port < 1 || port > 65535:
		return apiErr{http.StatusUnprocessableEntity, "port: 1 to 65535"}
	case port == sv.Port:
		return apiErr{http.StatusUnprocessableEntity, "this is already the service's main port"}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM service_ports WHERE service_id = $1`, sv.ID).Scan(&n); err != nil {
		return err
	}
	if n >= maxServicePorts {
		return apiErr{http.StatusUnprocessableEntity, "at most " + strconv.Itoa(maxServicePorts) + " extra ports per service"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO service_ports (service_id, org_id, label, port) VALUES ($1, $2, $3, $4)`, sv.ID, o.ID, label, port); err != nil {
		if isUniqueViolation(err) {
			return apiErr{http.StatusConflict, "this label or port is already published"}
		}
		return err
	}
	s.audit(r, tx, o.ID, "service.port_published", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"label": label, "port": port})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

func (s *Server) deleteServicePortTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, label string) error {
	tag, err := tx.Exec(r.Context(), `DELETE FROM service_ports WHERE service_id = $1 AND label = $2`, sv.ID, label)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apiErr{http.StatusNotFound, "no such port"}
	}
	s.audit(r, tx, o.ID, "service.port_unpublished", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"label": label})
	_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

func (s *Server) addDomainPathTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo, domainID, prefix, target string, strip bool) error {
	prefix = strings.TrimSpace(prefix)
	if !pathPrefixRe.MatchString(prefix) || strings.HasSuffix(prefix, "/") || strings.Contains(prefix, "//") {
		return apiErr{http.StatusUnprocessableEntity, "path: starts with / and does not end with /, such as /api"}
	}
	var host string
	if err := tx.QueryRow(r.Context(), `SELECT hostname FROM domains WHERE id::text = $1 AND service_id = $2`, domainID, sv.ID).Scan(&host); err != nil {
		return apiErr{http.StatusNotFound, "domain not found"}
	}
	ps, ss, _ := strings.Cut(strings.TrimSpace(target), "/")
	var targetID string
	if err := tx.QueryRow(r.Context(), `SELECT t.id FROM services t JOIN projects tp ON tp.id = t.project_id
		WHERE tp.slug = $1 AND t.slug = $2 AND t.preview_of IS NULL AND NOT t.worker`, ps, ss).Scan(&targetID); err != nil {
		return apiErr{http.StatusUnprocessableEntity, "service: choose a web service of the organization"}
	}
	if targetID == sv.ID {
		return apiErr{http.StatusUnprocessableEntity, "the domain already serves this service; choose another one"}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM domain_paths WHERE domain_id::text = $1`, domainID).Scan(&n); err != nil {
		return err
	}
	if n >= 20 {
		return apiErr{http.StatusUnprocessableEntity, "at most 20 paths per domain"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO domain_paths (org_id, domain_id, prefix, service_id, strip) VALUES ($1, $2, $3, $4, $5)`,
		o.ID, domainID, prefix, targetID, strip); err != nil {
		if isUniqueViolation(err) {
			return apiErr{http.StatusConflict, "this path is already routed"}
		}
		return err
	}
	s.audit(r, tx, o.ID, "domain.path_added", "domain:"+host, map[string]any{"path": prefix, "service": target, "strip": strip})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

func (s *Server) deleteDomainPathTx(r *http.Request, tx pgx.Tx, o *orgInfo, sv *serviceInfo, pathID string) error {
	var host, prefix string
	if err := tx.QueryRow(r.Context(), `
		DELETE FROM domain_paths dp USING domains dm WHERE dp.id::text = $1 AND dm.id = dp.domain_id AND dm.service_id = $2
		RETURNING dm.hostname, dp.prefix`, pathID, sv.ID).Scan(&host, &prefix); err != nil {
		return apiErr{http.StatusNotFound, "path not found"}
	}
	s.audit(r, tx, o.ID, "domain.path_removed", "domain:"+host, map[string]any{"path": prefix})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

// ---------- console ----------

func (s *Server) exposureForm(w http.ResponseWriter, r *http.Request, anchor, ok string, fn func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error) {
	var formErr string
	o, p, sv, err, done := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		err := fn(tx, o, p, sv)
		var ae apiErr
		if errors.As(err, &ae) {
			formErr = ae.msg
			return nil
		}
		return err
	})
	if !done {
		return
	}
	switch {
	case err != nil:
		s.serverError(w, r, err)
	case formErr != "":
		back(w, r, svcPath(o, p, sv)+"#"+anchor, "", formErr+".")
	default:
		back(w, r, svcPath(o, p, sv)+"#"+anchor, ok, "")
	}
}

func (s *Server) addServicePortForm(w http.ResponseWriter, r *http.Request) {
	label := r.PostFormValue("label")
	s.exposureForm(w, r, "domains", "Port published at "+strings.ToLower(label)+".<the service's address>; it answers within a minute.",
		func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
			return s.addServicePortTx(r, tx, o, p, sv, label, atoi(r.PostFormValue("port")))
		})
}

func (s *Server) deleteServicePortForm(w http.ResponseWriter, r *http.Request) {
	s.exposureForm(w, r, "domains", "Port unpublished.", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		return s.deleteServicePortTx(r, tx, o, p, sv, r.PathValue("label"))
	})
}

func (s *Server) addDomainPathForm(w http.ResponseWriter, r *http.Request) {
	s.exposureForm(w, r, "domains", "Path routed; it applies within a minute.", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		return s.addDomainPathTx(r, tx, o, p, sv, r.PathValue("id"), r.PostFormValue("prefix"), r.PostFormValue("target"), r.PostFormValue("strip") == "1")
	})
}

func (s *Server) deleteDomainPathForm(w http.ResponseWriter, r *http.Request) {
	s.exposureForm(w, r, "domains", "Path route removed.", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		return s.deleteDomainPathTx(r, tx, o, sv, r.PathValue("path"))
	})
}

// ---------- REST ----------

func (s *Server) apiServicePorts(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var out []portView
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		var err error
		out, err = s.loadServicePorts(r.Context(), tx, o, p, sv)
		return err
	}) {
		writeJSON(w, out)
	}
}

func (s *Server) apiAddServicePort(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var in struct {
		Label string `json:"label"`
		Port  int    `json:"port"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"label": "admin", "port": 9000}`)
		return
	}
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		return s.addServicePortTx(r, tx, o, p, sv, in.Label, in.Port)
	}) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) apiDeleteServicePort(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.apiService(w, r, o, func(tx pgx.Tx, p *projectInfo, sv *serviceInfo) error {
		return s.deleteServicePortTx(r, tx, o, p, sv, r.PathValue("label"))
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

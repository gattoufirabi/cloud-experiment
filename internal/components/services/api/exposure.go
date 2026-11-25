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

	"platform/internal/core/web"
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

func (s *API) loadServicePorts(ctx context.Context, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) ([]portView, error) {
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
		v.URL = s.AppURL(v.Label + "." + s.ServiceHost(o, p, sv))
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

func (s *API) addServicePortTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, label string, port int) error {
	label = strings.ToLower(strings.TrimSpace(label))
	switch {
	case sv.IsPreview():
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "previews publish their main port only"}
	case sv.Worker:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a worker has no public address; make it a web service first"}
	case !portLabelRe.MatchString(label) || strings.HasPrefix(label, "pr"):
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "label: a lowercase name of up to 20 letters and digits (not starting with \"pr\"), such as admin"}
	case port < 1 || port > 65535:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "port: 1 to 65535"}
	case port == sv.Port:
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "this is already the service's main port"}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM service_ports WHERE service_id = $1`, sv.ID).Scan(&n); err != nil {
		return err
	}
	if n >= maxServicePorts {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "at most " + strconv.Itoa(maxServicePorts) + " extra ports per service"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO service_ports (service_id, org_id, label, port) VALUES ($1, $2, $3, $4)`, sv.ID, o.ID, label, port); err != nil {
		if web.IsUniqueViolation(err) {
			return web.APIErr{Status: http.StatusConflict, Msg: "this label or port is already published"}
		}
		return err
	}
	s.Audit(r, tx, o.ID, "service.port_published", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"label": label, "port": port})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

func (s *API) deleteServicePortTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, label string) error {
	tag, err := tx.Exec(r.Context(), `DELETE FROM service_ports WHERE service_id = $1 AND label = $2`, sv.ID, label)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return web.APIErr{Status: http.StatusNotFound, Msg: "no such port"}
	}
	s.Audit(r, tx, o.ID, "service.port_unpublished", "service:"+p.Slug+"/"+sv.Slug, map[string]any{"label": label})
	_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

func (s *API) addDomainPathTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo, domainID, prefix, target string, strip bool) error {
	prefix = strings.TrimSpace(prefix)
	if !pathPrefixRe.MatchString(prefix) || strings.HasSuffix(prefix, "/") || strings.Contains(prefix, "//") {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "path: starts with / and does not end with /, such as /api"}
	}
	var host string
	if err := tx.QueryRow(r.Context(), `SELECT hostname FROM domains WHERE id::text = $1 AND service_id = $2`, domainID, sv.ID).Scan(&host); err != nil {
		return web.APIErr{Status: http.StatusNotFound, Msg: "domain not found"}
	}
	ps, ss, _ := strings.Cut(strings.TrimSpace(target), "/")
	var targetID string
	if err := tx.QueryRow(r.Context(), `SELECT t.id FROM services t JOIN projects tp ON tp.id = t.project_id
		WHERE tp.slug = $1 AND t.slug = $2 AND t.preview_of IS NULL AND NOT t.worker`, ps, ss).Scan(&targetID); err != nil {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "service: choose a web service of the organization"}
	}
	if targetID == sv.ID {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "the domain already serves this service; choose another one"}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM domain_paths WHERE domain_id::text = $1`, domainID).Scan(&n); err != nil {
		return err
	}
	if n >= 20 {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "at most 20 paths per domain"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO domain_paths (org_id, domain_id, prefix, service_id, strip) VALUES ($1, $2, $3, $4, $5)`,
		o.ID, domainID, prefix, targetID, strip); err != nil {
		if web.IsUniqueViolation(err) {
			return web.APIErr{Status: http.StatusConflict, Msg: "this path is already routed"}
		}
		return err
	}
	s.Audit(r, tx, o.ID, "domain.path_added", "domain:"+host, map[string]any{"path": prefix, "service": target, "strip": strip})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

func (s *API) deleteDomainPathTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, sv *web.ServiceInfo, pathID string) error {
	var host, prefix string
	if err := tx.QueryRow(r.Context(), `
		DELETE FROM domain_paths dp USING domains dm WHERE dp.id::text = $1 AND dm.id = dp.domain_id AND dm.service_id = $2
		RETURNING dm.hostname, dp.prefix`, pathID, sv.ID).Scan(&host, &prefix); err != nil {
		return web.APIErr{Status: http.StatusNotFound, Msg: "path not found"}
	}
	s.Audit(r, tx, o.ID, "domain.path_removed", "domain:"+host, map[string]any{"path": prefix})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
	return err
}

// ---------- console ----------

func (s *API) exposureForm(w http.ResponseWriter, r *http.Request, anchor, ok string, fn func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error) {
	var formErr string
	o, p, sv, err, done := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		err := fn(tx, o, p, sv)
		var ae web.APIErr
		if errors.As(err, &ae) {
			formErr = ae.Msg
			return nil
		}
		return err
	})
	if !done {
		return
	}
	switch {
	case err != nil:
		s.ServerError(w, r, err)
	case formErr != "":
		web.Back(w, r, web.SvcPath(o, p, sv)+"#"+anchor, "", formErr+".")
	default:
		web.Back(w, r, web.SvcPath(o, p, sv)+"#"+anchor, ok, "")
	}
}

func (s *API) addServicePortForm(w http.ResponseWriter, r *http.Request) {
	label := r.PostFormValue("label")
	s.exposureForm(w, r, "domains", "Port published at "+strings.ToLower(label)+".<the service's address>; it answers within a minute.",
		func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
			return s.addServicePortTx(r, tx, o, p, sv, label, web.Atoi(r.PostFormValue("port")))
		})
}

func (s *API) deleteServicePortForm(w http.ResponseWriter, r *http.Request) {
	s.exposureForm(w, r, "domains", "Port unpublished.", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.deleteServicePortTx(r, tx, o, p, sv, r.PathValue("label"))
	})
}

func (s *API) addDomainPathForm(w http.ResponseWriter, r *http.Request) {
	s.exposureForm(w, r, "domains", "Path routed; it applies within a minute.", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.addDomainPathTx(r, tx, o, p, sv, r.PathValue("id"), r.PostFormValue("prefix"), r.PostFormValue("target"), r.PostFormValue("strip") == "1")
	})
}

func (s *API) deleteDomainPathForm(w http.ResponseWriter, r *http.Request) {
	s.exposureForm(w, r, "domains", "Path route removed.", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.deleteDomainPathTx(r, tx, o, sv, r.PathValue("path"))
	})
}

// ---------- REST ----------

func (s *API) apiServicePorts(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []portView
	if s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		out, err = s.loadServicePorts(r.Context(), tx, o, p, sv)
		return err
	}) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiAddServicePort(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Label string `json:"label"`
		Port  int    `json:"port"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"label": "admin", "port": 9000}`)
		return
	}
	if s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.addServicePortTx(r, tx, o, p, sv, in.Label, in.Port)
	}) {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *API) apiDeleteServicePort(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.deleteServicePortTx(r, tx, o, p, sv, r.PathValue("label"))
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// setDefaultAddress turns the service's platform address off (only its verified custom
// domains answer) or back on. It can be turned off only while a custom domain is verified;
// with none left, the address answers again on its own.
func (s *API) setDefaultAddress(w http.ResponseWriter, r *http.Request) {
	v := strings.ToLower(r.PostFormValue("on")) // "1"/"0" from the console, true/false from the API
	on := v == "1" || v == "true" || v == "on"
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if sv.IsPreview() || sv.Worker {
			formErr = "Only a web service has a platform address to turn off."
			return nil
		}
		if !on {
			var n int
			if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM domains WHERE service_id = $1 AND verified_at IS NOT NULL`, sv.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				formErr = "Verify a custom domain first: without one, the service would have no address."
				return nil
			}
		}
		if _, err := tx.Exec(r.Context(), `UPDATE services SET default_address = $2 WHERE id = $1`, sv.ID, on); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "service.default_address", "service:"+sv.Slug, map[string]bool{"on": on})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	msg := "The platform address answers again."
	if !on {
		msg = "The platform address is off: the service answers only on its custom domains."
	}
	if formErr != "" {
		msg = ""
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#domains", msg, formErr)
}

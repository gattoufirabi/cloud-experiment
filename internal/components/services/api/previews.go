package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
)

type previewView struct {
	Name, Slug, Image, Status, URL string
	Expires                        time.Time
	Created                        time.Time
}

func (s *API) loadPreviews(r *http.Request, tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, parent *web.ServiceInfo) ([]previewView, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT s.preview_name, s.slug, s.image, coalesce(d.status, 'pending'), s.expires_at, s.created_at
		FROM services s
		LEFT JOIN LATERAL (SELECT status FROM deployments WHERE service_id = s.id ORDER BY revision DESC LIMIT 1) d ON true
		WHERE s.preview_of = $1 ORDER BY s.preview_name`, parent.ID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (previewView, error) {
		var v previewView
		err := row.Scan(&v.Name, &v.Slug, &v.Image, &v.Status, &v.Expires, &v.Created)
		v.URL = s.AppURL(s.PreviewHost(v.Name, parent.Slug, p.Slug, o.Slug))
		return v, err
	})
	return out, err
}

// ---------- console ----------

func (s *API) createPreviewForm(w http.ResponseWriter, r *http.Request) {
	var res web.PreviewResult
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if sv.IsPreview() {
			formErr = "A preview cannot have previews."
			return nil
		}
		var err error
		res, err = s.UpsertPreview(r, tx, o, p, sv, web.PreviewRequest{
			Name: r.PostFormValue("name"), Image: r.PostFormValue("image"), Hours: web.Atoi(r.PostFormValue("ttl_hours")),
		})
		var ae web.APIErr
		if errors.As(err, &ae) {
			formErr = ae.Msg
			return nil
		}
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#previews", "", formErr)
		return
	}
	msg := "Preview " + res.Name + " is deploying at " + res.URL
	if !res.Created {
		msg = "Preview " + res.Name + " is redeploying; it now expires " + res.Expires.UTC().Format("2006-01-02 15:04 UTC")
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#previews", msg, "")
}

func (s *API) deletePreviewForm(w http.ResponseWriter, r *http.Request) {
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		return s.DeletePreviewTx(r, tx, o, p, sv, r.PathValue("name"))
	})
	if !ok {
		return
	}
	if web.IsNoRows(err) {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#previews", "", "No such preview.")
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"#previews", "Preview deleted. Its container is being removed.", "")
}

// ---------- API v1 ----------

type apiPreview struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Image    string    `json:"image"`
	Status   string    `json:"status"`
	Expires  time.Time `json:"expires_at"`
	Revision int       `json:"revision,omitempty"`
}

func (s *API) apiPreviews(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out []apiPreview
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		list, err := s.loadPreviews(r, tx, o, p, sv)
		for _, v := range list {
			out = append(out, apiPreview{Name: v.Name, URL: v.URL, Image: v.Image, Status: v.Status, Expires: v.Expires})
		}
		return err
	}) {
		return
	}
	if out == nil {
		out = []apiPreview{}
	}
	web.WriteJSON(w, out)
}

// apiCreatePreview: POST .../previews {"name": "pr-42", "image": "...", "ttl_hours": 72}
func (s *API) apiCreatePreview(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var req web.PreviewRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON, for example {"name": "pr-42", "image": "nginx:1.27-alpine"}`)
		return
	}
	var res web.PreviewResult
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		if sv.IsPreview() {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a preview cannot have previews"}
		}
		var err error
		res, err = s.UpsertPreview(r, tx, o, p, sv, req)
		return err
	}) {
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiPreview{Name: res.Name, URL: res.URL, Image: req.Image, Status: "pending", Expires: res.Expires, Revision: res.Revision})
}

func (s *API) apiDeletePreview(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		err := s.DeletePreviewTx(r, tx, o, p, sv, r.PathValue("name"))
		if web.IsNoRows(err) {
			return web.APIErr{Status: http.StatusNotFound, Msg: "preview not found"}
		}
		return err
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

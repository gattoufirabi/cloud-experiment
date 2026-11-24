package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
)

// zipDeployForm: the service's Builds tab uploads an archive.
func (s *API) zipDeployForm(w http.ResponseWriter, r *http.Request) {
	var n int
	var formErr string
	o, p, sv, err, ok := s.WithService(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		up, hdr, ferr := r.FormFile("archive")
		if ferr != nil {
			formErr = "Choose a zip archive of your application's source."
			return nil
		}
		defer up.Close()
		b, err := io.ReadAll(io.LimitReader(up, web.MaxZipDeployBytes+1))
		if err != nil {
			return err
		}
		n, err = s.QueueZipBuild(r, tx, o, sv.ID, b, hdr.Filename, r.PostFormValue("deploy") == "1", web.SessionFrom(r).UserID)
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
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			web.Back(w, r, web.SvcPath(o, p, sv)+"#builds", "", fmt.Sprintf("The archive is larger than %d MB.", web.MaxZipDeployBytes>>20))
			return
		}
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, web.SvcPath(o, p, sv)+"#builds", "", formErr)
		return
	}
	web.Back(w, r, web.SvcPath(o, p, sv)+"/builds/"+web.Itoa(n), "Archive uploaded: build #"+web.Itoa(n)+" queued.", "")
}

// apiZipDeploy: POST the archive as the body (Content-Type application/zip);
// ?deploy=false only builds and pushes the image; ?filename= names it in the log.
func (s *API) apiZipDeploy(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	b, err := io.ReadAll(io.LimitReader(r.Body, web.MaxZipDeployBytes+1))
	if err != nil {
		web.APIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the archive is larger than %d MB", web.MaxZipDeployBytes>>20))
		return
	}
	deploy := r.URL.Query().Get("deploy") != "false"
	var n int
	var statusPath string
	if !s.APIService(w, r, o, func(tx pgx.Tx, p *web.ProjectInfo, sv *web.ServiceInfo) error {
		var err error
		n, err = s.QueueZipBuild(r, tx, o, sv.ID, b, r.URL.Query().Get("filename"), deploy, web.SessionFrom(r).UserID)
		statusPath = "/api/v1/projects/" + p.Slug + "/services/" + sv.Slug + "/builds/" + web.Itoa(n)
		return err
	}) {
		return
	}
	w.Header().Set("Location", statusPath)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "status": "queued", "status_url": statusPath})
}

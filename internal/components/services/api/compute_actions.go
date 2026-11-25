package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/registry"
)

// Compute area: a complete "Create service" form (name, source, type, resources,
// instances, variables, deploy now) and actions on several selected services at once.

func (s *API) createServiceFull(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	project := r.PostFormValue("project")
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	formBack := o.Path() + "/compute#create"
	if !model.ValidSlug(slug) {
		web.Back(w, r, formBack, "", "Service name: 2 to 20 characters, lowercase letters, digits and dashes.")
		return
	}
	env, err := model.ParseEnv(r.PostFormValue("env"))
	if err != nil {
		web.Back(w, r, formBack, "", "Environment: "+err.Error())
		return
	}
	spec := model.Spec{
		Image:      strings.TrimSpace(r.PostFormValue("image")),
		Port:       web.AtoiDefault(r.PostFormValue("port"), 8080),
		CPUMillis:  web.AtoiDefault(r.PostFormValue("cpu_millis"), 250),
		MemoryMB:   web.AtoiDefault(r.PostFormValue("memory_mb"), 256),
		HealthPath: strings.TrimSpace(r.PostFormValue("health_path")),
		Env:        env,
		Worker:     r.PostFormValue("kind") == "worker",
		Instances:  web.AtoiDefault(r.PostFormValue("instances"), 1),
	}
	if spec.HealthPath == "" || (spec.Worker && !strings.HasPrefix(spec.HealthPath, "/")) {
		spec.HealthPath = "/"
	}
	deployNow := r.PostFormValue("deploy") == "1" && spec.Image != ""
	var formErr string
	err = db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var projectID string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM projects WHERE slug = $1`, project).Scan(&projectID); err != nil {
			formErr = "Choose a project."
			return nil
		}
		check := spec
		if check.Image == "" {
			check.Image = "placeholder:latest" // no image yet: validate the rest
		}
		if err := check.Validate(); err != nil {
			formErr = err.Error()
			return nil
		}
		if err := registry.CheckNamespace(spec.Image, s.Cfg.RegistryHost, o.Slug); spec.Image != "" && err != nil {
			formErr = err.Error()
			return nil
		}
		for _, c := range []func() error{
			func() error { return s.CheckInstances(r, tx, o, spec.Count()) },
			func() error {
				return s.CheckQuota(r, tx, o, "", spec.CPUMillis*spec.Count(), spec.MemoryMB*spec.Count(), true)
			},
		} {
			if err := c(); err != nil {
				var qe web.QuotaError
				if errors.As(err, &qe) {
					formErr = qe.Error()
					return nil
				}
				return err
			}
		}
		envJSON, _ := json.Marshal(spec.Env)
		var id string
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO services (org_id, project_id, slug, image, port, cpu_millis, memory_mb, health_path, env, worker, instances)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
			o.ID, projectID, slug, spec.Image, spec.Port, spec.CPUMillis, spec.MemoryMB, spec.HealthPath, envJSON, spec.Worker, spec.Count()).Scan(&id); err != nil {
			if web.IsUniqueViolation(err) {
				formErr = "A service with this name already exists in the project."
				return web.ErrRollback
			}
			return err
		}
		s.Audit(r, tx, o.ID, "service.created", "service:"+project+"/"+slug, map[string]any{"image": spec.Image, "worker": spec.Worker, "instances": spec.Count()})
		if deployNow {
			if err := s.CheckDisk(r, tx, o); err != nil {
				var qe web.QuotaError
				if errors.As(err, &qe) {
					formErr = qe.Error()
					return nil
				}
				return err
			}
			_, err := s.CreateDeployment(r, tx, o, id, spec)
			return err
		}
		return nil
	})
	switch {
	case formErr != "":
		web.Back(w, r, formBack, "", formErr)
	case err != nil:
		s.ServerError(w, r, err)
	case deployNow:
		web.Back(w, r, o.Path()+"/p/"+project+"/s/"+slug, "Service created. The first deployment has started.", "")
	default:
		web.Back(w, r, o.Path()+"/p/"+project+"/s/"+slug, "Service created. Set its image or source, then deploy.", "")
	}
}

// computeActions applies an action to the services selected in the list
// ("sel" = "project/service"): redeploy, or delete (after typing "delete").
func (s *API) computeActions(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		web.Back(w, r, o.Path()+"/compute", "", "Invalid form.")
		return
	}
	sel, action := r.PostForm["sel"], r.PostFormValue("action")
	switch {
	case len(sel) == 0:
		web.Back(w, r, o.Path()+"/compute", "", "Select at least one service.")
		return
	case action != "redeploy" && action != "delete":
		web.Back(w, r, o.Path()+"/compute", "", "Choose an action.")
		return
	case action == "delete" && r.PostFormValue("confirm") != "delete":
		web.Back(w, r, o.Path()+"/compute", "", `Type "delete" to confirm deleting the selected services.`)
		return
	}
	var done, skipped []string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		if action == "redeploy" {
			if err := s.CheckDisk(r, tx, o); err != nil {
				return err
			}
		}
		for _, item := range sel {
			ps, ss, _ := strings.Cut(item, "/")
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT s.id FROM services s JOIN projects p ON p.id = s.project_id WHERE p.slug = $1 AND s.slug = $2 AND s.preview_of IS NULL`,
				ps, ss).Scan(&id); err != nil {
				if web.IsNoRows(err) {
					skipped = append(skipped, item)
					continue
				}
				return err
			}
			switch action {
			case "redeploy":
				_, sv, err := s.LoadServiceByID(r, tx, id)
				if err != nil {
					return err
				}
				if sv.Image == "" {
					skipped = append(skipped, item+" (no image)")
					continue
				}
				if _, err := s.CreateDeployment(r, tx, o, id, sv.Spec()); err != nil {
					skipped = append(skipped, item+" ("+err.Error()+")")
					continue
				}
			case "delete":
				if _, err := tx.Exec(r.Context(), `DELETE FROM services WHERE id = $1`, id); err != nil {
					return err
				}
				s.Audit(r, tx, o.ID, "service.deleted", "service:"+item, map[string]string{"via": "bulk action"})
			}
			done = append(done, item)
		}
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	var qe web.QuotaError
	switch {
	case errors.As(err, &qe):
		web.Back(w, r, o.Path()+"/compute", "", qe.Error())
		return
	case err != nil:
		s.ServerError(w, r, err)
		return
	}
	verb := map[string]string{"redeploy": "Redeploying", "delete": "Deleted"}[action]
	msg := fmt.Sprintf("%s %d service(s): %s.", verb, len(done), strings.Join(done, ", "))
	if len(done) == 0 {
		msg = ""
	}
	errMsg := ""
	if len(skipped) > 0 {
		errMsg = "Skipped: " + strings.Join(skipped, ", ")
	}
	web.Back(w, r, o.Path()+"/compute", msg, errMsg)
}

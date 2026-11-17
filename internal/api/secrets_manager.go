package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
)

// Secrets manager: organization-wide encrypted, versioned secrets. Services and
// functions use one through a variable whose value is ${secret:NAME}.

var orgSecretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

const (
	maxSecretValue    = 64 << 10
	keptSecretVersion = 10 // older versions are deleted
)

type orgSecret struct {
	ID, Name, Description string
	Version               int
	Created, Updated      time.Time
	UsedBy                []string // "project/service" or "project/function (function)"
	Versions              []orgSecretVersion
}

type orgSecretVersion struct {
	Version int
	Created time.Time
	By      string
}

// Ref is the variable value that references the secret.
func (s *orgSecret) Ref() string { return "${secret:" + s.Name + "}" }

func loadOrgSecrets(ctx context.Context, tx pgx.Tx) ([]*orgSecret, error) {
	rows, err := tx.Query(ctx, `SELECT id, name, description, current_version, created_at, updated_at FROM org_secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*orgSecret
	for rows.Next() {
		var s orgSecret
		if err := rows.Scan(&s.ID, &s.Name, &s.Description, &s.Version, &s.Created, &s.Updated); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, s := range out {
		if s.UsedBy, err = secretUsers(ctx, tx, s.Name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func loadOrgSecret(ctx context.Context, tx pgx.Tx, name string) (*orgSecret, error) {
	var s orgSecret
	err := tx.QueryRow(ctx, `SELECT id, name, description, current_version, created_at, updated_at FROM org_secrets WHERE name = $1`, name).
		Scan(&s.ID, &s.Name, &s.Description, &s.Version, &s.Created, &s.Updated)
	if err != nil {
		return nil, err
	}
	if s.UsedBy, err = secretUsers(ctx, tx, s.Name); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT v.version, v.created_at, coalesce(u.email, '') FROM org_secret_versions v LEFT JOIN users u ON u.id = v.created_by
		WHERE v.secret_id = $1 ORDER BY v.version DESC`, s.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v orgSecretVersion
		if err := rows.Scan(&v.Version, &v.Created, &v.By); err != nil {
			return nil, err
		}
		s.Versions = append(s.Versions, v)
	}
	return &s, rows.Err()
}

// secretUsers lists the services and functions with a variable referencing the secret.
func secretUsers(ctx context.Context, tx pgx.Tx, name string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.slug || '/' || s.slug FROM services s JOIN projects p ON p.id = s.project_id
		WHERE EXISTS (SELECT 1 FROM jsonb_each_text(s.env) e WHERE e.value = $1)
		UNION ALL
		SELECT p.slug || '/' || f.slug || ' (function)' FROM functions f JOIN projects p ON p.id = f.project_id
		WHERE EXISTS (SELECT 1 FROM jsonb_each_text(f.env) e WHERE e.value = $1)
		ORDER BY 1`, "${secret:"+name+"}")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func validSecretValue(v string) error {
	switch {
	case v == "":
		return apiErr{http.StatusUnprocessableEntity, "the secret value is empty"}
	case len(v) > maxSecretValue:
		return apiErr{http.StatusUnprocessableEntity, "the secret value is larger than 64 KB"}
	}
	return nil
}

func (s *Server) createOrgSecretTx(r *http.Request, tx pgx.Tx, o *orgInfo, name, desc, value string) error {
	if !orgSecretNameRe.MatchString(name) {
		return apiErr{http.StatusUnprocessableEntity, "secret name: up to 64 letters, digits, dots, dashes and underscores"}
	}
	if len(desc) > 200 {
		return apiErr{http.StatusUnprocessableEntity, "description: at most 200 characters"}
	}
	if err := validSecretValue(value); err != nil {
		return err
	}
	var n, max int
	if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM org_secrets), max_secrets FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return err
	}
	if n >= max {
		return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d secrets", max)}
	}
	var id string
	if err := tx.QueryRow(r.Context(), `INSERT INTO org_secrets (org_id, name, description) VALUES ($1, $2, $3) RETURNING id`, o.ID, name, desc).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			return apiErr{http.StatusConflict, "a secret with this name already exists"}
		}
		return err
	}
	if err := s.insertSecretVersion(r, tx, o, id, 1, value); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "secret.created", "secret:"+name, nil)
	return nil
}

func (s *Server) insertSecretVersion(r *http.Request, tx pgx.Tx, o *orgInfo, id string, version int, value string) error {
	enc, err := s.box.Seal([]byte(value), "orgsecret:"+id+":"+strconv.Itoa(version))
	if err != nil {
		return err
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO org_secret_versions (secret_id, org_id, version, value_enc, created_by) VALUES ($1, $2, $3, $4, nullif($5, '')::uuid)`,
		id, o.ID, version, enc, sessionFrom(r).UserID)
	return err
}

// rotateOrgSecretTx stores a new current version, redeploys the live services that
// use the secret and lets warm functions restart with it. Returns how many services
// were redeployed.
func (s *Server) rotateOrgSecretTx(r *http.Request, tx pgx.Tx, o *orgInfo, sec *orgSecret, value string) (int, error) {
	if err := validSecretValue(value); err != nil {
		return 0, err
	}
	next := sec.Version + 1
	if err := s.insertSecretVersion(r, tx, o, sec.ID, next, value); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(r.Context(), `UPDATE org_secrets SET current_version = $2, updated_at = now() WHERE id = $1`, sec.ID, next); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM org_secret_versions WHERE secret_id = $1 AND version <= $2`, sec.ID, next-keptSecretVersion); err != nil {
		return 0, err
	}
	n, err := s.redeploySecretUsers(r, tx, o, sec.Ref())
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'fn')`); err != nil {
		return 0, err
	}
	s.audit(r, tx, o.ID, "secret.rotated", "secret:"+sec.Name, map[string]int{"version": next, "services_redeployed": n})
	sec.Version = next
	return n, nil
}

// redeploySecretUsers starts a new deployment of every live service whose current
// settings reference the secret.
func (s *Server) redeploySecretUsers(r *http.Request, tx pgx.Tx, o *orgInfo, ref string) (int, error) {
	rows, err := tx.Query(r.Context(), `
		SELECT d.service_id, d.spec FROM deployments d
		WHERE d.status = 'live' AND EXISTS (SELECT 1 FROM jsonb_each_text(d.spec->'env') e WHERE e.value = $1)
		  AND NOT EXISTS (SELECT 1 FROM deployments x WHERE x.service_id = d.service_id AND x.status IN ('pending', 'deploying'))`, ref)
	if err != nil {
		return 0, err
	}
	type live struct {
		id   string
		spec []byte
	}
	var lives []live
	for rows.Next() {
		var l live
		if err := rows.Scan(&l.id, &l.spec); err != nil {
			rows.Close()
			return 0, err
		}
		lives = append(lives, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(lives) > 0 {
		if err := s.checkDisk(r, tx, o); err != nil {
			return 0, err
		}
	}
	for i, l := range lives {
		var spec model.Spec
		if err := json.Unmarshal(l.spec, &spec); err != nil {
			return i, err
		}
		if _, err := s.createDeployment(r, tx, o, l.id, spec); err != nil {
			return i, err
		}
	}
	return len(lives), nil
}

// readOrgSecret decrypts a version (0 = current) and audits the read.
func (s *Server) readOrgSecret(r *http.Request, tx pgx.Tx, o *orgInfo, sec *orgSecret, version int) (string, int, error) {
	if version == 0 {
		version = sec.Version
	}
	var enc []byte
	if err := tx.QueryRow(r.Context(), `SELECT value_enc FROM org_secret_versions WHERE secret_id = $1 AND version = $2`, sec.ID, version).Scan(&enc); err != nil {
		if isNoRows(err) {
			return "", 0, apiErr{http.StatusNotFound, "no such version (the last 10 are kept)"}
		}
		return "", 0, err
	}
	plain, err := s.box.Open(enc, "orgsecret:"+sec.ID+":"+strconv.Itoa(version))
	if err != nil {
		return "", 0, err
	}
	s.audit(r, tx, o.ID, "secret.read", "secret:"+sec.Name, map[string]int{"version": version})
	return string(plain), version, nil
}

func (s *Server) deleteOrgSecretTx(r *http.Request, tx pgx.Tx, o *orgInfo, sec *orgSecret) error {
	if len(sec.UsedBy) > 0 {
		return apiErr{http.StatusConflict, "the secret is used by " + strings.Join(sec.UsedBy, ", ") + "; remove those references first"}
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM org_secrets WHERE id = $1`, sec.ID); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "secret.deleted", "secret:"+sec.Name, nil)
	return nil
}

// ---------- REST API ----------

type apiOrgSecret struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     int       `json:"version"`
	Reference   string    `json:"reference"`
	UsedBy      []string  `json:"used_by"`
	Created     time.Time `json:"created_at"`
	Updated     time.Time `json:"updated_at"`
}

func orgSecretView(sec *orgSecret) apiOrgSecret {
	return apiOrgSecret{sec.Name, sec.Description, sec.Version, sec.Ref(), sec.UsedBy, sec.Created, sec.Updated}
}

func (s *Server) apiOrgSecrets(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	out := []apiOrgSecret{}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		secs, err := loadOrgSecrets(r.Context(), tx)
		for _, sec := range secs {
			out = append(out, orgSecretView(sec))
		}
		return err
	})
	if apiTx(w, s, err) {
		writeJSON(w, out)
	}
}

type orgSecretBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Value       string `json:"value"`
}

func (s *Server) apiCreateOrgSecret(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var b orgSecretBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSecretValue+4096)).Decode(&b); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"name": "stripe-key", "value": "..."}`)
		return
	}
	var view apiOrgSecret
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if err := s.createOrgSecretTx(r, tx, o, b.Name, b.Description, b.Value); err != nil {
			return err
		}
		sec, err := loadOrgSecret(r.Context(), tx, b.Name)
		if err == nil {
			view = orgSecretView(sec)
		}
		return err
	})
	if apiTx(w, s, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(view)
	}
}

func (s *Server) apiGetOrgSecret(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	var out struct {
		apiOrgSecret
		Value        string `json:"value"`
		ValueVersion int    `json:"value_version"`
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		sec, err := loadOrgSecret(r.Context(), tx, r.PathValue("secret"))
		if err != nil {
			return err
		}
		out.apiOrgSecret = orgSecretView(sec)
		out.Value, out.ValueVersion, err = s.readOrgSecret(r, tx, o, sec, version)
		return err
	})
	if apiTx(w, s, err) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, out)
	}
}

func (s *Server) apiPutOrgSecret(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var b orgSecretBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSecretValue+4096)).Decode(&b); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"value": "..."}`)
		return
	}
	var out struct {
		apiOrgSecret
		Redeployed int `json:"services_redeployed"`
	}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		sec, err := loadOrgSecret(r.Context(), tx, r.PathValue("secret"))
		if err != nil {
			return err
		}
		if b.Description != "" {
			if len(b.Description) > 200 {
				return apiErr{http.StatusUnprocessableEntity, "description: at most 200 characters"}
			}
			if _, err := tx.Exec(r.Context(), `UPDATE org_secrets SET description = $2 WHERE id = $1`, sec.ID, b.Description); err != nil {
				return err
			}
			sec.Description = b.Description
		}
		if b.Value != "" {
			if out.Redeployed, err = s.rotateOrgSecretTx(r, tx, o, sec, b.Value); err != nil {
				return err
			}
		}
		out.apiOrgSecret = orgSecretView(sec)
		return nil
	})
	if apiTx(w, s, err) {
		writeJSON(w, out)
	}
}

func (s *Server) apiDeleteOrgSecret(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		sec, err := loadOrgSecret(r.Context(), tx, r.PathValue("secret"))
		if err != nil {
			return err
		}
		return s.deleteOrgSecretTx(r, tx, o, sec)
	})
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- console ----------

func (s *Server) orgSecretsPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	var secs []*orgSecret
	var max int
	if err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if secs, err = loadOrgSecrets(r.Context(), tx); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT max_secrets FROM plans WHERE id = $1`, o.PlanID).Scan(&max)
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "secrets", http.StatusOK, data{"Org": o, "Secrets": secs, "Max": max, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *Server) createOrgSecret(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		return s.createOrgSecretTx(r, tx, o, name, strings.TrimSpace(r.PostFormValue("description")), r.PostFormValue("value"))
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/secrets#create", "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/secrets/"+name, "Secret "+name+" created. Use it in a variable as ${secret:"+name+"}.", "")
	}
}

// withOrgSecret loads the secret named in the path and runs fn in the tenant context.
func (s *Server) withOrgSecret(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *orgInfo, sec *orgSecret) error) (*orgInfo, *orgSecret, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var sec *orgSecret
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if sec, err = loadOrgSecret(r.Context(), tx, r.PathValue("secret")); err != nil {
			return err
		}
		return fn(tx, o, sec)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, false
	}
	return o, sec, err, true
}

func (s *Server) orgSecretPage(w http.ResponseWriter, r *http.Request) {
	o, sec, err, ok := s.withOrgSecret(w, r, "viewer", func(pgx.Tx, *orgInfo, *orgSecret) error { return nil })
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "secret", http.StatusOK, data{"Org": o, "Secret": sec, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

// revealOrgSecret shows a version's value once, on a page that is not cached; the read is audited.
func (s *Server) revealOrgSecret(w http.ResponseWriter, r *http.Request) {
	var value string
	var version int
	o, sec, err, ok := s.withOrgSecret(w, r, "developer", func(tx pgx.Tx, o *orgInfo, sec *orgSecret) error {
		var err error
		value, version, err = s.readOrgSecret(r, tx, o, sec, atoi(r.PostFormValue("version")))
		return err
	})
	if !ok {
		return
	}
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/secrets/"+sec.Name, "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, r, "secret", http.StatusOK, data{"Org": o, "Secret": sec, "CanEdit": true, "Revealed": value, "RevealedVersion": version})
	}
}

func (s *Server) rotateOrgSecret(w http.ResponseWriter, r *http.Request) {
	var n int
	o, sec, err, ok := s.withOrgSecret(w, r, "developer", func(tx pgx.Tx, o *orgInfo, sec *orgSecret) error {
		var err error
		if d := strings.TrimSpace(r.PostFormValue("description")); r.PostForm.Has("description") && d != sec.Description {
			if len(d) > 200 {
				return apiErr{http.StatusUnprocessableEntity, "description: at most 200 characters"}
			}
			if _, err := tx.Exec(r.Context(), `UPDATE org_secrets SET description = $2 WHERE id = $1`, sec.ID, d); err != nil {
				return err
			}
		}
		if v := r.PostFormValue("value"); v != "" {
			n, err = s.rotateOrgSecretTx(r, tx, o, sec, v)
		}
		return err
	})
	if !ok {
		return
	}
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/secrets/"+sec.Name, "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	case r.PostFormValue("value") == "":
		back(w, r, o.Path()+"/secrets/"+sec.Name, "Description saved.", "")
	default:
		msg := fmt.Sprintf("Version %d is now current.", sec.Version)
		if n > 0 {
			msg += fmt.Sprintf(" %d service(s) redeploying with it.", n)
		}
		back(w, r, o.Path()+"/secrets/"+sec.Name, msg, "")
	}
}

func (s *Server) deleteOrgSecret(w http.ResponseWriter, r *http.Request) {
	o, sec, err, ok := s.withOrgSecret(w, r, "developer", func(tx pgx.Tx, o *orgInfo, sec *orgSecret) error {
		if r.PostFormValue("confirm") != sec.Name {
			return apiErr{http.StatusUnprocessableEntity, "type the secret's name to delete it"}
		}
		return s.deleteOrgSecretTx(r, tx, o, sec)
	})
	if !ok {
		return
	}
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/secrets/"+sec.Name, "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/secrets", "Secret "+sec.Name+" deleted.", "")
	}
}

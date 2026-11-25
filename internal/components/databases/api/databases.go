package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
)

func (s *API) withDatabase(w http.ResponseWriter, r *http.Request, minRole string,
	fn func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) error) (*web.OrgInfo, *web.ProjectInfo, *web.DatabaseInfo, error, bool) {
	o, ok := s.Org(w, r, minRole)
	if !ok {
		return nil, nil, nil, nil, false
	}
	var p *web.ProjectInfo
	var d *web.DatabaseInfo
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.LoadProject(r, tx); err != nil {
			return err
		}
		if d, err = s.LoadDatabase(r, tx, p.ID); err != nil {
			return err
		}
		return fn(tx, o, p, d)
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return nil, nil, nil, nil, false
	}
	return o, p, d, err, true
}

func dbPath(o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) string {
	return o.Path() + "/p/" + p.Slug + "/d/" + d.Slug
}

func (s *API) databasePage(w http.ResponseWriter, r *http.Request) {
	var services []string
	var backups *web.BackupView
	var pub dbPublic
	o, p, d, err, ok := s.withDatabase(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) error {
		rows, err := tx.Query(r.Context(), `SELECT slug FROM services WHERE project_id = $1 AND preview_of IS NULL ORDER BY slug`, p.ID)
		if err != nil {
			return err
		}
		if services, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		if backups, err = web.LoadTargetBackups(r, tx, "database_id", d.ID, d.BackupEnabled, d.BackupHour); err != nil {
			return err
		}
		pub, err = s.loadDBPublic(r, tx, o, p, d)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	canEdit := model.RoleAtLeast(o.Role, "developer")
	d2 := web.Data{
		"Org": o, "Project": p, "Database": d, "Services": services, "Backups": backups,
		"Path": dbPath(o, p, d), "CanEdit": canEdit, "CanRestore": model.RoleAtLeast(o.Role, "admin"),
		"Public": pub, "PublicEngine": publicEngine(d.Engine), "MyIP": web.ClientIP(r), "CanAdmin": model.RoleAtLeast(o.Role, "admin"),
	}
	if canEdit { // viewers never receive the password
		d2["Password"], d2["URL"] = s.DBPassword(d), s.DBURL(d)
	}
	s.Render(w, r, "database", http.StatusOK, d2)
}

func (s *API) databaseSettings(w http.ResponseWriter, r *http.Request) {
	var formErr string
	cpu := web.Atoi(r.PostFormValue("cpu_millis"))
	mem := web.Atoi(r.PostFormValue("memory_mb"))
	hour, herr := strconv.Atoi(r.PostFormValue("backup_hour"))
	if herr != nil || hour < 0 || hour > 23 {
		hour = 3
	}
	enabled := r.PostFormValue("backup_enabled") == "1"
	o, p, d, err, ok := s.withDatabase(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) error {
		var err error
		formErr, err = s.DatabaseSettingsTx(r, tx, o, d, cpu, mem, hour, enabled)
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
		web.Back(w, r, dbPath(o, p, d)+"#db-settings", "", formErr)
		return
	}
	msg := "Settings saved."
	if cpu != d.CPUMillis || mem != d.MemoryMB {
		msg = "Settings saved. The database restarts with the new resources (a few seconds of downtime)."
	}
	web.Back(w, r, dbPath(o, p, d)+"#db-settings", msg, "")
}

// attachDatabase stores the connection URL as an encrypted secret of a service.
func (s *API) attachDatabase(w http.ResponseWriter, r *http.Request) {
	var formErr string
	svcSlug := r.PostFormValue("service")
	name := strings.TrimSpace(r.PostFormValue("name"))
	o, p, d, err, ok := s.withDatabase(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) error {
		if name == "" {
			name = d.DefaultSecret()
		}
		if !model.SecretNameRe.MatchString(name) || strings.HasPrefix(name, "PLATFORM_") {
			formErr = "Secret names use uppercase letters, digits and underscores."
			return nil
		}
		var serviceID string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM services WHERE project_id = $1 AND slug = $2`, p.ID, svcSlug).Scan(&serviceID); err != nil {
			formErr = "Choose a service of this project."
			return nil
		}
		enc, err := s.Box.Seal([]byte(s.DBURL(d)), "secret:"+serviceID+":"+name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO secrets (org_id, service_id, name, value_enc, source) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, source = EXCLUDED.source, created_at = now()`,
			o.ID, serviceID, name, enc, "database:"+d.ID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "database.attached", "database:"+d.Slug, map[string]string{"service": svcSlug, "secret": name})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if formErr != "" {
		web.Back(w, r, dbPath(o, p, d)+"#connect", "", formErr)
		return
	}
	web.Back(w, r, dbPath(o, p, d)+"#connect", "Secret "+name+" added to "+svcSlug+". Redeploy that service to apply it.", "")
}

func (s *API) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, d, err, ok := s.withDatabase(w, r, "admin", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) error {
		if r.PostFormValue("confirm") != d.Slug {
			formErr = "Type the database ID exactly to confirm deletion."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM databases WHERE id = $1`, d.ID); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "database.deleted", "database:"+p.Slug+"/"+d.Slug, nil)
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
	if formErr != "" {
		web.Back(w, r, dbPath(o, p, d)+"#db-settings", "", formErr)
		return
	}
	web.Back(w, r, o.Path()+"/p/"+p.Slug+"#databases", "Database deleted. Its container is being removed.", "")
}

func (s *API) databaseBackupNow(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, d, err, ok := s.withDatabase(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) error {
		if d.Streaming() {
			formErr = "Kafka has no backups: topics are a stream, kept for 7 days."
			return nil
		}
		busy, err := s.QueueTargetJob(r, tx, o, "database_id", d.ID, d.Slug, "backup", "")
		if busy {
			formErr = "A backup or restore is already running for this database."
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
		web.Back(w, r, dbPath(o, p, d)+"#db-backups", "", formErr)
		return
	}
	web.Back(w, r, dbPath(o, p, d)+"#db-backups", "Backup started.", "")
}

func (s *API) databaseRestore(w http.ResponseWriter, r *http.Request) {
	var formErr string
	snap := r.PathValue("snap")
	o, p, d, err, ok := s.withDatabase(w, r, "admin", func(tx pgx.Tx, o *web.OrgInfo, p *web.ProjectInfo, d *web.DatabaseInfo) error {
		if !web.FullSnapshotRe.MatchString(snap) {
			return pgx.ErrNoRows
		}
		if r.PostFormValue("confirm") != d.Slug {
			formErr = "Type the database ID exactly to confirm the restore."
			return nil
		}
		var exists bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM backup_snapshots WHERE database_id = $1 AND snapshot_id = $2)`,
			d.ID, snap).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return pgx.ErrNoRows
		}
		busy, err := s.QueueTargetJob(r, tx, o, "database_id", d.ID, d.Slug, "restore", snap)
		if busy {
			formErr = "A backup or restore is already running for this database."
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
		web.Back(w, r, dbPath(o, p, d)+"#db-backups", "", formErr)
		return
	}
	web.Back(w, r, dbPath(o, p, d)+"#db-backups", "Restore started. Connections may be interrupted while the data is replaced.", "")
}

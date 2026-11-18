package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/secure"
)

// Managed databases (PostgreSQL, MySQL, MariaDB, Redis) in the console. The API stores the desired database (with a
// generated, encrypted password); the agent runs it on the project's private network.

type databaseInfo struct {
	ID, Slug, Engine, Version, DBName, Username, Status, Message string
	CPUMillis, MemoryMB, BackupHour                              int
	BackupEnabled                                                bool
	PasswordEnc                                                  []byte
	CreatedAt                                                    time.Time
}

// Host is the name services in the same project use to connect.
func (d *databaseInfo) Host() string { return "pd-" + model.Compact(d.ID)[:12] }

// Port and Label describe the engine for templates.
func (d *databaseInfo) Port() int { return model.EngineInfo(d.Engine).Port }

func (d *databaseInfo) Label() string { return model.EngineInfo(d.Engine).Name + " " + d.Version }

// SQL reports whether the engine has a database name and user (all but Redis).
func (d *databaseInfo) SQL() bool { return d.Engine != "redis" && d.Engine != "kafka" }

// Streaming reports whether this is a message stream (Kafka): no database name, no backups.
func (d *databaseInfo) Streaming() bool { return model.Streaming(d.Engine) }

// DumpTool names the tool that takes this engine's consistent backups.
func (d *databaseInfo) DumpTool() string {
	switch d.Engine {
	case "redis":
		return "Redis snapshot"
	case "mysql":
		return "mysqldump"
	case "mariadb":
		return "mariadb-dump"
	case "mongodb":
		return "mongodump"
	case "kafka":
		return "nothing (Kafka topics are not backed up)"
	}
	return "pg_dump"
}

// DefaultSecret is the usual environment variable name for this engine's URL.
func (d *databaseInfo) DefaultSecret() string {
	switch d.Engine {
	case "redis":
		return "REDIS_URL"
	case "mongodb":
		return "MONGODB_URI"
	case "kafka":
		return "KAFKA_URL"
	}
	return "DATABASE_URL"
}

func (s *Server) loadDatabase(r *http.Request, tx pgx.Tx, projectID string) (*databaseInfo, error) {
	var d databaseInfo
	err := tx.QueryRow(r.Context(), `
		SELECT id, slug, engine, version, db_name, username, status, message, cpu_millis, memory_mb, backup_hour, backup_enabled, password_enc, created_at
		FROM databases WHERE project_id = $1 AND slug = $2`, projectID, r.PathValue("db")).
		Scan(&d.ID, &d.Slug, &d.Engine, &d.Version, &d.DBName, &d.Username, &d.Status, &d.Message, &d.CPUMillis, &d.MemoryMB,
			&d.BackupHour, &d.BackupEnabled, &d.PasswordEnc, &d.CreatedAt)
	return &d, err
}

func (s *Server) withDatabase(w http.ResponseWriter, r *http.Request, minRole string,
	fn func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error) (*orgInfo, *projectInfo, *databaseInfo, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, nil, false
	}
	var p *projectInfo
	var d *databaseInfo
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if p, err = s.loadProject(r, tx); err != nil {
			return err
		}
		if d, err = s.loadDatabase(r, tx, p.ID); err != nil {
			return err
		}
		return fn(tx, o, p, d)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, nil, false
	}
	return o, p, d, err, true
}

func dbPath(o *orgInfo, p *projectInfo, d *databaseInfo) string {
	return o.Path() + "/p/" + p.Slug + "/d/" + d.Slug
}

func (s *Server) dbPassword(d *databaseInfo) string {
	pw, err := s.box.Open(d.PasswordEnc, "db:"+d.ID)
	if err != nil {
		return ""
	}
	return string(pw)
}

func (s *Server) dbURL(d *databaseInfo) string {
	if d.Engine == "redis" {
		u := url.URL{Scheme: "redis", User: url.UserPassword("default", s.dbPassword(d)), Host: d.Host() + ":6379", Path: "/0"}
		return u.String()
	}
	if d.Engine == "kafka" { // bootstrap server and SASL/PLAIN credentials in one value
		u := url.URL{Scheme: "kafka", User: url.UserPassword(d.Username, s.dbPassword(d)), Host: d.Host() + ":9092",
			RawQuery: "security.protocol=SASL_PLAINTEXT&sasl.mechanism=PLAIN"}
		return u.String()
	}
	if d.Engine == "mongodb" {
		u := url.URL{Scheme: "mongodb", User: url.UserPassword(d.Username, s.dbPassword(d)), Host: d.Host() + ":27017", Path: "/" + d.DBName, RawQuery: "authSource=admin"}
		return u.String()
	}
	if model.MySQLFamily(d.Engine) {
		u := url.URL{Scheme: "mysql", User: url.UserPassword(d.Username, s.dbPassword(d)), Host: d.Host() + ":3306", Path: "/" + d.DBName}
		return u.String()
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(d.Username, s.dbPassword(d)), Host: d.Host() + ":5432", Path: "/" + d.DBName, RawQuery: "sslmode=disable"}
	return u.String()
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	// "postgres:17", "mysql:8.4", "redis:7"...; an unknown choice falls back to the default.
	eng, found := model.FindEngine(r.PostFormValue("engine"))
	if !found {
		eng = model.Engines[0]
	}
	engine, version := eng.ID, eng.Version
	dbName, user := "app", "app" // SQL database and user
	if engine == "redis" {
		dbName, user = "0", "default" // Redis logical database and ACL user
	}
	base := o.Path() + "/p/" + r.PathValue("project")
	if !model.ValidSlug(slug) {
		back(w, r, base+"#databases", "", "Database ID: 2 to 20 characters, lowercase letters, digits and dashes.")
		return
	}
	var formErr string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.loadProject(r, tx)
		if err != nil {
			return err
		}
		plan, err := s.loadPlan(r, tx, o.PlanID)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM databases`).Scan(&n); err != nil {
			return err
		}
		if n >= plan.MaxDatabases {
			formErr = fmt.Sprintf("Your %s plan allows %d managed databases.", plan.Name, plan.MaxDatabases)
			return nil
		}
		mem := max(minDBMemory(engine), 256)
		backups := !model.Streaming(engine)
		if err := s.checkQuota(r, tx, o, "", 250, mem, false); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		var id string
		// Insert with a placeholder, then seal the password bound to the row ID.
		if err := tx.QueryRow(r.Context(), `
			INSERT INTO databases (org_id, project_id, slug, engine, version, db_name, username, password_enc, memory_mb, backup_enabled)
			VALUES ($1, $2, $3, $4, $5, $6, $7, '\x00', $8, $9) RETURNING id`,
			o.ID, p.ID, slug, engine, version, dbName, user, mem, backups).Scan(&id); err != nil {
			if isUniqueViolation(err) {
				formErr = "A database with this ID already exists in the project."
				return errRollback
			}
			return err
		}
		enc, err := s.box.Seal([]byte(secure.NewToken()[:32]), "db:"+id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE databases SET password_enc = $2 WHERE id = $1`, id, enc); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "database.created", "database:"+p.Slug+"/"+slug, map[string]string{"engine": engine, "version": version})
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	switch {
	case formErr != "":
		back(w, r, base+"#databases", "", formErr)
	case isNoRows(err):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, base+"/d/"+slug, "Database created. It is starting; this takes under a minute.", "")
	}
}

func (s *Server) databasePage(w http.ResponseWriter, r *http.Request) {
	var services []string
	var backups *backupView
	o, p, d, err, ok := s.withDatabase(w, r, "viewer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error {
		rows, err := tx.Query(r.Context(), `SELECT slug FROM services WHERE project_id = $1 AND preview_of IS NULL ORDER BY slug`, p.ID)
		if err != nil {
			return err
		}
		if services, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		backups, err = loadTargetBackups(r, tx, "database_id", d.ID, d.BackupEnabled, d.BackupHour)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	canEdit := model.RoleAtLeast(o.Role, "developer")
	d2 := data{
		"Org": o, "Project": p, "Database": d, "Services": services, "Backups": backups,
		"Path": dbPath(o, p, d), "CanEdit": canEdit, "CanRestore": model.RoleAtLeast(o.Role, "admin"),
	}
	if canEdit { // viewers never receive the password
		d2["Password"], d2["URL"] = s.dbPassword(d), s.dbURL(d)
	}
	s.render(w, r, "database", http.StatusOK, d2)
}

func (s *Server) databaseSettings(w http.ResponseWriter, r *http.Request) {
	var formErr string
	cpu := atoi(r.PostFormValue("cpu_millis"))
	mem := atoi(r.PostFormValue("memory_mb"))
	hour, herr := strconv.Atoi(r.PostFormValue("backup_hour"))
	if herr != nil || hour < 0 || hour > 23 {
		hour = 3
	}
	enabled := r.PostFormValue("backup_enabled") == "1"
	o, p, d, err, ok := s.withDatabase(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error {
		if d.Streaming() {
			enabled = false // Kafka has no backups
		}
		if cpu < 50 || cpu > 4000 || mem < 128 || mem > 8192 {
			formErr = "CPU must be 50 to 4000 millicores and memory 128 to 8192 MB."
			return nil
		}
		if min := minDBMemory(d.Engine); mem < min {
			formErr = fmt.Sprintf("%s needs at least %d MB of memory.", model.EngineInfo(d.Engine).Name, min)
			return nil
		}
		if err := s.checkQuota(r, tx, o, d.ID, cpu, mem, false); err != nil {
			var qe quotaError
			if errors.As(err, &qe) {
				formErr = qe.Error()
				return nil
			}
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE databases SET cpu_millis = $2, memory_mb = $3, backup_hour = $4, backup_enabled = $5 WHERE id = $1`,
			d.ID, cpu, mem, hour, enabled); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "database.updated", "database:"+d.Slug, map[string]any{"cpu_millis": cpu, "memory_mb": mem})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, dbPath(o, p, d)+"#db-settings", "", formErr)
		return
	}
	msg := "Settings saved."
	if cpu != d.CPUMillis || mem != d.MemoryMB {
		msg = "Settings saved. The database restarts with the new resources (a few seconds of downtime)."
	}
	back(w, r, dbPath(o, p, d)+"#db-settings", msg, "")
}

// attachDatabase stores the connection URL as an encrypted secret of a service.
func (s *Server) attachDatabase(w http.ResponseWriter, r *http.Request) {
	var formErr string
	svcSlug := r.PostFormValue("service")
	name := strings.TrimSpace(r.PostFormValue("name"))
	o, p, d, err, ok := s.withDatabase(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error {
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
		enc, err := s.box.Seal([]byte(s.dbURL(d)), "secret:"+serviceID+":"+name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO secrets (org_id, service_id, name, value_enc, source) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, source = EXCLUDED.source, created_at = now()`,
			o.ID, serviceID, name, enc, "database:"+d.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "database.attached", "database:"+d.Slug, map[string]string{"service": svcSlug, "secret": name})
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, dbPath(o, p, d)+"#connect", "", formErr)
		return
	}
	back(w, r, dbPath(o, p, d)+"#connect", "Secret "+name+" added to "+svcSlug+". Redeploy that service to apply it.", "")
}

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, d, err, ok := s.withDatabase(w, r, "admin", func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error {
		if r.PostFormValue("confirm") != d.Slug {
			formErr = "Type the database ID exactly to confirm deletion."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM databases WHERE id = $1`, d.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "database.deleted", "database:"+p.Slug+"/"+d.Slug, nil)
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, dbPath(o, p, d)+"#db-settings", "", formErr)
		return
	}
	back(w, r, o.Path()+"/p/"+p.Slug+"#databases", "Database deleted. Its container is being removed.", "")
}

func (s *Server) databaseBackupNow(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, p, d, err, ok := s.withDatabase(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error {
		if d.Streaming() {
			formErr = "Kafka has no backups: topics are a stream, kept for 7 days."
			return nil
		}
		busy, err := s.queueTargetJob(r, tx, o, "database_id", d.ID, d.Slug, "backup", "")
		if busy {
			formErr = "A backup or restore is already running for this database."
		}
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, dbPath(o, p, d)+"#db-backups", "", formErr)
		return
	}
	back(w, r, dbPath(o, p, d)+"#db-backups", "Backup started.", "")
}

func (s *Server) databaseRestore(w http.ResponseWriter, r *http.Request) {
	var formErr string
	snap := r.PathValue("snap")
	o, p, d, err, ok := s.withDatabase(w, r, "admin", func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error {
		if !fullSnapshotRe.MatchString(snap) {
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
		busy, err := s.queueTargetJob(r, tx, o, "database_id", d.ID, d.Slug, "restore", snap)
		if busy {
			formErr = "A backup or restore is already running for this database."
		}
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if formErr != "" {
		back(w, r, dbPath(o, p, d)+"#db-backups", "", formErr)
		return
	}
	back(w, r, dbPath(o, p, d)+"#db-backups", "Restore started. Connections may be interrupted while the data is replaced.", "")
}

// engineName is the display name of a database engine, for templates.
func engineName(id string) string { return model.EngineInfo(id).Name }

// minDBMemory is the least memory an engine runs with (MongoDB's first start runs its
// shell next to the server; at 256 MB it is killed before creating the user).
func minDBMemory(engine string) int {
	if engine == "mongodb" || engine == "kafka" { // Kafka: a JVM broker and controller
		return 512
	}
	return 128
}

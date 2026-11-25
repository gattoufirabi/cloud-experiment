package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

// Managed databases (PostgreSQL, MySQL, MariaDB, Redis) in the console. The API stores the desired database (with a
// generated, encrypted password); the agent runs it on the project's private network.

type DatabaseInfo struct {
	ID, Slug, Engine, Version, DBName, Username, Status, Message string
	CPUMillis, MemoryMB, BackupHour                              int
	BackupEnabled                                                bool
	PasswordEnc                                                  []byte
	CreatedAt                                                    time.Time
}

// Host is the name services in the same project use to connect.
func (d *DatabaseInfo) Host() string { return "pd-" + model.Compact(d.ID)[:12] }

// Port and Label describe the engine for templates.
func (d *DatabaseInfo) Port() int { return model.EngineInfo(d.Engine).Port }

func (d *DatabaseInfo) Label() string { return model.EngineInfo(d.Engine).Name + " " + d.Version }

// SQL reports whether the engine has a database name and user (all but Redis).
func (d *DatabaseInfo) SQL() bool { return d.Engine != "redis" && d.Engine != "kafka" }

// Streaming reports whether this is a message stream (Kafka): no database name, no backups.
func (d *DatabaseInfo) Streaming() bool { return model.Streaming(d.Engine) }

// DumpTool names the tool that takes this engine's consistent backups.
func (d *DatabaseInfo) DumpTool() string {
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
func (d *DatabaseInfo) DefaultSecret() string {
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

func (s *Server) LoadDatabase(r *http.Request, tx pgx.Tx, projectID string) (*DatabaseInfo, error) {
	var d DatabaseInfo
	err := tx.QueryRow(r.Context(), `
		SELECT id, slug, engine, version, db_name, username, status, message, cpu_millis, memory_mb, backup_hour, backup_enabled, password_enc, created_at
		FROM databases WHERE project_id = $1 AND slug = $2`, projectID, r.PathValue("db")).
		Scan(&d.ID, &d.Slug, &d.Engine, &d.Version, &d.DBName, &d.Username, &d.Status, &d.Message, &d.CPUMillis, &d.MemoryMB,
			&d.BackupHour, &d.BackupEnabled, &d.PasswordEnc, &d.CreatedAt)
	return &d, err
}

func (s *Server) DBPassword(d *DatabaseInfo) string {
	pw, err := s.Box.Open(d.PasswordEnc, "db:"+d.ID)
	if err != nil {
		return ""
	}
	return string(pw)
}

func (s *Server) DBURL(d *DatabaseInfo) string {
	if d.Engine == "redis" {
		u := url.URL{Scheme: "redis", User: url.UserPassword("default", s.DBPassword(d)), Host: d.Host() + ":6379", Path: "/0"}
		return u.String()
	}
	if d.Engine == "kafka" { // bootstrap server and SASL/PLAIN credentials in one value
		u := url.URL{Scheme: "kafka", User: url.UserPassword(d.Username, s.DBPassword(d)), Host: d.Host() + ":9092",
			RawQuery: "security.protocol=SASL_PLAINTEXT&sasl.mechanism=PLAIN"}
		return u.String()
	}
	if d.Engine == "mongodb" {
		u := url.URL{Scheme: "mongodb", User: url.UserPassword(d.Username, s.DBPassword(d)), Host: d.Host() + ":27017", Path: "/" + d.DBName, RawQuery: "authSource=admin"}
		return u.String()
	}
	if model.MySQLFamily(d.Engine) {
		u := url.URL{Scheme: "mysql", User: url.UserPassword(d.Username, s.DBPassword(d)), Host: d.Host() + ":3306", Path: "/" + d.DBName}
		return u.String()
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(d.Username, s.DBPassword(d)), Host: d.Host() + ":5432", Path: "/" + d.DBName, RawQuery: "sslmode=disable"}
	return u.String()
}

func (s *Server) CreateDatabase(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	// "postgres:17", "mysql:8.4", "redis:7"...; an unknown choice falls back to the default.
	eng, found := model.FindEngine(r.PostFormValue("engine"))
	if !found {
		eng = model.Engines[0]
	}
	base := o.Path() + "/p/" + r.PathValue("project")
	if !model.ValidSlug(slug) {
		Back(w, r, base+"#databases", "", "Database ID: 2 to 20 characters, lowercase letters, digits and dashes.")
		return
	}
	var formErr string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		p, err := s.LoadProject(r, tx)
		if err != nil {
			return err
		}
		formErr, err = s.CreateDatabaseTx(r, tx, o, p, slug, eng)
		return err
	})
	switch {
	case formErr != "":
		Back(w, r, base+"#databases", "", formErr)
	case IsNoRows(err):
		s.NotFound(w, r)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		Back(w, r, base+"/d/"+slug, "Database created. It is starting; this takes under a minute.", "")
	}
}

// createDatabaseTx creates a managed database with a generated password. A duplicate ID
// rolls the transaction back (errRollback) with formErr set.
func (s *Server) CreateDatabaseTx(r *http.Request, tx pgx.Tx, o *OrgInfo, p *ProjectInfo, slug string, eng model.Engine) (formErr string, err error) {
	engine, version := eng.ID, eng.Version
	dbName, user := "app", "app" // SQL database and user
	if engine == "redis" {
		dbName, user = "0", "default" // Redis logical database and ACL user
	}
	err = func() error {
		if own, err := OnOwnServer(r.Context(), tx, p.ID); err != nil || own {
			if own {
				formErr = OwnServerOnlyMsg
			}
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
		if err := s.CheckQuota(r, tx, o, "", 250, mem, false); err != nil {
			var qe QuotaError
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
			if IsUniqueViolation(err) {
				formErr = "A database with this ID already exists in the project."
				return ErrRollback
			}
			return err
		}
		// A password starting with "-" would be read as an option by command-line clients.
		pw := "p" + strings.TrimLeft(secure.NewToken(), "-_")[:31]
		enc, err := s.Box.Seal([]byte(pw), "db:"+id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE databases SET password_enc = $2 WHERE id = $1`, id, enc); err != nil {
			return err
		}
		s.Audit(r, tx, o.ID, "database.created", "database:"+p.Slug+"/"+slug, map[string]string{"engine": engine, "version": version})
		_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	}()
	return formErr, err
}

// databaseSettingsTx validates and saves a database's resources and backup schedule.
func (s *Server) DatabaseSettingsTx(r *http.Request, tx pgx.Tx, o *OrgInfo, d *DatabaseInfo, cpu, mem, hour int, enabled bool) (formErr string, err error) {
	err = func() error {
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
		if err := s.CheckQuota(r, tx, o, d.ID, cpu, mem, false); err != nil {
			var qe QuotaError
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
		s.Audit(r, tx, o.ID, "database.updated", "database:"+d.Slug, map[string]any{"cpu_millis": cpu, "memory_mb": mem})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`)
		return err
	}()
	return formErr, err
}

// engineName is the display name of a database engine, for templates.
func EngineName(id string) string { return model.EngineInfo(id).Name }

// minDBMemory is the least memory an engine runs with (MongoDB's first start runs its
// shell next to the server; at 256 MB it is killed before creating the user).
func minDBMemory(engine string) int {
	if engine == "mongodb" || engine == "kafka" { // Kafka: a JVM broker and controller
		return 512
	}
	return 128
}

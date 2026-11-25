package model

// Managed database engines and the versions offered. The databases table's
// databases_engine_version constraint lists the same pairs.

type Engine struct {
	ID, Version, Name string
	Port              int
	Scheme            string // connection URL scheme
}

// Engines in the order the console offers them; the first is the default.
var Engines = []Engine{
	{"postgres", "17", "PostgreSQL", 5432, "postgres"},
	{"postgres", "16", "PostgreSQL", 5432, "postgres"},
	{"mysql", "8.4", "MySQL", 3306, "mysql"},
	{"mariadb", "11.4", "MariaDB", 3306, "mysql"},
	{"mongodb", "8.0", "MongoDB", 27017, "mongodb"},
	{"redis", "7", "Redis", 6379, "redis"},
	{"kafka", "3.9", "Apache Kafka", 9092, "kafka"},
}

// FindEngine returns the engine for "id:version" (as sent by the console form).
func FindEngine(choice string) (Engine, bool) {
	for _, e := range Engines {
		if e.ID+":"+e.Version == choice {
			return e, true
		}
	}
	return Engine{}, false
}

// EngineInfo describes an engine by ID; unknown IDs are treated as PostgreSQL.
func EngineInfo(id string) Engine {
	for _, e := range Engines {
		if e.ID == id {
			return e
		}
	}
	return Engines[0]
}

// Streaming reports whether the engine is a message stream (no nightly backups).
func Streaming(id string) bool { return id == "kafka" }

// MySQLFamily reports whether the engine speaks the MySQL protocol (MySQL, MariaDB).
func MySQLFamily(id string) bool { return id == "mysql" || id == "mariadb" }

// Choice is the form value for this engine and version.
func (e Engine) Choice() string { return e.ID + ":" + e.Version }

// Label is the display name with the version, such as "MariaDB 11.4".
func (e Engine) Label() string { return e.Name + " " + e.Version }

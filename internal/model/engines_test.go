package model

import "testing"

func TestFindEngine(t *testing.T) {
	for choice, want := range map[string]string{"postgres:17": "PostgreSQL 17", "mysql:8.4": "MySQL 8.4", "mariadb:11.4": "MariaDB 11.4", "redis:7": "Redis 7"} {
		e, ok := FindEngine(choice)
		if !ok || e.Label() != want {
			t.Errorf("%s: got %q, %v", choice, e.Label(), ok)
		}
	}
	for _, bad := range []string{"", "mysql", "mysql:5.7", "oracle:23", "postgres:15"} {
		if _, ok := FindEngine(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if EngineInfo("mariadb").Port != 3306 || EngineInfo("unknown").ID != "postgres" {
		t.Error("EngineInfo")
	}
	if !MySQLFamily("mysql") || !MySQLFamily("mariadb") || MySQLFamily("postgres") {
		t.Error("MySQLFamily")
	}
}

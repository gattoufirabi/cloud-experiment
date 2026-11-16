package agent

import (
	"strings"
	"testing"
)

func TestMySQLEngineConfig(t *testing.T) {
	d := managedDB{ID: "x", Engine: "mariadb", Version: "11.4", MemoryMB: 256, DBName: "app", Username: "app"}
	image, cmd, env, data := engineConfig(d, "s3cret")
	if image != "mariadb:11.4" || data != "/var/lib/mysql" {
		t.Fatalf("image %q data %q", image, data)
	}
	args := strings.Join(cmd, " ")
	for _, want := range []string{"--innodb-use-native-aio=0", "--innodb-buffer-pool-size=128M", "--performance-schema=OFF"} {
		if !strings.Contains(args, want) {
			t.Errorf("command lacks %s: %s", want, args)
		}
	}
	if !strings.Contains(strings.Join(env, " "), "MARIADB_PASSWORD=s3cret") {
		t.Errorf("env: %v", env)
	}
	// The password never appears on a command line run inside the container.
	for _, c := range [][]string{readyCmd(d), mysqlShell(d, "mariadb-dump", "app")} {
		if s := strings.Join(c, " "); strings.Contains(s, "s3cret") || !strings.Contains(s, `MYSQL_PWD="$MARIADB_PASSWORD"`) {
			t.Errorf("command %q", s)
		}
	}
	d.Engine, d.Version = "mysql", "8.4"
	image, cmd, env, _ = engineConfig(d, "s3cret")
	if image != "mysql:8.4" || !strings.Contains(strings.Join(env, " "), "MYSQL_PASSWORD=s3cret") || !strings.Contains(strings.Join(env, " "), "MYSQL_INITDB_SKIP_TZINFO=1") {
		t.Errorf("mysql: %q %v", image, env)
	}
	if args := strings.Join(cmd, " "); !strings.Contains(args, "--innodb-buffer-pool-size=64M") || !strings.Contains(args, "--mysqlx=OFF") {
		t.Errorf("mysql command %s", args)
	}
	if _, dump, _ := mysqlTools(d); !strings.Contains(dump, "--set-gtid-purged=OFF") {
		t.Errorf("mysql dump %q", dump)
	}
}

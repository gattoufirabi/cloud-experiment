package api

import (
	"io"
	"strings"
	"testing"
	"time"

	"platform/internal/model"
)

// The service page renders with jobs, finished runs (exit codes), a worker and
// several instances: template errors otherwise only show up at run time as a 500.
func TestServicePageRendersJobsAndInstances(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code0, code3 := 0, 3
	d := data{
		"Org":     &orgInfo{Slug: "acme", Name: "Acme", Role: "owner"},
		"Project": &projectInfo{Slug: "shop", Name: "Shop"},
		"Service": &serviceInfo{Slug: "web", Image: "nginx:1.27-alpine", Port: 80, CPUMillis: 100, MemoryMB: 64, Instances: 3, Env: map[string]string{}},
		"CanEdit": true,
		"Path":    "/o/acme/p/shop/s/web",
		"Jobs": []jobView{
			{Slug: "hello", Schedule: "* * * * *", Command: "echo hi", Timeout: 60, Enabled: true, NextRun: &now, LastStatus: "succeeded", LastRun: &now},
			{Slug: "fail", Schedule: "0 0 1 1 *", Command: "exit 3", Timeout: 60, LastStatus: "failed", LastRun: &now},
		},
		"JobRuns": []jobRunView{
			{ID: "r1", Job: "hello", Trigger: "manual", Status: "succeeded", ExitCode: &code0, Started: now, Finished: &now, Log: "hi"},
			{ID: "r2", Job: "fail", Trigger: "schedule", Status: "failed", ExitCode: &code3, Started: now, Finished: &now, Log: "boom"},
			{ID: "r3", Job: "slow", Trigger: "manual", Status: "running", Started: now},
		},
		"ProjectVars": []projectVar{{Name: "SHARED_TOKEN", Secret: true}},
	}
	var b strings.Builder
	if err := pages["service"].ExecuteTemplate(&b, "content", d); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"<strong>hello</strong>", "exit 3", "3 instances", "<code>SHARED_TOKEN</code>"} {
		if !strings.Contains(out, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A worker page renders too.
	d["Service"] = &serviceInfo{Slug: "queue", Image: "nginx:1.27-alpine", Worker: true, Instances: 1, Env: map[string]string{}}
	if err := pages["service"].ExecuteTemplate(io.Discard, "content", d); err != nil {
		t.Fatal(err)
	}
}

func TestProjectPageRendersVariables(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	d := data{
		"Org":       &orgInfo{Slug: "acme", Name: "Acme", Role: "owner"},
		"Project":   &projectInfo{Slug: "shop", Name: "Shop"},
		"Services":  []any{},
		"Databases": []any{},
		"Variables": []projectVar{{Name: "API_URL", Value: "https://api.example.test", Updated: time.Now()}, {Name: "KEY", Secret: true, Updated: time.Now()}},
	}
	var b strings.Builder
	if err := pages["project"].ExecuteTemplate(&b, "content", d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "https://api.example.test") {
		t.Error("plain value not shown")
	}
}

// The project page offers every engine, and a MariaDB database page renders its SQL
// connection details and dump tool.
func TestDatabasePagesRenderMySQLFamily(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	org, proj := &orgInfo{Slug: "acme", Name: "Acme", Role: "owner"}, &projectInfo{Slug: "shop", Name: "Shop"}
	var b strings.Builder
	if err := pages["project"].ExecuteTemplate(&b, "content", data{
		"Org": org, "Project": proj, "Services": []any{}, "Variables": []projectVar{}, "Engines": model.Engines,
		"Databases": []struct {
			Slug, Engine, Version, Status string
			CPUMillis, MemoryMB           int
		}{{"main", "mariadb", "11.4", "running", 250, 256}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`value="mysql:8.4"`, "MariaDB 11.4", "MariaDB 11.4</option>", "<td>MariaDB 11.4</td>"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("project page lacks %q", want)
		}
	}
	b.Reset()
	d := &databaseInfo{ID: "0b6c1e2a-1111-2222-3333-444455556666", Slug: "main", Engine: "mariadb", Version: "11.4", DBName: "app", Username: "app", Status: "running", CPUMillis: 250, MemoryMB: 256}
	if err := pages["database"].ExecuteTemplate(&b, "content", data{
		"Org": org, "Project": proj, "Database": d, "Services": []string{"web"}, "Backups": &backupView{Enabled: true, Hour: 3},
		"Path": "/o/acme/p/shop/d/main", "CanEdit": true, "Password": "pw", "URL": "mysql://app:pw@pd-0b6c1e2a1111:3306/app",
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MariaDB 11.4", "<code>3306</code>", "<dt>user</dt>", "mariadb-dump", "mysql://app:pw@"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("database page lacks %q", want)
		}
	}
}

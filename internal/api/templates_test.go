package api

import (
	"io"
	"strings"
	"testing"
	"time"
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

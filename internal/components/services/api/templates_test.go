package api

import (
	"io"
	"strings"
	"testing"
	"time"

	"platform/internal/core/web"
)

// The service page renders with jobs, finished runs (exit codes), a worker and
// several instances: template errors otherwise only show up at run time as a 500.
func TestServicePageRendersJobsAndInstances(t *testing.T) {
	pages, err := web.ParseTemplates(Templates)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code0, code3 := 0, 3
	d := web.Data{
		"Org":     &web.OrgInfo{Slug: "acme", Name: "Acme", Role: "owner"},
		"Project": &web.ProjectInfo{Slug: "shop", Name: "Shop"},
		"Service": &web.ServiceInfo{Slug: "web", Image: "nginx:1.27-alpine", Port: 80, CPUMillis: 100, MemoryMB: 64, Instances: 3, Env: map[string]string{}},
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
		"ProjectVars": []web.ProjectVar{{Name: "SHARED_TOKEN", Secret: true}},
	}
	var b strings.Builder
	if err := pages["service"].ExecuteTemplate(&b, "content", d); err != nil {
		t.Fatal(err)
	}
	out := b.String() + settingsPane(t, pages["service"], d) // the settings tab is a page of its own
	for _, want := range []string{"<strong>hello</strong>", "exit 3", "3 instances", "<code>SHARED_TOKEN</code>"} {
		if !strings.Contains(out, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A service with a persistent folder shows its backups and snapshots (main page and settings page).
	d["Service"] = &web.ServiceInfo{Slug: "files", Image: "nginx:1.27-alpine", Port: 80, VolumePath: "/data", Instances: 1, Env: map[string]string{}}
	d["Backups"] = &web.BackupView{Enabled: true, Hour: 3, Snaps: []web.SnapshotView{{ID: "abcdef0123456789", TakenAt: now}}}
	var bk strings.Builder
	if err := pages["service"].ExecuteTemplate(&bk, "content", d); err != nil {
		t.Fatal("service with backups:", err)
	}
	if !strings.Contains(bk.String(), "abcdef01") {
		t.Error("the snapshot is not listed on the service page")
	}
	_ = settingsPane(t, pages["service"], d)
	// A worker page renders too.
	d["Service"] = &web.ServiceInfo{Slug: "queue", Image: "nginx:1.27-alpine", Worker: true, Instances: 1, Env: map[string]string{}}
	if err := pages["service"].ExecuteTemplate(io.Discard, "content", d); err != nil {
		t.Fatal(err)
	}
}

// settingsPane renders a page's settings view (its settings tab, on a page of its own).
func settingsPane(t *testing.T, page interface {
	ExecuteTemplate(io.Writer, string, any) error
}, d web.Data) string {
	t.Helper()
	sd := web.Data{}
	for k, v := range d {
		sd[k] = v
	}
	sd["View"] = "settings"
	var b strings.Builder
	if err := page.ExecuteTemplate(&b, "content", sd); err != nil {
		t.Fatal("settings view:", err)
	}
	return b.String()
}

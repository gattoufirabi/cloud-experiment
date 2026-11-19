package api

import (
	"io"
	"strings"
	"testing"
	"time"

	"platform/internal/functions"
	"platform/internal/garage"
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
	out := b.String() + settingsPane(t, pages["service"], d) // the settings tab is a page of its own
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

// The storage and bucket pages render with buckets, keys, a new key's secret, a folder
// listing and website settings.
func TestStoragePagesRender(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	org := &orgInfo{Slug: "acme", Name: "Acme", Role: "admin"}
	now := time.Now()
	b := &bucketInfo{ID: "b1", Name: "uploads", FullName: "acme-uploads", QuotaMB: 100, Bytes: 25 << 20, Objects: 3, Website: true, IndexDocument: "index.html", CreatedAt: now}
	var out strings.Builder
	if err := pages["storage"].ExecuteTemplate(&out, "content", data{
		"Org": org, "Buckets": []*bucketInfo{b}, "Quota": storageQuota{MaxBuckets: 10, MaxStorageMB: 10240, Buckets: 1, AllocatedMB: 100},
		"Keys":   []storageKeyView{{ID: "k1", Name: "ci", AccessKeyID: "GKabc", Created: now, Grants: []grantView{{Bucket: "uploads", Write: true}}}},
		"NewKey": garage.Key{AccessKeyID: "GKnew", SecretAccessKey: "s3cr3t", Name: "laptop"},
		"S3URL":  "https://s3.example.test", "Region": "us-east-1", "CanEdit": true, "CanDelete": true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"acme-uploads", "s3cr3t", "GKabc", "read-write", "public website", `name="perm_uploads"`, "25%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("storage page lacks %q", want)
		}
	}
	out.Reset()
	if err := pages["bucket"].ExecuteTemplate(&out, "content", data{
		"Org": org, "Bucket": b, "Prefix": "docs/", "Parent": "", "Path": "/o/acme/storage/b/uploads",
		"Objects":  []objectRow{{Key: "docs/sub/", Name: "sub/", Folder: true}, {Key: "docs/a b.txt", Name: "a b.txt", Size: 2048, Modified: now}},
		"Services": []string{"shop/web"}, "S3URL": "https://s3.example.test", "Region": "us-east-1", "WebsiteURL": "https://acme-uploads.web.apps.test/",
		"CanEdit": true, "CanDelete": true, "MaxUploadMB": 25,
	}); err != nil {
		t.Fatal(err)
	}
	out.WriteString(settingsPane(t, pages["bucket"], data{"Org": org, "Bucket": b, "Path": "/o/acme/storage/b/uploads", "CanEdit": true, "CanDelete": true}))
	for _, want := range []string{"key=docs%2fa%20b.txt", "prefix=docs%2fsub%2f", "2.0 KB", "https://acme-uploads.web.apps.test/", "shop/web", "Delete bucket"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("bucket page lacks %q", want)
		}
	}
}

// The functions list and the function page (code, test result, configuration, monitor) render.
func TestFunctionPagesRender(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	org, proj := &orgInfo{Slug: "acme", Name: "Acme", Role: "owner"}, &projectInfo{Slug: "shop", Name: "Shop"}
	var b strings.Builder
	if err := pages["functions"].ExecuteTemplate(&b, "content", data{"Org": org, "CanEdit": true, "Runtimes": functions.Runtimes,
		"Projects":  []projectOption{{"shop", "Shop"}},
		"Functions": []functionRow{{Project: "shop", ProjectName: "Shop", Slug: "resize", Runtime: "nodejs22", MemoryMB: 128, URLEnabled: true, Invocations: 3, Errors: 1}}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/o/acme/p/shop/f/resize", "Node.js 22", `value="python3.13"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("functions page lacks %q", want)
		}
	}
	b.Reset()
	dur := 12
	f := &functionInfo{Slug: "resize", Runtime: "nodejs22", Handler: "index.handler", MemoryMB: 256, TimeoutSeconds: 10, Concurrency: 5,
		Env: map[string]string{"A": "1"}, URLEnabled: true, URLAuth: "token", HasToken: true, Schedule: "0 3 * * *"}
	inv := &invocation{ID: "abc", Status: "failed", ErrorType: "TypeError", Error: "boom", Log: "line", DurationMS: &dur, Cold: true,
		Response: []byte(`{"ok":false,"errorType":"TypeError"}`)}
	if err := pages["function"].ExecuteTemplate(&b, "content", data{"Org": org, "Project": proj, "Function": f, "Path": "/o/acme/p/shop/f/resize",
		"MainFile": "index.mjs", "Source": "export const handler = 1", "Files": []string{"index.mjs", "lib.mjs"}, "Editable": true, "CodeSize": 900,
		"URL": "https://resize-shop-acme.fn.apps.test/", "CanEdit": true, "TestEvent": "{}", "TestResult": inv, "NewToken": "fnt_x",
		"Invocations": []invocation{*inv}, "Stats": struct {
			Count, Errors, Cold int
			AvgMS               *float64
		}{3, 1, 1, nil}}); err != nil {
		t.Fatal(err)
	}
	fd := data{"Org": org, "Project": proj, "Function": f, "Path": "/o/acme/p/shop/f/resize", "URL": "https://resize-shop-acme.fn.apps.test/", "CanEdit": true}
	b.WriteString(settingsPane(t, pages["function"], fd))
	for _, want := range []string{"export const handler = 1", "TypeError", "cold start", "fnt_x", "A=1", "resize-shop-acme.fn.apps.test", `value="256" selected`, "lib.mjs"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("function page lacks %q", want)
		}
	}
}

// settingsPane renders a page's settings view (its settings tab, on a page of its own).
func settingsPane(t *testing.T, page interface {
	ExecuteTemplate(io.Writer, string, any) error
}, d data) string {
	t.Helper()
	sd := data{}
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

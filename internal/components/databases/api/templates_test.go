package api

import (
	"strings"
	"testing"

	"platform/internal/core/web"
	"platform/internal/lib/model"
)

func TestDatabasePagesRenderMySQLFamily(t *testing.T) {
	pages, err := web.ParseTemplates(Templates)
	if err != nil {
		t.Fatal(err)
	}
	org, proj := &web.OrgInfo{Slug: "acme", Name: "Acme", Role: "owner"}, &web.ProjectInfo{Slug: "shop", Name: "Shop"}
	var b strings.Builder
	if err := pages["project"].ExecuteTemplate(&b, "content", web.Data{
		"Org": org, "Project": proj, "Services": []any{}, "Variables": []web.ProjectVar{}, "Engines": model.Engines,
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
	d := &web.DatabaseInfo{ID: "0b6c1e2a-1111-2222-3333-444455556666", Slug: "main", Engine: "mariadb", Version: "11.4", DBName: "app", Username: "app", Status: "running", CPUMillis: 250, MemoryMB: 256}
	if err := pages["database"].ExecuteTemplate(&b, "content", web.Data{
		"Org": org, "Project": proj, "Database": d, "Services": []string{"web"}, "Backups": &web.BackupView{Enabled: true, Hour: 3},
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

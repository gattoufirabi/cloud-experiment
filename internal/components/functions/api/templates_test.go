package api

import (
	"io"
	"strings"
	"testing"

	"platform/internal/core/web"
	"platform/internal/lib/functions"
)

// The functions list and the function page (code, test result, configuration, monitor) render.
func TestFunctionPagesRender(t *testing.T) {
	pages, err := web.ParseTemplates(Templates)
	if err != nil {
		t.Fatal(err)
	}
	org, proj := &web.OrgInfo{Slug: "acme", Name: "Acme", Role: "owner"}, &web.ProjectInfo{Slug: "shop", Name: "Shop"}
	var b strings.Builder
	if err := pages["functions"].ExecuteTemplate(&b, "content", web.Data{"Org": org, "CanEdit": true, "Runtimes": functions.Runtimes,
		"Projects":  []web.ProjectOption{{Slug: "shop", Name: "Shop"}},
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
	f := &web.FunctionInfo{Slug: "resize", Runtime: "nodejs22", Handler: "index.handler", MemoryMB: 256, TimeoutSeconds: 10, Concurrency: 5,
		Env: map[string]string{"A": "1"}, URLEnabled: true, URLAuth: "token", HasToken: true, Schedule: "0 3 * * *"}
	inv := &invocation{ID: "abc", Status: "failed", ErrorType: "TypeError", Error: "boom", Log: "line", DurationMS: &dur, Cold: true,
		Response: []byte(`{"ok":false,"errorType":"TypeError"}`)}
	if err := pages["function"].ExecuteTemplate(&b, "content", web.Data{"Org": org, "Project": proj, "Function": f, "Path": "/o/acme/p/shop/f/resize",
		"MainFile": "index.mjs", "Source": "export const handler = 1", "Files": []string{"index.mjs", "lib.mjs"}, "Editable": true, "CodeSize": 900,
		"URL": "https://resize-shop-acme.fn.apps.test/", "CanEdit": true, "TestEvent": "{}", "TestResult": inv, "NewToken": "fnt_x",
		"Invocations": []invocation{*inv}, "Stats": struct {
			Count, Errors, Cold int
			AvgMS               *float64
		}{3, 1, 1, nil}}); err != nil {
		t.Fatal(err)
	}
	fd := web.Data{"Org": org, "Project": proj, "Function": f, "Path": "/o/acme/p/shop/f/resize", "URL": "https://resize-shop-acme.fn.apps.test/", "CanEdit": true}
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

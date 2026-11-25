package web

import (
	"strings"
	"testing"
	"time"
)

func TestProjectPageRendersVariables(t *testing.T) {
	pages, err := ParseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	d := Data{
		"Org":       &OrgInfo{Slug: "acme", Name: "Acme", Role: "owner"},
		"Project":   &ProjectInfo{Slug: "shop", Name: "Shop"},
		"Services":  []any{},
		"Databases": []any{},
		"Variables": []ProjectVar{{Name: "API_URL", Value: "https://api.example.test", Updated: time.Now()}, {Name: "KEY", Secret: true, Updated: time.Now()}},
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

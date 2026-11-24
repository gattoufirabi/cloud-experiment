package api

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"platform/internal/core/web"
)

// Registering every route of every component must not panic (two handlers for one pattern would).
func TestRoutesRegister(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("routes conflict: %v", p)
		}
	}()
	New(&web.Server{})
}

// Every page of every component parses, inside the core's layout, and no two share a name.
func TestTemplatesParse(t *testing.T) {
	pages, err := web.ParseTemplates(Templates()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) < 60 {
		t.Fatalf("only %d pages", len(pages))
	}
}

// Components stay independent: none imports another component's api or agent package, and
// the core imports no component (shared code goes to the core instead).
func TestComponentBoundaries(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}} {{join .TestImports \" \"}}",
		"platform/internal/components/...", "platform/internal/core/...").Output()
	if err != nil {
		t.Skip("go list unavailable:", err)
	}
	re := regexp.MustCompile(`^platform/internal/components/(\w+)/`)
	side := regexp.MustCompile(`^platform/internal/components/(\w+)/(api|agent)$`)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		owner := "core"
		if m := re.FindStringSubmatch(f[0]); m != nil {
			owner = m[1]
		}
		for _, imp := range f[1:] {
			if m := side.FindStringSubmatch(imp); m != nil && m[1] != owner {
				t.Errorf("%s imports %s", f[0], imp)
			}
		}
	}
}

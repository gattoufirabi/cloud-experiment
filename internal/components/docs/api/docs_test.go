package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"platform/internal/core/web"
	"platform/internal/lib/config"
)

func testDocs(t *testing.T) *web.DocsSite {
	t.Helper()
	site, err := LoadDocs(config.Config{BaseURL: "https://console.test", AppsDomain: "apps.test"})
	if err != nil {
		t.Fatal(err)
	}
	return site
}

// Every topic page follows the same plan: presentation, use cases, how to use it,
// technical reference (and optionally troubleshooting).
func TestDocTopicsStructure(t *testing.T) {
	site := testDocs(t)
	want := []string{"overview", "use-cases", "how-to-use-it", "technical-reference"}
	for _, topic := range site.Order {
		if topic.Slug == "admin" {
			continue
		}
		p := site.Pages[topic.Slug]
		if p.Title == "" || p.Lede == "" {
			t.Errorf("%s: missing title or introduction", topic.Slug)
		}
		var got []string
		seen := map[string]bool{}
		for _, e := range p.TOC {
			got = append(got, e.ID)
			for _, s := range e.Sub {
				if seen[s.ID] {
					t.Errorf("%s: heading %q appears twice", topic.Slug, s.ID)
				}
				seen[s.ID] = true
			}
		}
		if len(got) < 4 || strings.Join(got[:4], ",") != strings.Join(want, ",") || len(got) > 5 || len(got) == 5 && got[4] != "troubleshooting" {
			t.Errorf("%s: sections are %v, want %v (+ troubleshooting)", topic.Slug, got, want)
		}
	}
}

var docLinkRe = regexp.MustCompile(`/docs/([a-z-]+)(#[a-z0-9-]+)?`)

// Every /docs link of the console and of the docs themselves points at a topic and,
// when it has one, at a heading that exists.
func TestDocLinks(t *testing.T) {
	site := testDocs(t)
	check := func(where, text string) {
		for _, m := range docLinkRe.FindAllStringSubmatch(text, -1) {
			slug, anchor := m[1], strings.TrimPrefix(m[2], "#")
			if slug == "user" && where != "docs.go" {
				t.Errorf("%s: link to the old guide %s", where, m[0])
				continue
			}
			p, ok := site.Pages[slug]
			if !ok {
				if slug != "user" {
					t.Errorf("%s: unknown topic %s", where, m[0])
				}
				continue
			}
			if anchor != "" && !web.HasAnchor(p, anchor) {
				t.Errorf("%s: no heading %q on /docs/%s", where, anchor, slug)
			}
		}
	}
	// every page template: the core's and each component's
	names, _ := filepath.Glob("../../../core/web/templates/*.html")
	more, _ := filepath.Glob("../../*/api/templates/*.html")
	if len(names) == 0 || len(more) == 0 {
		t.Fatal("no templates found")
	}
	for _, n := range append(names, more...) {
		b, _ := os.ReadFile(n)
		check(n, string(b))
		// A "Guide: ..." help link opens its topic, never just the index.
		if strings.Contains(string(b), `href="/docs" target="_blank"`) {
			t.Errorf("%s: a help link points at the docs index instead of its topic", n)
		}
	}
	docs, _ := fs.Glob(docsFS, "docs/*/*.md")
	docs2, _ := fs.Glob(docsFS, "docs/*.md")
	for _, n := range append(docs, docs2...) {
		b, _ := docsFS.ReadFile(n)
		check(n, string(b))
	}
}

// Old /docs/user#anchor links land on the topic that now covers them.
func TestDocLegacyLinks(t *testing.T) {
	site := testDocs(t)
	if got := site.LegacyURL("object-storage"); got != "/docs/object-storage" && !strings.HasPrefix(got, "/docs/object-storage#") {
		t.Errorf("object-storage -> %s", got)
	}
	if got := site.LegacyURL("nope"); got != "/docs" {
		t.Errorf("unknown anchor -> %s", got)
	}
	if !strings.Contains(site.Legacy, `"mailboxes":"/docs/mailboxes`) {
		t.Errorf("legacy map lacks mailboxes: %.200s", site.Legacy)
	}
}

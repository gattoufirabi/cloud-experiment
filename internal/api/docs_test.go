package api

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"platform/internal/config"
)

func testDocs(t *testing.T) *docsSite {
	t.Helper()
	site, err := loadDocs(config.Config{BaseURL: "https://console.test", AppsDomain: "apps.test"})
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
	for _, topic := range site.order {
		if topic.Slug == "admin" {
			continue
		}
		p := site.pages[topic.Slug]
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
			p, ok := site.pages[slug]
			if !ok {
				if slug != "user" {
					t.Errorf("%s: unknown topic %s", where, m[0])
				}
				continue
			}
			if anchor != "" && !hasAnchor(p, anchor) {
				t.Errorf("%s: no heading %q on /docs/%s", where, anchor, slug)
			}
		}
	}
	names, _ := fs.Glob(templateFS, "templates/*.html")
	for _, n := range names {
		b, _ := templateFS.ReadFile(n)
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
	if got := site.legacyURL("object-storage"); got != "/docs/object-storage" && !strings.HasPrefix(got, "/docs/object-storage#") {
		t.Errorf("object-storage -> %s", got)
	}
	if got := site.legacyURL("nope"); got != "/docs" {
		t.Errorf("unknown anchor -> %s", got)
	}
	if !strings.Contains(site.legacy, `"mailboxes":"/docs/mailboxes`) {
		t.Errorf("legacy map lacks mailboxes: %.200s", site.legacy)
	}
}

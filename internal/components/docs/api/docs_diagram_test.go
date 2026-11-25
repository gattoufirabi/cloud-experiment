package api

import (
	"strings"
	"testing"
)

// "::: diagram NAME" places the built-in SVG; an unknown name places nothing, and the
// diagram uses classes only (the console's CSP forbids inline styles).
func TestDocDiagrams(t *testing.T) {
	site := testDocs(t)
	page := string(site.Pages["how-it-works"].HTML)
	if !strings.Contains(page, `<figure class="doc-diagram scroll-x"><svg class="dg"`) {
		t.Fatal("architecture diagram missing from how-it-works")
	}
	svg := string(docDiagrams["architecture"])
	if strings.Contains(svg, "style=") || strings.Contains(svg, "<script") || strings.Contains(svg, "var(") {
		t.Error("diagram must use classes only: no style attributes, scripts or var() in attributes")
	}
	if out := string(renderMarkdown("# T\n\nIntro.\n\n::: diagram nope\n").HTML); strings.Contains(out, "<figure") {
		t.Errorf("unknown diagram rendered: %s", out)
	}
}

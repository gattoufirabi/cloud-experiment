package api

import (
	"strings"
	"testing"
)

func TestMarkdownEscapesHTML(t *testing.T) {
	p := renderMarkdown("# T\n\nHello <script>alert(1)</script> & `<b>code</b>`\n\n| a | b |\n| --- | --- |\n| <img src=x> | ok |\n")
	out := string(p.Lede) + string(p.HTML)
	for _, bad := range []string{"<script>", "<img", "<b>code"} {
		if strings.Contains(out, bad) {
			t.Fatalf("unescaped %q in %s", bad, out)
		}
	}
	if !strings.Contains(out, "&lt;script&gt;") || !strings.Contains(out, "<code>&lt;b&gt;code&lt;/b&gt;</code>") {
		t.Fatalf("expected escaped text, got %s", out)
	}
}

func TestMarkdownLinksAreRestricted(t *testing.T) {
	p0 := renderMarkdown("[ok](/docs/user) [anchor](#x) [ext](https://example.com) [js](javascript:alert(1)) [proto](//evil.example)")
	out := string(p0.Lede) + string(p0.HTML)
	for _, want := range []string{`<a href="/docs/user">ok</a>`, `<a href="#x">anchor</a>`, `<a href="https://example.com">ext</a>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, "javascript:") && strings.Contains(out, "href=\"javascript") || strings.Contains(out, `href="//`) {
		t.Fatalf("unsafe link rendered: %s", out)
	}
}

func TestMarkdownStructure(t *testing.T) {
	p := renderMarkdown("# Guide\n\n## First part\n\n1. one\n2. **two**\n\n```\ncode <x>\n```\n\n3. three\n\n- a *b*\n- c\n\n## Second part\n\ntext")
	if p.Title != "Guide" || len(p.TOC) != 2 || p.TOC[0].ID != "first-part" || p.TOC[1].Title != "Second part" {
		t.Fatalf("bad title/toc: %+v", p)
	}
	out := string(p.Lede) + string(p.HTML)
	for _, want := range []string{`<h2 id="first-part">`, `<ol start="1">`, `<strong>two</strong>`, `<pre class="doc-code"><code>code &lt;x&gt;</code></pre>`, `<ol start="3">`, `<em>b</em>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

func TestEmbeddedDocsRender(t *testing.T) {
	for _, name := range []string{"admin"} { // topic pages: TestDocTopicsStructure
		raw, err := docsFS.ReadFile("docs/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		p := renderMarkdown(string(raw))
		if p.Title == "" || len(p.TOC) < 8 {
			t.Fatalf("%s: title %q, %d sections", name, p.Title, len(p.TOC))
		}
	}
}

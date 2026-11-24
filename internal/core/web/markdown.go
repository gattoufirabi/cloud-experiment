package web

import (
	"html/template"
)

// A deliberately small markdown renderer for the built-in docs: headings, paragraphs,
// lists, tables, fenced code, inline code, bold, italics and links. Everything is
// HTML-escaped first; links are only kept for /path, #anchor and https:// targets.
// No dependency, nothing to configure, nothing that can inject markup.
// One extension: a line "::: diagram NAME" places a built-in SVG diagram (docDiagrams).

type TocEntry struct {
	ID, Title string
	Sub       []TocEntry // the ### headings under a ## heading
}

type DocPage struct {
	Title string
	Lede  template.HTML // the first paragraph, before any ## heading
	HTML  template.HTML
	TOC   []TocEntry
}

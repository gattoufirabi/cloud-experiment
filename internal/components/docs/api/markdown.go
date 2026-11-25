package api

import (
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"

	"platform/internal/core/web"
)

var (
	orderedRe = regexp.MustCompile(`^(\d+)\. (.*)$`)
	sepCellRe = regexp.MustCompile(`^:?-{3,}:?$`)
	linkRe    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	boldRe    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	emRe      = regexp.MustCompile(`(^|[\s(])\*([^*\s][^*]*?)\*`)
	slugRe    = regexp.MustCompile(`[^a-z0-9]+`)
)

// docDiagrams are the built-in SVG diagrams, keyed by name, read from embedded files
// when the docs load (docs/diagrams/NAME.svg): platform content, never user input.
var docDiagrams = map[string]template.HTML{}

func slugify(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// inline renders one line of text: escape, then code spans, then bold, italics, links.
func inline(s string) string {
	parts := strings.Split(s, "`")
	var b strings.Builder
	for i, p := range parts {
		p = html.EscapeString(p)
		if i%2 == 1 && i < len(parts)-1 {
			b.WriteString("<code>" + p + "</code>")
			continue
		}
		if i%2 == 1 { // unmatched backtick: keep it literally
			b.WriteString("`")
		}
		p = linkRe.ReplaceAllStringFunc(p, func(m string) string {
			sub := linkRe.FindStringSubmatch(m)
			target := sub[2]
			if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") ||
				strings.HasPrefix(target, "#") || strings.HasPrefix(target, "https://") {
				return `<a href="` + target + `">` + sub[1] + `</a>`
			}
			return sub[1]
		})
		p = boldRe.ReplaceAllString(p, "<strong>$1</strong>")
		p = emRe.ReplaceAllString(p, "$1<em>$2</em>")
		b.WriteString(p)
	}
	return b.String()
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	// "\|" is a literal bar inside a cell
	bar := string(rune(0))
	cells := strings.Split(strings.ReplaceAll(line, `\|`, bar), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(strings.ReplaceAll(cells[i], bar, "|"))
	}
	return cells
}

func renderMarkdown(src string) web.DocPage {
	var page web.DocPage
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	var para []string
	flush := func() {
		if len(para) > 0 {
			if page.Lede == "" && len(page.TOC) == 0 && b.Len() == 0 {
				page.Lede = template.HTML(inline(strings.Join(para, " ")))
				para = nil
				return
			}
			b.WriteString("<p>" + inline(strings.Join(para, " ")) + "</p>\n")
			para = nil
		}
	}
	for i := 0; i < len(lines); {
		t := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(t, "```"):
			flush()
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			b.WriteString(`<pre class="doc-code"><code>` + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
			i++
			continue
		case strings.HasPrefix(t, "::: diagram "):
			flush()
			if svg, ok := docDiagrams[strings.TrimSpace(t[len("::: diagram "):])]; ok {
				b.WriteString(`<figure class="doc-diagram scroll-x">` + string(svg) + "</figure>\n")
			}
		case t == "":
			flush()
		case strings.HasPrefix(t, "# "):
			flush()
			page.Title = strings.TrimSpace(t[2:])
		case strings.HasPrefix(t, "## "):
			flush()
			h := strings.TrimSpace(t[3:])
			id := slugify(h)
			page.TOC = append(page.TOC, web.TocEntry{ID: id, Title: h})
			fmt.Fprintf(&b, "<h2 id=\"%s\">%s</h2>\n", id, inline(h))
		case strings.HasPrefix(t, "### "):
			flush()
			h := strings.TrimSpace(t[4:])
			id := slugify(h)
			if n := len(page.TOC); n > 0 {
				page.TOC[n-1].Sub = append(page.TOC[n-1].Sub, web.TocEntry{ID: id, Title: h})
			}
			fmt.Fprintf(&b, "<h3 id=\"%s\">%s</h3>\n", id, inline(h))
		case strings.HasPrefix(t, "|"):
			flush()
			var rows [][]string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				rows = append(rows, splitRow(lines[i]))
			}
			b.WriteString("<div class=\"scroll-x\"><table>\n")
			for r, cells := range rows {
				if r == 1 && len(cells) > 0 && sepCellRe.MatchString(cells[0]) {
					continue
				}
				tag := "td"
				if r == 0 {
					tag = "th"
					b.WriteString("<thead>")
				}
				b.WriteString("<tr>")
				for _, c := range cells {
					b.WriteString("<" + tag + ">" + inline(c) + "</" + tag + ">")
				}
				b.WriteString("</tr>")
				if r == 0 {
					b.WriteString("</thead><tbody>")
				}
				b.WriteString("\n")
			}
			b.WriteString("</tbody></table></div>\n")
			continue
		case strings.HasPrefix(t, "- "):
			flush()
			b.WriteString("<ul>\n")
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "- "); i++ {
				b.WriteString("<li>" + inline(strings.TrimSpace(lines[i])[2:]) + "</li>\n")
			}
			b.WriteString("</ul>\n")
			continue
		case orderedRe.MatchString(t):
			flush()
			first := orderedRe.FindStringSubmatch(t)[1]
			fmt.Fprintf(&b, "<ol start=\"%s\">\n", first)
			for ; i < len(lines) && orderedRe.MatchString(strings.TrimSpace(lines[i])); i++ {
				b.WriteString("<li>" + inline(orderedRe.FindStringSubmatch(strings.TrimSpace(lines[i]))[2]) + "</li>\n")
			}
			b.WriteString("</ol>\n")
			continue
		default:
			para = append(para, t)
		}
		i++
	}
	flush()
	page.HTML = template.HTML(b.String()) // built only from escaped text and fixed tags
	return page
}

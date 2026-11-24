package web

type DocTopic struct {
	Slug, Title, Summary string
	Group                string
}

type DocsSite struct {
	Pages  map[string]DocPage
	Order  []DocTopic // every topic in reading order, admin last
	Legacy string     // JSON: old /docs/user anchor -> new URL
}

func HasAnchor(p DocPage, id string) bool {
	for _, e := range p.TOC {
		if e.ID == id {
			return true
		}
		for _, s := range e.Sub {
			if s.ID == id {
				return true
			}
		}
	}
	return false
}

// legacyURL is where an old /docs/user#anchor link now points ("/docs" when unknown).
func (d *DocsSite) LegacyURL(anchor string) string {
	slug, ok := LegacyDocAnchors[anchor]
	if !ok {
		return "/docs"
	}
	if HasAnchor(d.Pages[slug], anchor) {
		return "/docs/" + slug + "#" + anchor
	}
	return "/docs/" + slug
}

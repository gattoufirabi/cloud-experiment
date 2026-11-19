package api

import (
	"io/fs"
	"strings"
	"testing"
)

// A "\|" inside a table cell is a literal bar, not a new cell.
func TestTableEscapedBar(t *testing.T) {
	cells := splitRow("| read\\|write | b |")
	if len(cells) != 2 || cells[0] != "read|write" {
		t.Fatalf("cells = %q", cells)
	}
}

// Every table of the guides has the same number of cells on each row.
func TestGuideTables(t *testing.T) {
	names, _ := fs.Glob(docsFS, "docs/user/*.md")
	for _, name := range append(names, "docs/admin.md") {
		name = strings.TrimSuffix(strings.TrimPrefix(name, "docs/"), ".md")
		raw, err := docsFS.ReadFile("docs/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		for i := 0; i < len(lines); i++ {
			if !strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				continue
			}
			want := len(splitRow(lines[i]))
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				if n := len(splitRow(lines[i])); n != want {
					t.Errorf("%s.md line %d: %d cells, the table's header has %d", name, i+1, n, want)
				}
			}
		}
	}
}

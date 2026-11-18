package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"sort"
	"strings"
	"testing"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, c)
	}
	_ = zw.Close()
	return b.Bytes()
}

func tarNames(t *testing.T, data []byte) []string {
	tr := tar.NewReader(bytes.NewReader(data))
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			out = append(out, h.Name)
		}
	}
	sort.Strings(out)
	return out
}

func TestZipToTar(t *testing.T) {
	tb, n, err := zipToTar(makeZip(t, map[string]string{"Dockerfile": "FROM x", "src/app.js": "x"}))
	if err != nil || n != 2 || strings.Join(tarNames(t, tb), ",") != "repo/Dockerfile,repo/src/app.js" {
		t.Fatalf("plain: %v %d %v", err, n, tarNames(t, tb))
	}
	// One top-level folder becomes the root (GitHub "Download ZIP").
	tb, _, err = zipToTar(makeZip(t, map[string]string{"app-main/package.json": "{}", "app-main/server.js": "x", "__MACOSX/app-main/._x": "junk"}))
	if err != nil || strings.Join(tarNames(t, tb), ",") != "repo/package.json,repo/server.js" {
		t.Fatalf("folder: %v %v", err, tarNames(t, tb))
	}
	for _, bad := range []string{"../evil", "/etc/passwd", "a/../../evil"} {
		if _, _, err := zipToTar(makeZip(t, map[string]string{bad: "x"})); err == nil {
			t.Fatalf("unsafe path %q accepted", bad)
		}
	}
	if _, _, err := zipToTar([]byte("not a zip")); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, _, err := zipToTar(makeZip(t, map[string]string{})); err == nil {
		t.Fatal("empty archive accepted")
	}
}

package functions

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"testing"
)

func TestZipRoundTripAndTar(t *testing.T) {
	rt, _ := FindRuntime("nodejs22")
	files, err := ReadZip(StarterZip(rt))
	if err != nil || string(files["index.mjs"]) != rt.Starter {
		t.Fatalf("starter zip: %v %v", err, files)
	}
	if MainFile(rt, "index.handler", files) != "index.mjs" {
		t.Error("main file")
	}
	files["lib/util.js"] = []byte("module.exports = 1")
	if MainFile(rt, "lib/util.handler", files) != "lib/util.js" {
		t.Error("main file .js")
	}
	tb, err := ContainerTar(rt, files)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(tb))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	for _, want := range []string{"var/runtime/invoke.mjs", "var/task/index.mjs", "var/task/lib/", "var/task/lib/util.js"} {
		if !names[want] {
			t.Errorf("tar lacks %s (%v)", want, names)
		}
	}
	a, _ := BuildZip(files)
	b, _ := BuildZip(files)
	if !bytes.Equal(a, b) {
		t.Error("BuildZip is not deterministic")
	}
}

func TestReadZipRefusesUnsafePaths(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil.js")
	w.Write([]byte("x"))
	zw.Close()
	if _, err := ReadZip(buf.Bytes()); err == nil {
		t.Error("path traversal accepted")
	}
	if _, err := ReadZip([]byte("not a zip")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestValidHandler(t *testing.T) {
	for _, h := range []string{"index.handler", "lambda_function.lambda_handler", "src/app.main"} {
		if !ValidHandler(h) {
			t.Errorf("%s refused", h)
		}
	}
	for _, h := range []string{"", "index", "../x.handler", "index.", "a b.handler", "index.1x"} {
		if ValidHandler(h) {
			t.Errorf("%q accepted", h)
		}
	}
}

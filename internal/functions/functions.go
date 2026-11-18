// Package functions holds what the API and the agent share about serverless functions:
// the runtimes, their starter code, and how a function's code (a zip archive, as on AWS
// Lambda) is read, edited and turned into the files of its container.
package functions

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed runtime/invoke.mjs runtime/invoke.py
var runtimeFS embed.FS

const (
	MaxZipBytes      = 10 << 20 // uploaded archive
	MaxUnzippedBytes = 50 << 20
	MaxFiles         = 2000
	MaxPayloadBytes  = 6 << 20 // event and response, as on AWS Lambda
)

type Runtime struct {
	ID, Name, Image, Ext, Bootstrap, DefaultHandler, StarterFile string
	Command                                                      []string
	Starter                                                      string
	// Web apps: an ordinary HTTP server listening on $PORT.
	WebCommand, WebStarterFile, WebStarter string
}

var Runtimes = []Runtime{
	{ID: "nodejs22", Name: "Node.js 22", Image: "node:22-alpine", Ext: ".mjs", Bootstrap: "invoke.mjs",
		DefaultHandler: "index.handler", StarterFile: "index.mjs", Command: []string{"node", "/var/runtime/invoke.mjs"},
		Starter: `export const handler = async (event, context) => {
  console.log("event", JSON.stringify(event));
  return {
    statusCode: 200,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ message: "Hello from " + context.functionName, path: event.rawPath ?? null }),
  };
};
`,
		WebCommand: "node server.mjs", WebStarterFile: "server.mjs",
		WebStarter: `import http from "node:http";

// An ordinary web server: the platform starts it with PORT set and sends it the
// function URL's requests (other events arrive as POST /events).
const server = http.createServer((req, res) => {
  if (req.url === "/events" && req.method === "POST") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ received: JSON.parse(body || "null") }));
    });
    return;
  }
  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end("<h1>Hello from a web app</h1><p>Path: " + req.url.replace(/[<>&"]/g, "") + "</p>");
});
server.listen(process.env.PORT || 8080, () => console.log("listening on", process.env.PORT || 8080));
`},
	{ID: "python3.13", Name: "Python 3.13", Image: "python:3.13-alpine", Ext: ".py", Bootstrap: "invoke.py",
		DefaultHandler: "lambda_function.lambda_handler", StarterFile: "lambda_function.py", Command: []string{"python3", "-u", "/var/runtime/invoke.py"},
		Starter: `import json


def lambda_handler(event, context):
    print("event", json.dumps(event))
    return {
        "statusCode": 200,
        "headers": {"content-type": "application/json"},
        "body": json.dumps({"message": "Hello from " + context.function_name, "path": (event or {}).get("rawPath")}),
    }
`,
		WebCommand: "python3 -u app.py", WebStarterFile: "app.py",
		WebStarter: `import html
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


# An ordinary web server: the platform starts it with PORT set and sends it the
# function URL's requests (other events arrive as POST /events).
class App(BaseHTTPRequestHandler):
    def do_GET(self):
        body = ("<h1>Hello from a web app</h1><p>Path: %s</p>" % html.escape(self.path)).encode()
        self.send_response(200)
        self.send_header("content-type", "text/html; charset=utf-8")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        data = self.rfile.read(int(self.headers.get("content-length") or 0))
        body = json.dumps({"received": json.loads(data or "null")}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


ThreadingHTTPServer(("0.0.0.0", int(os.environ.get("PORT", "8080"))), App).serve_forever()
`},
}

func FindRuntime(id string) (Runtime, bool) {
	for _, r := range Runtimes {
		if r.ID == id {
			return r, true
		}
	}
	return Runtime{}, false
}

var handlerRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]{0,99}\.[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// ValidHandler checks a handler such as "index.handler" or "src/app.main".
func ValidHandler(h string) bool { return handlerRe.MatchString(h) && !strings.Contains(h, "..") }

// MainFile is the file a handler points at, for the console's code editor: for Node.js
// the first of .mjs, .js, .cjs present in the archive (default .mjs).
func MainFile(rt Runtime, handler string, files map[string][]byte) string {
	base := handler[:strings.LastIndex(handler, ".")]
	if rt.ID == "nodejs22" {
		for _, ext := range []string{".mjs", ".js", ".cjs"} {
			if _, ok := files[base+ext]; ok {
				return base + ext
			}
		}
		return base + ".mjs"
	}
	return base + rt.Ext
}

// WebStarterZip is the code of a new web-app function: a small HTTP server.
func WebStarterZip(rt Runtime) []byte {
	z, _ := BuildZip(map[string][]byte{rt.WebStarterFile: []byte(rt.WebStarter)})
	return z
}

// WebMainFile is the file a web app's start command runs ("node server.mjs" ->
// server.mjs), for the code editor; the runtime's starter file name otherwise.
func WebMainFile(rt Runtime, command string, files map[string][]byte) string {
	for _, f := range strings.Fields(command) {
		if _, ok := files[strings.TrimPrefix(f, "./")]; ok {
			return strings.TrimPrefix(f, "./")
		}
	}
	return rt.WebStarterFile
}

// StarterZip is the archive of a new function.

func StarterZip(rt Runtime) []byte {
	z, _ := BuildZip(map[string][]byte{rt.StarterFile: []byte(rt.Starter)})
	return z
}

// ReadZip returns the files of a code archive, refusing unsafe or oversized ones.
func ReadZip(data []byte) (map[string][]byte, error) {
	if len(data) > MaxZipBytes {
		return nil, fmt.Errorf("the archive is larger than %d MB", MaxZipBytes>>20)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("not a valid zip archive")
	}
	files := map[string][]byte{}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || name == ".." {
			return nil, fmt.Errorf("unsafe path in archive: %s", f.Name)
		}
		if len(files) >= MaxFiles {
			return nil, fmt.Errorf("more than %d files in the archive", MaxFiles)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, MaxUnzippedBytes-total+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		total += int64(len(b))
		if total > MaxUnzippedBytes {
			return nil, fmt.Errorf("the code is larger than %d MB once unzipped", MaxUnzippedBytes>>20)
		}
		files[name] = b
	}
	if len(files) == 0 {
		return nil, errors.New("the archive is empty")
	}
	return files, nil
}

// BuildZip makes an archive of files (sorted, fixed dates: the same files give the same bytes).
func BuildZip(files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range sortedNames(files) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(files[name]); err != nil {
			return nil, err
		}
	}
	err := zw.Close()
	return buf.Bytes(), err
}

func sortedNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ContainerTar lays out the function's files for its container: the code in
// /var/task and the runtime bootstrap in /var/runtime, readable by everyone, owned by
// root (the function runs as an unprivileged user and cannot change its code).
func ContainerTar(rt Runtime, files map[string][]byte) ([]byte, error) {
	boot, err := runtimeFS.ReadFile("runtime/" + rt.Bootstrap)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	mtime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	addDir := func(d string) error {
		for _, p := range parents(d) {
			if !dirs[p] {
				dirs[p] = true
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: 0o755, ModTime: mtime}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	add := func(name string, b []byte) error {
		if err := addDir(path.Dir(name)); err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: mtime}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err := add("var/runtime/"+rt.Bootstrap, boot); err != nil {
		return nil, err
	}
	if err := addDir("var/task"); err != nil {
		return nil, err
	}
	for _, name := range sortedNames(files) {
		if err := add("var/task/"+name, files[name]); err != nil {
			return nil, err
		}
	}
	err = tw.Close()
	return buf.Bytes(), err
}

func parents(d string) []string {
	var out []string
	parts := strings.Split(d, "/")
	for i := range parts {
		out = append(out, strings.Join(parts[:i+1], "/"))
	}
	return out
}

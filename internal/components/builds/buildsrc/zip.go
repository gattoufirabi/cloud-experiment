package buildsrc

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// Zip deployment: the uploaded source archive becomes the build's repo/ folder.

const (
	zipMaxFiles    = 20000
	zipMaxUnpacked = 500 << 20
)

// ZipToTar converts a source archive into a tar of repo/<files>, refusing unsafe paths,
// links and oversized content. When every file sits in one top-level folder (an archive
// of a folder, such as GitHub's "Download ZIP"), that folder becomes the root.
func ZipToTar(data []byte) ([]byte, int, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, errors.New("not a valid zip archive")
	}
	type entry struct {
		name string
		f    *zip.File
	}
	var entries []entry
	prefix, common := "", true
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.Mode()&0o170000 == 0o120000 { // symbolic link
			return nil, 0, fmt.Errorf("links are not allowed in the archive: %s", f.Name)
		}
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || name == ".." || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			return nil, 0, fmt.Errorf("unsafe path in the archive: %s", f.Name)
		}
		if strings.HasPrefix(name, "__MACOSX/") || path.Base(name) == ".DS_Store" {
			continue
		}
		top, _, nested := strings.Cut(name, "/")
		switch {
		case !nested:
			common = false
		case prefix == "":
			prefix = top
		case prefix != top:
			common = false
		}
		entries = append(entries, entry{name, f})
		if len(entries) > zipMaxFiles {
			return nil, 0, fmt.Errorf("more than %d files in the archive", zipMaxFiles)
		}
	}
	if len(entries) == 0 {
		return nil, 0, errors.New("the archive is empty")
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	dirs := map[string]bool{}
	var total int64
	addDirs := func(p string) error {
		var parts []string
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			parts = append([]string{d}, parts...)
		}
		for _, d := range parts {
			if !dirs[d] {
				dirs[d] = true
				if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: mtime}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "repo/", Mode: 0o755, ModTime: mtime}); err != nil {
		return nil, 0, err
	}
	dirs["repo"] = true
	for _, e := range entries {
		name := e.name
		if common && prefix != "" {
			name = strings.TrimPrefix(name, prefix+"/")
		}
		target := "repo/" + name
		if err := addDirs(target); err != nil {
			return nil, 0, err
		}
		rc, err := e.f.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("read %s: %w", e.name, err)
		}
		content, err := io.ReadAll(io.LimitReader(rc, zipMaxUnpacked-total+1))
		rc.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("read %s: %w", e.name, err)
		}
		total += int64(len(content))
		if total > zipMaxUnpacked {
			return nil, 0, fmt.Errorf("the archive unpacks to more than %d MB", zipMaxUnpacked>>20)
		}
		mode := int64(0o644)
		if e.f.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: target, Mode: mode, Size: int64(len(content)), ModTime: mtime}); err != nil {
			return nil, 0, err
		}
		if _, err := tw.Write(content); err != nil {
			return nil, 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), len(entries), nil
}

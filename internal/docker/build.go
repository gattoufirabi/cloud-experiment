package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Helpers used by the build pipeline: an isolated network, volumes, reading a file
// out of a stopped container, loading and pushing images.

// NetworkCreateIsolated creates a bridge where containers cannot reach each other
// (inter-container communication off), e.g. one shared network for builds.
func (c *Client) NetworkCreateIsolated(ctx context.Context, name, subnet string, labels map[string]string) error {
	return c.call(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "CheckDuplicate": true, "Labels": labels,
		"IPAM": map[string]any{"Driver": "default", "Config": []map[string]string{{"Subnet": subnet}}},
		"Options": map[string]string{
			"com.docker.network.bridge.name":       name,
			"com.docker.network.bridge.enable_icc": "false",
		},
	}, nil)
}

func (c *Client) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	return c.call(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	err := c.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), url.Values{"force": {"1"}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// CopyFileOut streams one regular file out of a (stopped) container. The caller closes
// the returned reader. size is the file's size in bytes.
func (c *Client) CopyFileOut(ctx context.Context, id, path string) (io.ReadCloser, int64, error) {
	resp, err := c.req(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, 0, err
	}
	tr := tar.NewReader(resp.Body)
	hdr, err := tr.Next()
	if err != nil {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("read archive: %w", err)
	}
	if hdr.Typeflag != tar.TypeReg {
		resp.Body.Close()
		return nil, 0, errors.New(path + " is not a regular file")
	}
	return struct {
		io.Reader
		io.Closer
	}{tr, resp.Body}, hdr.Size, nil
}

// VolumeSizes returns every volume's size in bytes (Docker's disk usage report;
// -1 when Docker has not computed it).
func (c *Client) VolumeSizes(ctx context.Context) (map[string]int64, error) {
	var out struct {
		Volumes []struct {
			Name      string `json:"Name"`
			UsageData struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
	}
	if err := c.call(ctx, http.MethodGet, "/system/df", url.Values{"type": {"volume"}}, nil, &out); err != nil {
		return nil, err
	}
	sizes := make(map[string]int64, len(out.Volumes))
	for _, v := range out.Volumes {
		sizes[v.Name] = v.UsageData.Size
	}
	return sizes, nil
}

// ImageLoad loads an image tarball (docker save / BuildKit "docker" exporter format).
func (c *Client) ImageLoad(ctx context.Context, tarball io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/images/load?quiet=1", tarball)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return streamError(resp)
}

// ImagePush pushes ref (repository:tag) with an X-Registry-Auth value.
func (c *Client) ImagePush(ctx context.Context, ref, auth string) error {
	repo, tag := SplitRef(ref)
	resp, err := c.reqH(ctx, http.MethodPost, "/images/"+repo+"/push", url.Values{"tag": {tag}}, nil,
		map[string]string{"X-Registry-Auth": auth})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return streamError(resp)
}

func (c *Client) ImageRemove(ctx context.Context, ref string) error {
	err := c.call(ctx, http.MethodDelete, "/images/"+ref, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// streamError reads a Docker JSON progress stream and returns its first error.
func streamError(resp *http.Response) error {
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Message: string(b)}
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
}

// Info is the subset of Docker's /info the platform uses.
type Info struct {
	Name          string `json:"Name"`
	NCPU          int    `json:"NCPU"`
	MemTotal      int64  `json:"MemTotal"`
	Containers    int    `json:"Containers"`
	ServerVersion string `json:"ServerVersion"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	err := c.call(ctx, http.MethodGet, "/info", nil, nil, &i)
	return &i, err
}

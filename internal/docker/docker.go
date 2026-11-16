// Package docker is a minimal Docker Engine API client over the Unix socket.
// Only the calls the agent needs; no SDK dependency tree to audit.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Client struct {
	hc   *http.Client
	base string
}

func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{hc: &http.Client{Transport: tr}, base: "http://docker/v1.43"}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker %d: %s", e.Status, e.Message) }

func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) req(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	return c.reqH(ctx, method, path, q, body, nil)
}

func (c *Client) reqH(ctx context.Context, method, path string, q url.Values, body any, hdr map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var m struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&m)
		return nil, &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	return resp, nil
}

func (c *Client) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := c.req(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ---------- networks ----------

type Network struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
	IPAM   struct {
		Config []struct {
			Subnet string `json:"Subnet"`
		} `json:"Config"`
	} `json:"IPAM"`
	Containers map[string]struct {
		Name        string `json:"Name"`
		IPv4Address string `json:"IPv4Address"` // "10.208.3.7/24"
	} `json:"Containers"`
}

func (c *Client) NetworkInspect(ctx context.Context, name string) (*Network, error) {
	var n Network
	err := c.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, &n)
	return &n, err
}

// NetworkCreate creates a bridge network with an explicit subnet (CIDR).
func (c *Client) NetworkCreate(ctx context.Context, name, subnet string, labels map[string]string) error {
	return c.call(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name":           name,
		"Driver":         "bridge",
		"CheckDuplicate": true,
		"Labels":         labels,
		"IPAM":           map[string]any{"Driver": "default", "Config": []map[string]string{{"Subnet": subnet}}},
		"Options": map[string]string{
			"com.docker.network.bridge.name":       name, // lets host firewall rules match pt-*
			"com.docker.network.bridge.enable_icc": "true",
		},
	}, nil)
}

func (c *Client) NetworkConnect(ctx context.Context, network, container string) error {
	err := c.call(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil,
		map[string]any{"Container": container}, nil)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// ---------- images ----------

// SplitRef returns fromImage and tag; a missing tag becomes "latest" so a pull
// never fetches every tag of a repository.
func SplitRef(ref string) (string, string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

// RegistryAuth encodes credentials for the X-Registry-Auth header.
func RegistryAuth(username, password, server string) string {
	b, _ := json.Marshal(map[string]string{"username": username, "password": password, "serveraddress": server})
	return base64.URLEncoding.EncodeToString(b)
}

// ImagePull pulls ref; auth is an X-Registry-Auth value or empty for anonymous pulls.
func (c *Client) ImagePull(ctx context.Context, ref, auth string) error {
	from, tag := SplitRef(ref)
	var hdr map[string]string
	if auth != "" {
		hdr = map[string]string{"X-Registry-Auth": auth}
	}
	resp, err := c.reqH(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {from}, "tag": {tag}}, nil, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
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

// ---------- containers ----------

type Mount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

type RestartPolicy struct {
	Name string `json:"Name"`
}

type HostConfig struct {
	Runtime        string            `json:"Runtime,omitempty"`
	Memory         int64             `json:"Memory"`
	MemorySwap     int64             `json:"MemorySwap"`
	NanoCPUs       int64             `json:"NanoCpus"`
	PidsLimit      int64             `json:"PidsLimit"`
	CapDrop        []string          `json:"CapDrop"`
	CapAdd         []string          `json:"CapAdd"`
	SecurityOpt    []string          `json:"SecurityOpt"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	Privileged     bool              `json:"Privileged"`
	NetworkMode    string            `json:"NetworkMode"`
	RestartPolicy  RestartPolicy     `json:"RestartPolicy"`
	LogConfig      LogConfig         `json:"LogConfig"`
	Mounts         []Mount           `json:"Mounts,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	ExtraHosts     []string          `json:"ExtraHosts,omitempty"` // "name:ip" lines added to /etc/hosts
}

type ContainerConfig struct {
	Image      string            `json:"Image"`
	User       string            `json:"User,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env"`
	Labels     map[string]string `json:"Labels"`
	HostConfig HostConfig        `json:"HostConfig"`
	// NetworkingConfig pins an address on HostConfig.NetworkMode (optional).
	NetworkingConfig *NetworkingConfig `json:"NetworkingConfig,omitempty"`
}

type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointConfig `json:"EndpointsConfig"`
}

type EndpointConfig struct {
	IPAMConfig *EndpointIPAM `json:"IPAMConfig,omitempty"`
}

type EndpointIPAM struct {
	IPv4Address string `json:"IPv4Address,omitempty"`
}

// FixedAddress pins a container to ip on network at creation.
func FixedAddress(network, ip string) *NetworkingConfig {
	return &NetworkingConfig{EndpointsConfig: map[string]EndpointConfig{network: {IPAMConfig: &EndpointIPAM{IPv4Address: ip}}}}
}

func (c *Client) ContainerCreate(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, cfg, &out)
	return out.ID, err
}

func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

func (c *Client) ContainerStop(ctx context.Context, id string, timeoutSec int) error {
	err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop",
		url.Values{"t": {strconv.Itoa(timeoutSec)}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerRemove force-removes a container; named volumes are kept.
func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	err := c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"true"}}, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

type ContainerInfo struct {
	ID         string `json:"Id"`
	Name       string `json:"Name"`
	HostConfig struct {
		ExtraHosts []string `json:"ExtraHosts"`
	} `json:"HostConfig"`
	State struct {
		Running   bool   `json:"Running"`
		Status    string `json:"Status"`
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (c *Client) ContainerInspect(ctx context.Context, id string) (*ContainerInfo, error) {
	var info ContainerInfo
	err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &info)
	return &info, err
}

type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

func (c *Client) ContainerList(ctx context.Context, label string) ([]ContainerSummary, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []ContainerSummary
	err := c.call(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &out)
	return out, err
}

// ContainerLogs streams stdout+stderr with timestamps since a Unix time (follow mode).
func (c *Client) ContainerLogs(ctx context.Context, id string, since int64, follow bool, tail string) (io.ReadCloser, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "since": {strconv.FormatInt(since, 10)}}
	if follow {
		q.Set("follow", "1")
	}
	if tail != "" {
		q.Set("tail", tail)
	}
	resp, err := c.req(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Demux splits Docker's multiplexed log stream (non-TTY containers) into lines.
func Demux(r io.Reader, fn func(stream byte, line string)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size > 1<<20 {
			if _, err := io.CopyN(io.Discard, br, int64(size)); err != nil {
				return err
			}
			continue
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimRight(string(buf), "\n"), "\n") {
			fn(hdr[0], line)
		}
	}
}

type Stats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

func (c *Client) ContainerStats(ctx context.Context, id string) (*Stats, error) {
	var s Stats
	err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats",
		url.Values{"stream": {"false"}, "one-shot": {"true"}}, nil, &s)
	return &s, err
}

// ContainerWait blocks until the container stops and returns its exit code.
func (c *Client) ContainerWait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait",
		url.Values{"condition": {"not-running"}}, nil, &out)
	return out.StatusCode, err
}

// ContainerOutput returns everything a stopped container wrote to stdout and stderr.
func (c *Client) ContainerOutput(ctx context.Context, id string) ([]byte, []byte, error) {
	resp, err := c.req(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs",
		url.Values{"stdout": {"1"}, "stderr": {"1"}}, nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var stdout, stderr bytes.Buffer
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if err == io.EOF {
				return stdout.Bytes(), stderr.Bytes(), nil
			}
			return stdout.Bytes(), stderr.Bytes(), err
		}
		dst := &stdout
		if hdr[0] == 2 {
			dst = &stderr
		}
		if _, err := io.CopyN(dst, br, int64(binary.BigEndian.Uint32(hdr[4:]))); err != nil {
			return stdout.Bytes(), stderr.Bytes(), err
		}
		if stdout.Len() > 64<<20 {
			return nil, nil, errors.New("container output too large")
		}
	}
}

// ImageID returns the local image ID (content hash of the image config) for ref.
func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &out)
	return out.ID, err
}

// VolumeExists reports whether a named volume exists.
func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// NetworkList returns networks carrying the given label (key=value).
func (c *Client) NetworkList(ctx context.Context, label string) ([]Network, error) {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	var out []Network
	err := c.call(ctx, http.MethodGet, "/networks", url.Values{"filters": {string(f)}}, nil, &out)
	return out, err
}

func (c *Client) NetworkDisconnect(ctx context.Context, network, container string) error {
	err := c.call(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", nil,
		map[string]any{"Container": container, "Force": true}, nil)
	if IsNotFound(err) || (err != nil && strings.Contains(err.Error(), "not connected")) {
		return nil
	}
	return err
}

func (c *Client) NetworkRemove(ctx context.Context, network string) error {
	err := c.call(ctx, http.MethodDelete, "/networks/"+url.PathEscape(network), nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerExec runs cmd inside a running container and returns its output and exit code.
func (c *Client) ContainerExec(ctx context.Context, id string, cmd, env []string) ([]byte, []byte, int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Cmd": cmd, "Env": env,
	}, &created); err != nil {
		return nil, nil, -1, err
	}
	resp, err := c.req(ctx, http.MethodPost, "/exec/"+url.PathEscape(created.ID)+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, nil, -1, err
	}
	stdout, stderr, err := readMux(resp.Body)
	resp.Body.Close()
	if err != nil {
		return stdout, stderr, -1, err
	}
	var info struct {
		ExitCode int `json:"ExitCode"`
	}
	if err := c.call(ctx, http.MethodGet, "/exec/"+url.PathEscape(created.ID)+"/json", nil, nil, &info); err != nil {
		return stdout, stderr, -1, err
	}
	return stdout, stderr, info.ExitCode, nil
}

// readMux splits Docker's multiplexed stdout/stderr stream.
func readMux(r io.Reader) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	br := bufio.NewReaderSize(r, 64<<10)
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if err == io.EOF {
				return stdout.Bytes(), stderr.Bytes(), nil
			}
			return stdout.Bytes(), stderr.Bytes(), err
		}
		dst := &stdout
		if hdr[0] == 2 {
			dst = &stderr
		}
		if _, err := io.CopyN(dst, br, int64(binary.BigEndian.Uint32(hdr[4:]))); err != nil {
			return stdout.Bytes(), stderr.Bytes(), err
		}
	}
}

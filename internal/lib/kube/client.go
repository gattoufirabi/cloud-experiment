// Package kube is a small Kubernetes API client for the agent: just what it needs to
// run autoscaled services on a k3s cluster (server-side apply, get, list, delete, scale
// and pod logs). It speaks the REST API directly instead of pulling in client-go, so the
// agent stays a small, auditable binary.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FieldManager owns every field the agent applies (server-side apply).
const FieldManager = "staysa-agent"

// Obj is a Kubernetes object as decoded JSON.
type Obj = map[string]any

var ErrNotFound = errors.New("not found")

type Client struct {
	base  string // https://host:port, no trailing slash
	hc    *http.Client
	token string
}

type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
			TLSServerName            string `yaml:"tls-server-name"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			ClientCertificate     string `yaml:"client-certificate"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKey             string `yaml:"client-key"`
			ClientKeyData         string `yaml:"client-key-data"`
			Token                 string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// Load reads a kubeconfig file (k3s writes /etc/rancher/k3s/k3s.yaml) and uses its
// current context. server, when set, replaces the cluster address: k3s writes
// https://127.0.0.1:6443, which a containerised agent cannot reach.
func Load(path, server string) (*Client, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	return Parse(raw, filepath.Dir(path), server)
}

// Parse builds a client from kubeconfig content; dir resolves relative file paths.
func Parse(raw []byte, dir, server string) (*Client, error) {
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	ctxName := kc.CurrentContext
	clusterName, userName := "", ""
	for _, c := range kc.Contexts {
		if c.Name == ctxName || ctxName == "" {
			clusterName, userName = c.Context.Cluster, c.Context.User
			break
		}
	}
	if clusterName == "" && len(kc.Clusters) == 1 {
		clusterName = kc.Clusters[0].Name
	}
	if userName == "" && len(kc.Users) == 1 {
		userName = kc.Users[0].Name
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	c := &Client{}
	found := false
	for _, cl := range kc.Clusters {
		if cl.Name != clusterName {
			continue
		}
		found = true
		c.base = strings.TrimRight(cl.Cluster.Server, "/")
		tc.ServerName = cl.Cluster.TLSServerName
		tc.InsecureSkipVerify = cl.Cluster.InsecureSkipTLSVerify
		ca, err := pemField(cl.Cluster.CertificateAuthorityData, cl.Cluster.CertificateAuthority, dir)
		if err != nil {
			return nil, fmt.Errorf("cluster CA: %w", err)
		}
		if len(ca) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(ca) {
				return nil, errors.New("cluster CA: no certificate found")
			}
			tc.RootCAs = pool
		}
	}
	if !found || c.base == "" {
		return nil, errors.New("kubeconfig: no cluster for the current context")
	}
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		c.token = u.User.Token
		cert, err := pemField(u.User.ClientCertificateData, u.User.ClientCertificate, dir)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		key, err := pemField(u.User.ClientKeyData, u.User.ClientKey, dir)
		if err != nil {
			return nil, fmt.Errorf("client key: %w", err)
		}
		if len(cert) > 0 && len(key) > 0 {
			pair, err := tls.X509KeyPair(cert, key)
			if err != nil {
				return nil, fmt.Errorf("client certificate: %w", err)
			}
			tc.Certificates = []tls.Certificate{pair}
		}
	}
	if server != "" {
		u, err := url.Parse(server)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid Kubernetes server address %q", server)
		}
		if tc.ServerName == "" {
			// The k3s certificate names 127.0.0.1 and the host name, not the address the
			// agent dials: verify it against the original name instead.
			if orig, err := url.Parse(c.base); err == nil {
				tc.ServerName = orig.Hostname()
			}
		}
		c.base = strings.TrimRight(server, "/")
	}
	c.hc = &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second}}
	return c, nil
}

// NewForTest talks to an httptest server.
func NewForTest(base string, hc *http.Client) *Client { return &Client{base: base, hc: hc} }

func pemField(data, file, dir string) ([]byte, error) {
	if data != "" {
		return base64.StdEncoding.DecodeString(data)
	}
	if file == "" {
		return nil, nil
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	return os.ReadFile(file)
}

// resource maps a kind to its API path parts.
type resource struct {
	prefix     string // /api/v1 or /apis/<group>/<version>
	plural     string
	namespaced bool
}

var resources = map[string]resource{
	"Namespace":               {"/api/v1", "namespaces", false},
	"Node":                    {"/api/v1", "nodes", false},
	"Pod":                     {"/api/v1", "pods", true},
	"Secret":                  {"/api/v1", "secrets", true},
	"Service":                 {"/api/v1", "services", true},
	"Deployment":              {"/apis/apps/v1", "deployments", true},
	"HorizontalPodAutoscaler": {"/apis/autoscaling/v2", "horizontalpodautoscalers", true},
	"NetworkPolicy":           {"/apis/networking.k8s.io/v1", "networkpolicies", true},
	"RuntimeClass":            {"/apis/node.k8s.io/v1", "runtimeclasses", false},
}

// Path is the API path of a kind, optionally in a namespace and for one object.
func Path(kind, ns, name string) (string, error) {
	r, ok := resources[kind]
	if !ok {
		return "", fmt.Errorf("unknown kind %s", kind)
	}
	p := r.prefix
	if r.namespaced && ns != "" {
		p += "/namespaces/" + url.PathEscape(ns)
	}
	p += "/" + r.plural
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p, nil
}

// APIError is a non-2xx answer of the API server.
type APIError struct {
	Status  int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes API: %d %s: %s", e.Status, e.Reason, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, contentType string) ([]byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode >= 300 {
		var st struct{ Reason, Message string }
		_ = json.Unmarshal(out, &st)
		if st.Message == "" {
			st.Message = strings.TrimSpace(string(out))
		}
		return nil, &APIError{Status: resp.StatusCode, Reason: st.Reason, Message: st.Message}
	}
	return out, nil
}

// Apply creates or updates obj with server-side apply, owning the fields it sets.
func (c *Client) Apply(ctx context.Context, obj Obj) (Obj, error) {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(Obj)
	name, _ := meta["name"].(string)
	ns, _ := meta["namespace"].(string)
	p, err := Path(kind, ns, name)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	out, err := c.do(ctx, http.MethodPatch, p, url.Values{"fieldManager": {FieldManager}, "force": {"true"}}, body, "application/apply-patch+yaml")
	if err != nil {
		return nil, fmt.Errorf("apply %s %s/%s: %w", kind, ns, name, err)
	}
	var res Obj
	return res, json.Unmarshal(out, &res)
}

func (c *Client) Get(ctx context.Context, kind, ns, name string) (Obj, error) {
	p, err := Path(kind, ns, name)
	if err != nil {
		return nil, err
	}
	out, err := c.do(ctx, http.MethodGet, p, nil, nil, "")
	if err != nil {
		return nil, err
	}
	var res Obj
	return res, json.Unmarshal(out, &res)
}

// List returns the objects of a kind matching a label selector; ns "" lists every namespace.
func (c *Client) List(ctx context.Context, kind, ns, selector string) ([]Obj, error) {
	p, err := Path(kind, ns, "")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if selector != "" {
		q.Set("labelSelector", selector)
	}
	out, err := c.do(ctx, http.MethodGet, p, q, nil, "")
	if err != nil {
		return nil, err
	}
	var res struct{ Items []Obj }
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// Delete removes an object (its dependents in the background); a missing one is not an error.
func (c *Client) Delete(ctx context.Context, kind, ns, name string) error {
	p, err := Path(kind, ns, name)
	if err != nil {
		return err
	}
	body := []byte(`{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Background"}`)
	_, err = c.do(ctx, http.MethodDelete, p, nil, body, "application/json")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Scale sets a Deployment's replica count through its scale subresource (which the
// horizontal autoscaler also uses, so no applied field is taken over).
func (c *Client) Scale(ctx context.Context, ns, name string, replicas int) error {
	p, err := Path("Deployment", ns, name)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(Obj{"spec": Obj{"replicas": replicas}})
	_, err = c.do(ctx, http.MethodPatch, p+"/scale", nil, body, "application/merge-patch+json")
	return err
}

// Logs returns a pod's log stream, each line prefixed with its RFC 3339 timestamp.
func (c *Client) Logs(ctx context.Context, ns, pod string, since time.Time, follow bool, tail int) (io.ReadCloser, error) {
	p, err := Path("Pod", ns, pod)
	if err != nil {
		return nil, err
	}
	q := url.Values{"timestamps": {"true"}}
	if follow {
		q.Set("follow", "true")
	}
	if !since.IsZero() {
		q.Set("sinceTime", since.UTC().Format(time.RFC3339))
	}
	if tail > 0 {
		q.Set("tailLines", fmt.Sprint(tail))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+p+"/log?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, Message: "pod logs"}
	}
	return resp.Body, nil
}

// Str reads a nested string field ("" when missing).
func Str(o Obj, path ...string) string {
	v, _ := field(o, path...).(string)
	return v
}

// Int reads a nested number field (0 when missing).
func Int(o Obj, path ...string) int {
	switch v := field(o, path...).(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// Slice reads a nested list field.
func Slice(o Obj, path ...string) []any {
	v, _ := field(o, path...).([]any)
	return v
}

// Labels returns an object's labels.
func Labels(o Obj) map[string]string {
	out := map[string]string{}
	m, _ := field(o, "metadata", "labels").(map[string]any)
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func field(o Obj, path ...string) any {
	var cur any = o
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

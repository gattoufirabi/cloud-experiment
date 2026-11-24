package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseKubeconfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"kind":"RuntimeClass","handler":"runsc"}`)
	}))
	defer srv.Close()
	ca := base64.StdEncoding.EncodeToString(pemCert(t, srv))
	cfg := `
apiVersion: v1
kind: Config
current-context: default
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:1
    certificate-authority-data: ` + ca + `
users:
- name: default
  user:
    token: test-token
contexts:
- name: default
  context: {cluster: default, user: default}
`
	// The kubeconfig names 127.0.0.1:1; the agent dials the override instead and still
	// verifies the certificate against the original name.
	c, err := Parse([]byte(cfg), ".", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.Get(context.Background(), "RuntimeClass", "", "runsc")
	if err != nil {
		t.Fatal(err)
	}
	if Str(rc, "handler") != "runsc" {
		t.Fatalf("handler = %q", Str(rc, "handler"))
	}
}

func TestParseKubeconfigErrors(t *testing.T) {
	if _, err := Parse([]byte("clusters: []"), ".", ""); err == nil {
		t.Fatal("a kubeconfig without a cluster must be refused")
	}
	if _, err := Parse([]byte("clusters: [{name: a, cluster: {server: 'https://x'}}]"), ".", "::bad"); err == nil {
		t.Fatal("an invalid server override must be refused")
	}
}

func TestApplyUsesServerSideApply(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"spec":{"clusterIP":"10.43.0.10"}}`)
	}))
	defer srv.Close()
	c := NewForTest(srv.URL, srv.Client())
	w := Workload{OrgID: "o", ProjectID: "11111111-2222-3333-4444-555555555555", ServiceID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Port: 8080, Revision: 3}
	res, err := c.Apply(context.Background(), w.Service())
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPatch || got.Header.Get("Content-Type") != "application/apply-patch+yaml" {
		t.Fatalf("%s %s", got.Method, got.Header.Get("Content-Type"))
	}
	if got.URL.Path != "/api/v1/namespaces/sp-11111111222233334444555555555555/services/svc-aaaaaaaabbbb" {
		t.Fatalf("path %s", got.URL.Path)
	}
	if got.URL.Query().Get("fieldManager") != FieldManager || got.URL.Query().Get("force") != "true" {
		t.Fatalf("query %s", got.URL.RawQuery)
	}
	var sent Obj
	if err := json.Unmarshal(body, &sent); err != nil || Str(sent, "kind") != "Service" {
		t.Fatalf("body %s", body)
	}
	if Str(res, "spec", "clusterIP") != "10.43.0.10" {
		t.Fatal("result not decoded")
	}
}

func TestErrorsAndList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/missing"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound) // already gone: not an error
		case r.URL.Path == "/apis/apps/v1/deployments":
			if r.URL.Query().Get("labelSelector") != LabelManaged+"=true" {
				t.Errorf("selector %q", r.URL.Query().Get("labelSelector"))
			}
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"a","labels":{"staysa.io/service":"s1"}},"status":{"replicas":3}}]}`)
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"reason":"Forbidden","message":"no"}`)
		}
	}))
	defer srv.Close()
	c := NewForTest(srv.URL, srv.Client())
	ctx := context.Background()
	if _, err := c.Get(ctx, "Deployment", "ns", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := c.Delete(ctx, "Deployment", "ns", "gone"); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
	var apiErr *APIError
	if _, err := c.Get(ctx, "Pod", "ns", "x"); !errors.As(err, &apiErr) || apiErr.Status != 403 || apiErr.Reason != "Forbidden" {
		t.Fatalf("want a 403 APIError, got %v", err)
	}
	items, err := c.List(ctx, "Deployment", "", LabelManaged+"=true")
	if err != nil || len(items) != 1 || Int(items[0], "status", "replicas") != 3 || Labels(items[0])[LabelService] != "s1" {
		t.Fatalf("list: %v %v", items, err)
	}
	if _, err := Path("ConfigMap", "", ""); err == nil {
		t.Fatal("unknown kinds must be refused")
	}
}

func TestScaleAndLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/scale"):
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/merge-patch+json" || string(b) != `{"spec":{"replicas":0}}` {
				t.Errorf("scale: %s %s", r.Header.Get("Content-Type"), b)
			}
			_, _ = io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/log"):
			if r.URL.Query().Get("timestamps") != "true" || r.URL.Query().Get("sinceTime") == "" {
				t.Errorf("log query %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, "2026-11-24T10:00:00Z hello\n")
		}
	}))
	defer srv.Close()
	c := NewForTest(srv.URL, srv.Client())
	if err := c.Scale(context.Background(), "ns", "d", 0); err != nil {
		t.Fatal(err)
	}
	rc, err := c.Logs(context.Background(), "ns", "p", time.Unix(1700000000, 0), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(b), "hello") {
		t.Fatalf("logs %q", b)
	}
}

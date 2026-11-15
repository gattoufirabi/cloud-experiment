package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestBlocked(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "10.1.2.3", "172.20.0.5", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
	} {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1"} {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestClientRefusesInternalTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil) // 127.0.0.1
	_, err := Do(context.Background(), Client(false), req)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected the loopback request to be blocked, got %v", err)
	}
	// "localhost" resolves to loopback: blocked at dial time too.
	req, _ = http.NewRequest(http.MethodGet, "http://localhost:"+srv.URL[len("http://127.0.0.1:"):], nil)
	if _, err := Do(context.Background(), Client(false), req); !errors.Is(err, ErrBlocked) {
		t.Fatalf("expected localhost to be blocked, got %v", err)
	}
	// Development mode allows it.
	req, _ = http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := Do(context.Background(), Client(true), req)
	if err != nil {
		t.Fatalf("allowPrivate request failed: %v", err)
	}
	resp.Body.Close()
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/latest/meta-data", http.StatusFound))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := Do(context.Background(), Client(true), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect was followed: status %d", resp.StatusCode)
	}
}

func TestValidateURL(t *testing.T) {
	ok := []string{"https://hooks.example.com/x", "https://ntfy.sh"}
	bad := []string{"ftp://x", "https://10.0.0.1/hook", "https://[::1]/", "https://user:pw@example.com",
		"http://example.com", "https://localhost/x", "not a url", "https://169.254.169.254/"}
	for _, u := range ok {
		if err := ValidateURL(u, true, false); err != nil {
			t.Errorf("%s: unexpected error %v", u, err)
		}
	}
	for _, u := range bad {
		if err := ValidateURL(u, true, false); err == nil {
			t.Errorf("%s: expected an error", u)
		}
	}
}

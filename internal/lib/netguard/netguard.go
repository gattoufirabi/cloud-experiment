// Package netguard makes outbound HTTP requests to client-supplied URLs (webhooks)
// without letting a client reach the platform's own internal services (SSRF).
//
// The check runs on the IP address actually dialled, after DNS resolution, so a hostname
// that resolves (or later re-resolves) to an internal address is refused too.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

var ErrBlocked = errors.New("destination address is not allowed")

// Blocked reports whether addr is private, loopback, link-local or otherwise internal.
func Blocked(addr netip.Addr) bool {
	addr = addr.Unmap() // ::ffff:10.0.0.1 is 10.0.0.1
	for _, p := range blocked {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Client returns an HTTP client that refuses internal destinations (unless allowPrivate,
// for local development), never follows redirects and gives up quickly.
func Client(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrBlocked, host)
			}
			if !allowPrivate && Blocked(ip) {
				return fmt.Errorf("%w: %s", ErrBlocked, ip)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil, // never route through an environment proxy
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ValidateURL checks a client-supplied URL before it is stored. requireHTTPS is false
// only in development. It also rejects literal internal IPs early for a clear message;
// the dialer still enforces the rule at send time.
func ValidateURL(raw string, requireHTTPS, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || len(raw) > 500 {
		return errors.New("enter a full URL such as https://hooks.example.com/platform")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if requireHTTPS {
			return errors.New("the URL must use https")
		}
	default:
		return errors.New("the URL must start with https://")
	}
	if u.User != nil {
		return errors.New("put credentials in the secret field, not in the URL")
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !allowPrivate && Blocked(ip) {
		return errors.New("private and internal addresses are not allowed")
	}
	if !allowPrivate && strings.EqualFold(u.Hostname(), "localhost") {
		return errors.New("private and internal addresses are not allowed")
	}
	return nil
}

// Do sends req with ctx using c; a convenience that keeps callers short.
func Do(ctx context.Context, c *http.Client, req *http.Request) (*http.Response, error) {
	return c.Do(req.WithContext(ctx))
}

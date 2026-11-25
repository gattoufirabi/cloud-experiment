package worker

import (
	"context"
	"net"
	"time"
)

// Outgoing email: the primary verifies sending domains through DNS (an ownership TXT
// record and the DKIM key) and delivers queued messages, signed with the domain's
// DKIM key, to each recipient's mail servers (or through the platform's SMTP relay).
// Each recipient has its own result; temporary failures are retried with back-off
// (2, 4, 8, 16 minutes). See emailsend.go.

const (
	EmailMaxAttempts = 5
	EmailBatch       = 20
)

func (a *Agent) PokeEmail() {
	select {
	case a.EmailWake <- struct{}{}:
	default:
	}
}

// resolver uses EMAIL_DNS_RESOLVER when set (tests), the system resolver otherwise.
func (a *Agent) Resolver() *net.Resolver {
	if a.Cfg.EmailDNSResolver == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		return d.DialContext(ctx, network, a.Cfg.EmailDNSResolver)
	}}
}

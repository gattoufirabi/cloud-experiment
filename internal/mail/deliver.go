package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"

	"platform/internal/config"
	"platform/internal/netguard"
)

// Per-recipient delivery. Each recipient ends up:
//   - delivered: the receiving server accepted the message;
//   - bounced: the server refused the recipient for good (5xx to RCPT TO: unknown
//     mailbox, no such domain...): the address goes on the suppression list;
//   - rejected: the server refused the message itself for good (5xx to MAIL FROM or
//     DATA: policy, content): not the address's fault;
//   - deferred: a temporary problem (4xx, no answer): tried again later.

const (
	Delivered = "delivered"
	Bounced   = "bounced"
	Rejected  = "rejected"
	Deferred  = "deferred"
)

// Result is what happened to one recipient.
type Result struct {
	Addr, Status, Detail string
}

func smtpText(err error) (int, string) {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code, fmt.Sprintf("%d %s", te.Code, te.Msg)
	}
	return 0, err.Error()
}

func all(rcpts []string, status, detail string) []Result {
	out := make([]Result, len(rcpts))
	for i, r := range rcpts {
		out[i] = Result{r, status, detail}
	}
	return out
}

// transact runs one SMTP transaction (MAIL, RCPT for each recipient, DATA) on a ready
// client and reports each recipient.
func transact(c *smtp.Client, from string, rcpts []string, msg string) []Result {
	if err := c.Mail(from); err != nil {
		code, text := smtpText(err)
		if code >= 500 {
			return all(rcpts, Rejected, "sender refused: "+text)
		}
		return all(rcpts, Deferred, "sender refused for now: "+text)
	}
	out := make([]Result, 0, len(rcpts))
	var accepted []string
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			code, text := smtpText(err)
			if code >= 500 {
				out = append(out, Result{r, Bounced, text})
			} else {
				out = append(out, Result{r, Deferred, text})
			}
			continue
		}
		accepted = append(accepted, r)
	}
	if len(accepted) == 0 {
		_ = c.Reset()
		return out
	}
	w, err := c.Data()
	if err == nil {
		if _, err = w.Write([]byte(msg)); err == nil {
			err = w.Close()
		}
	}
	if err != nil {
		code, text := smtpText(err)
		st := Deferred
		if code >= 500 {
			st = Rejected
		}
		return append(out, all(accepted, st, "message refused: "+text)...)
	}
	return append(out, all(accepted, Delivered, "accepted")...)
}

// SendRelay hands the message to the configured SMTP relay (SMTP_HOST).
func SendRelay(cfg config.Config, envelopeFrom string, rcpts []string, msg string) []Result {
	if cfg.SMTPHost == "" {
		return all(rcpts, Deferred, ErrNotConfigured.Error())
	}
	host := cfg.SMTPHost
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.SMTPPort))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if cfg.SMTPTLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return all(rcpts, Deferred, fmt.Sprintf("connect to %s: %v", addr, err))
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return all(rcpts, Deferred, err.Error())
	}
	defer c.Close()
	if cfg.SMTPTLS == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return all(rcpts, Deferred, "starttls: "+err.Error())
		}
	}
	if cfg.SMTPUser != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, host)); err != nil {
			return all(rcpts, Deferred, "relay authentication: "+err.Error())
		}
	}
	out := transact(c, envelopeFrom, rcpts, msg)
	_ = c.Quit()
	return out
}

// Direct delivery settings.
type Direct struct {
	Resolver     *net.Resolver
	HeloName     string // this server's name (its reverse DNS should point here)
	Port         int    // 25; another port in tests
	AllowPrivate bool   // tests only: the receiving server is on a private network
}

// SendDirect delivers to each recipient domain's mail servers (MX records, by
// preference; the domain itself when it has none). Mail servers on private or internal
// addresses are refused: recipients are chosen by tenants.
func SendDirect(ctx context.Context, d Direct, envelopeFrom string, rcpts []string, msg string) []Result {
	byDomain := map[string][]string{}
	var domains []string
	for _, r := range rcpts {
		dom := strings.ToLower(r[strings.LastIndex(r, "@")+1:])
		if _, ok := byDomain[dom]; !ok {
			domains = append(domains, dom)
		}
		byDomain[dom] = append(byDomain[dom], r)
	}
	var out []Result
	for _, dom := range domains {
		out = append(out, d.toDomain(ctx, dom, envelopeFrom, byDomain[dom], msg)...)
	}
	return out
}

func (d Direct) toDomain(ctx context.Context, domain, from string, rcpts []string, msg string) []Result {
	hosts, res := d.mxHosts(ctx, domain)
	if res != nil {
		return all(rcpts, res.Status, res.Detail)
	}
	lastErr := "no mail server answered"
	for _, h := range hosts {
		conn, err := d.dial(ctx, h)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		out, retry := d.session(conn, h, from, rcpts, msg, true)
		if retry { // STARTTLS failed on this server: once more without it
			if conn, err = d.dial(ctx, h); err == nil {
				out, _ = d.session(conn, h, from, rcpts, msg, false)
			}
		}
		if out != nil {
			return out
		}
		lastErr = "the connection to " + h + " failed"
	}
	return all(rcpts, Deferred, lastErr)
}

// mxHosts returns the domain's mail servers by preference, or a final result for every
// recipient (no such domain, or a domain that accepts no mail).
func (d Direct) mxHosts(ctx context.Context, domain string) ([]string, *Result) {
	r := d.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mxs, err := r.LookupMX(lctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			// No MX: the domain itself receives mail if it has an address (RFC 5321 5.1).
			if addrs, aerr := r.LookupIPAddr(lctx, domain); aerr == nil && len(addrs) > 0 {
				return []string{domain}, nil
			}
			return nil, &Result{Status: Bounced, Detail: "the domain " + domain + " does not exist or receives no mail"}
		}
		return nil, &Result{Status: Deferred, Detail: "DNS lookup of " + domain + ": " + err.Error()}
	}
	sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
	var hosts []string
	for _, mx := range mxs {
		h := strings.TrimSuffix(mx.Host, ".")
		if h == "" { // null MX (RFC 7505): the domain accepts no mail
			return nil, &Result{Status: Bounced, Detail: "the domain " + domain + " accepts no mail"}
		}
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		return []string{domain}, nil
	}
	return hosts, nil
}

func (d Direct) dial(ctx context.Context, host string) (net.Conn, error) {
	r := d.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := r.LookupNetIP(lctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("cannot resolve %s", host)
	}
	port := d.Port
	if port == 0 {
		port = 25
	}
	var lastErr error = fmt.Errorf("%s has only private addresses", host)
	for _, ip := range addrs {
		ip = ip.Unmap()
		if !d.AllowPrivate && netguard.Blocked(ip) {
			continue
		}
		dialer := net.Dialer{Timeout: 20 * time.Second}
		conn, err := dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(ip, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = fmt.Errorf("connect to %s: %w", host, err)
	}
	return nil, lastErr
}

// session greets the server, upgrades to TLS when offered (opportunistic, as mail
// servers do between themselves) and runs the transaction. retry asks for a second
// attempt without TLS when the upgrade failed.
func (d Direct) session(conn net.Conn, host, from string, rcpts []string, msg string, useTLS bool) (out []Result, retry bool) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, false
	}
	defer c.Close()
	helo := d.HeloName
	if helo == "" {
		helo = "localhost"
	}
	if err := c.Hello(helo); err != nil {
		code, text := smtpText(err)
		if code >= 500 {
			return all(rcpts, Rejected, "greeting refused: "+text), false
		}
		return nil, false
	}
	if ok, _ := c.Extension("STARTTLS"); ok && useTLS {
		if err := c.StartTLS(&tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}); err != nil { //nolint:gosec // opportunistic TLS between mail servers
			return nil, true
		}
	}
	out = transact(c, from, rcpts, msg)
	_ = c.Quit()
	return out, false
}

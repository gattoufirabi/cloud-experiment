package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sasl "github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"
	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/mail"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

// SMTP submission for tenants' applications (WordPress, Django, Nodemailer...): the same
// sending as the email API, for software that only speaks SMTP. The proxy listens on
// port 465 (implicit TLS with the console's certificate, RFC 8314) and forwards plain SMTP
// here over the internal network, so this listener is never published itself. Clients
// sign in with AUTH PLAIN, any user name and an email token (or a developer API token)
// as the password. Every message goes through the same checks and limits as the API:
// a verified From domain of the token's organization, 50 recipients, the plan's daily
// count and size.

func (s *API) ServeSMTP(ctx context.Context) {
	if s.Cfg.SMTPSubmissionListen == "" || !s.EmailEnabled() {
		return
	}
	srv := smtp.NewServer(&smtpBackend{s: s})
	srv.Addr = s.Cfg.SMTPSubmissionListen
	srv.Domain = s.Cfg.MailHost()
	srv.MaxRecipients = 50
	srv.MaxMessageBytes = 11 << 20
	srv.ReadTimeout = 2 * time.Minute
	srv.WriteTimeout = 2 * time.Minute
	// TLS is ended by the proxy (this port is only reachable from it), unless a
	// certificate folder is given for STARTTLS here.
	srv.AllowInsecureAuth = s.Cfg.SMTPSubmissionInsecure || s.Cfg.SMTPSubmissionCertDir == ""
	if s.Cfg.SMTPSubmissionCertDir != "" {
		cl := &certLoader{dir: s.Cfg.SMTPSubmissionCertDir, host: s.Cfg.MailHost()}
		srv.TLSConfig = &tls.Config{GetCertificate: cl.get, MinVersion: tls.VersionTLS12}
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	s.Log.Info("smtp submission listening", "addr", srv.Addr, "host", srv.Domain, "tls", srv.TLSConfig != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, smtp.ErrServerClosed) && ctx.Err() == nil {
		s.Log.Error("smtp submission", "err", err)
	}
}

// certLoader reads the proxy's certificate for the mail host from Caddy's data folder
// (re-read when Caddy renews it).
type certLoader struct {
	dir, host string
	mu        sync.Mutex
	cert      *tls.Certificate
	mod       time.Time
	checked   time.Time
}

func (c *certLoader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && time.Since(c.checked) < 10*time.Minute {
		return c.cert, nil
	}
	c.checked = time.Now()
	var crt string
	_ = filepath.WalkDir(filepath.Join(c.dir, "caddy", "certificates"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == c.host+".crt" {
			crt = p
			return fs.SkipAll
		}
		return nil
	})
	if crt == "" {
		if c.cert != nil {
			return c.cert, nil
		}
		return nil, fmt.Errorf("no certificate for %s yet", c.host)
	}
	if st, err := os.Stat(crt); err == nil && c.cert != nil && !st.ModTime().After(c.mod) {
		return c.cert, nil
	}
	pair, err := tls.LoadX509KeyPair(crt, strings.TrimSuffix(crt, ".crt")+".key")
	if err != nil {
		if c.cert != nil {
			return c.cert, nil
		}
		return nil, err
	}
	if st, err := os.Stat(crt); err == nil {
		c.mod = st.ModTime()
	}
	c.cert = &pair
	return c.cert, nil
}

type smtpBackend struct{ s *API }

func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{s: b.s, remote: c.Conn().RemoteAddr().String()}, nil
}

type smtpSession struct {
	s      *API
	remote string
	org    *web.OrgInfo
	rcpts  []string
}

func smtpErr(code int, enh smtp.EnhancedCode, msg string) error {
	return &smtp.SMTPError{Code: code, EnhancedCode: enh, Message: msg}
}

func (ss *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (ss *smtpSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		ip := ss.remote
		if i := strings.LastIndex(ip, ":"); i > 0 {
			ip = ip[:i]
		}
		if !ss.s.Limiter.Allow("smtp-fail:"+ip, 30, 10*time.Minute) {
			return smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "Too many failed sign-ins, try again later")
		}
		tok := strings.TrimSpace(password)
		if !strings.HasPrefix(tok, web.APITokenPrefix) {
			return smtpErr(535, smtp.EnhancedCode{5, 7, 8}, "Use an email token (pla_...) as the password")
		}
		var o web.OrgInfo
		var role string
		var emailOnly bool
		var queueScope, projectScope *string
		err := ss.s.Pool.QueryRow(context.Background(), `
			SELECT role, org_id, org_slug, plan_id, queue_id::text, email_only, project_slug FROM api_token_check($1)`,
			secure.HashToken(tok)).Scan(&role, &o.ID, &o.Slug, &o.PlanID, &queueScope, &emailOnly, &projectScope)
		if err != nil || queueScope != nil || projectScope != nil || !(emailOnly || model.RoleAtLeast(role, "developer")) {
			return smtpErr(535, smtp.EnhancedCode{5, 7, 8}, "Invalid, expired or revoked token")
		}
		o.Role = role
		ss.org = &o
		return nil
	}), nil
}

func (ss *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if ss.org == nil {
		return smtpErr(530, smtp.EnhancedCode{5, 7, 0}, "Sign in first (AUTH PLAIN with your email token)")
	}
	return nil
}

func (ss *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if ss.org == nil {
		return smtpErr(530, smtp.EnhancedCode{5, 7, 0}, "Sign in first")
	}
	f, _, err := mail.ParseAddress(to)
	if err != nil {
		return smtpErr(553, smtp.EnhancedCode{5, 1, 3}, "Invalid recipient address")
	}
	if len(ss.rcpts) >= 50 {
		return smtpErr(452, smtp.EnhancedCode{4, 5, 3}, "At most 50 recipients per message")
	}
	ss.rcpts = append(ss.rcpts, f)
	return nil
}

func (ss *smtpSession) Data(r io.Reader) error {
	if ss.org == nil || len(ss.rcpts) == 0 {
		_, _ = io.Copy(io.Discard, r)
		return smtpErr(503, smtp.EnhancedCode{5, 5, 1}, "No recipients")
	}
	raw, err := io.ReadAll(io.LimitReader(r, 11<<20))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var id string
	err = db.WithOrg(ctx, ss.s.Pool, ss.org.ID, func(tx pgx.Tx) error {
		msg, fromAddr, err := mail.PrepareRaw(raw, ss.s.Cfg.MailHost(), time.Now())
		if err != nil {
			return smtpErr(550, smtp.EnhancedCode{5, 6, 0}, "Invalid message: "+err.Error())
		}
		domain := fromAddr[strings.LastIndex(fromAddr, "@")+1:]
		var domainID, status string
		if err := tx.QueryRow(ctx, `SELECT id, status FROM email_domains WHERE domain = $1`, domain).Scan(&domainID, &status); err != nil || status != "verified" {
			return smtpErr(550, smtp.EnhancedCode{5, 7, 1}, "From: "+domain+" is not a verified sending domain of your organization")
		}
		id, err = ss.s.enqueueEmailTx(ctx, tx, ss.org, queued{DomainID: domainID, From: fromAddr, To: ss.rcpts, Subject: headerSubject(msg),
			Raw: []byte(msg), Size: len(msg), Source: "smtp"})
		var ae web.APIErr
		if errors.As(err, &ae) {
			code, enh := 552, smtp.EnhancedCode{5, 3, 4}
			if ae.Status == 429 {
				code, enh = 452, smtp.EnhancedCode{4, 5, 3}
			}
			return smtpErr(code, enh, ae.Msg)
		}
		return err
	})
	ss.rcpts = nil
	if err != nil {
		var se *smtp.SMTPError
		if errors.As(err, &se) {
			return se
		}
		ss.s.Log.Error("smtp submission", "err", err)
		return smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Temporary error, try again")
	}
	ss.s.Log.Info("smtp message queued", "org", ss.org.Slug, "id", id)
	return nil
}

// headerSubject returns the decoded Subject header (one line, at most 998 characters).
func headerSubject(msg string) string {
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	for i, l := range lines {
		if len(l) > 8 && strings.EqualFold(l[:8], "subject:") {
			v := l[8:]
			for j := i + 1; j < len(lines) && (strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t")); j++ {
				v += lines[j]
			}
			dec := new(mime.WordDecoder)
			if d, err := dec.DecodeHeader(strings.TrimSpace(v)); err == nil {
				v = d
			}
			v = strings.TrimSpace(v)
			if len(v) > 998 {
				v = v[:998]
			}
			return v
		}
	}
	return "(no subject)"
}

func (ss *smtpSession) Reset() { ss.rcpts = nil }

func (ss *smtpSession) Logout() error { return nil }

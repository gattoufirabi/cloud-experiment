package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
	"platform/internal/lib/stalwart"
)

// Hosted mailboxes. A domain the organization verified for sending (email_domains) can
// also receive mail on the platform's mail server: it gets mailboxes (IMAP, POP3 and SMTP
// submission), aliases and a catch-all. A domain goes on the mail server only once it is
// verified (otherwise any organization could capture another's mail locally); in a zone
// hosted here, its MX, DKIM, SPF, DMARC and client SRV records are written automatically.
// Passwords go straight to the mail server and are never stored by the platform.

var localPartRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

const minMailboxPassword = 12

type mailDomain struct {
	ID, EmailDomainID, Domain, StalwartID, CatchAll, DKIM string
	ServerName                                            string // own name for webmail and mail apps ("" = the platform's)
	Status                                                string // the domain's verification (email_domains.status)
	Mailboxes                                             int
	Created                                               time.Time
}

// Active: the domain is on the mail server and receives mail.
func (d *mailDomain) Active() bool { return d.StalwartID != "" }

type mailbox struct {
	ID, LocalPart, Name, StalwartID string
	QuotaMB                         int
	Aliases                         []mailAlias
	UsedMB                          float64
	Created                         time.Time
}

type mailAlias struct{ ID, LocalPart string }

// domainWebmailURL is where this domain's mailboxes open the webmail.
func (s *API) domainWebmailURL(md *mailDomain) string {
	if md.ServerName == "" {
		return s.webmailURL()
	}
	u, err := url.Parse(s.Cfg.BaseURL)
	if err != nil || u.Scheme == "https" {
		return "https://" + md.ServerName
	}
	host := md.ServerName
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	return u.Scheme + "://" + host
}

// webmailURL is the webmail's address ("" when there is none), with the console's scheme
// and port.
func (s *API) webmailURL() string {
	if s.Cfg.WebmailHost == "" {
		return ""
	}
	u, err := url.Parse(s.Cfg.BaseURL)
	if err != nil {
		return "https://" + s.Cfg.WebmailHost
	}
	host := s.Cfg.WebmailHost
	if p := u.Port(); p != "" && !strings.Contains(host, ":") {
		host += ":" + p
	}
	return u.Scheme + "://" + host
}

const mailDomainCols = `md.id, md.email_domain_id, md.domain, coalesce(md.stalwart_id, ''), md.catch_all, md.dkim, md.server_name, ed.status,
	(SELECT count(*) FROM mailboxes b WHERE b.mail_domain_id = md.id)::int, md.created_at`

func scanMailDomain(row pgx.Row) (*mailDomain, error) {
	var d mailDomain
	if err := row.Scan(&d.ID, &d.EmailDomainID, &d.Domain, &d.StalwartID, &d.CatchAll, &d.DKIM, &d.ServerName, &d.Status, &d.Mailboxes, &d.Created); err != nil {
		return nil, err
	}
	return &d, nil
}

func loadMailDomains(ctx context.Context, tx pgx.Tx) ([]*mailDomain, error) {
	rows, err := tx.Query(ctx, `SELECT `+mailDomainCols+` FROM mail_domains md JOIN email_domains ed ON ed.id = md.email_domain_id ORDER BY md.domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*mailDomain
	for rows.Next() {
		d, err := scanMailDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func loadMailDomain(ctx context.Context, tx pgx.Tx, domain string) (*mailDomain, error) {
	return scanMailDomain(tx.QueryRow(ctx, `SELECT `+mailDomainCols+` FROM mail_domains md JOIN email_domains ed ON ed.id = md.email_domain_id WHERE md.domain = $1`, domain))
}

func loadMailboxes(ctx context.Context, tx pgx.Tx, mailDomainID string) ([]*mailbox, error) {
	rows, err := tx.Query(ctx, `SELECT id, local_part, display_name, stalwart_id, quota_mb, created_at FROM mailboxes WHERE mail_domain_id = $1 ORDER BY local_part`, mailDomainID)
	if err != nil {
		return nil, err
	}
	var out []*mailbox
	byID := map[string]*mailbox{}
	for rows.Next() {
		var b mailbox
		if err := rows.Scan(&b.ID, &b.LocalPart, &b.Name, &b.StalwartID, &b.QuotaMB, &b.Created); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, &b)
		byID[b.ID] = &b
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT id, mailbox_id, local_part FROM mail_aliases WHERE mail_domain_id = $1 ORDER BY local_part`, mailDomainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a mailAlias
		var box string
		if err := rows.Scan(&a.ID, &box, &a.LocalPart); err != nil {
			return nil, err
		}
		if b := byID[box]; b != nil {
			b.Aliases = append(b.Aliases, a)
		}
	}
	return out, rows.Err()
}

func loadMailbox(ctx context.Context, tx pgx.Tx, md *mailDomain, local string) (*mailbox, error) {
	bs, err := loadMailboxes(ctx, tx, md.ID)
	if err != nil {
		return nil, err
	}
	for _, b := range bs {
		if b.LocalPart == local {
			return b, nil
		}
	}
	return nil, pgx.ErrNoRows
}

// mailErr turns a mail server failure into a message for the person.
func mailErr(err error) error {
	var se *stalwart.Error
	if errors.As(err, &se) {
		switch se.Type {
		case "primaryKeyViolation":
			return web.APIErr{Status: http.StatusConflict, Msg: "this address is already used on the mail server"}
		case "validationFailed", "invalidProperties", "invalidPatch":
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "the mail server refused it: " + se.Detail}
		}
	}
	return web.APIErr{Status: http.StatusBadGateway, Msg: "the mail server is not answering; try again in a minute"}
}

// ---------- domains ----------

// addMailDomainTx hosts mailboxes for domain: it becomes a sending domain if it is not
// one yet (that is how it is verified), and goes on the mail server once verified.
func (s *API) addMailDomainTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, domain string) (*mailDomain, error) {
	if !s.MailHostingEnabled() {
		return nil, web.APIErr{Status: http.StatusServiceUnavailable, Msg: "mailboxes are not available on this platform"}
	}
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if len(domain) > 253 || !emailDomainRe.MatchString(domain) {
		return nil, web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "enter a domain name such as example.com"}
	}
	if domain == strings.ToLower(s.Cfg.MailHost()) {
		return nil, web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "this name belongs to the platform"}
	}
	ed, err := loadSendDomain(r.Context(), tx, domain)
	if web.IsNoRows(err) {
		if err := s.addEmailDomainTx(r, tx, o, domain); err != nil {
			return nil, err
		}
		ed, err = loadSendDomain(r.Context(), tx, domain)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO mail_domains (org_id, email_domain_id, domain) VALUES ($1, $2, $3)`, o.ID, ed.ID, domain); err != nil {
		if web.IsUniqueViolation(err) {
			return nil, web.APIErr{Status: http.StatusConflict, Msg: "this domain already has mailboxes"}
		}
		return nil, err
	}
	s.Audit(r, tx, o.ID, "mail.domain_added", "mail-domain:"+domain, nil)
	md, err := loadMailDomain(r.Context(), tx, domain)
	if err != nil {
		return nil, err
	}
	if md.Status == "verified" {
		if err := s.activateMailDomainTx(r, tx, o, md); err != nil {
			return nil, err
		}
	}
	return md, nil
}

// activateMailDomainTx puts a verified domain on the mail server and writes its records.
func (s *API) activateMailDomainTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain) error {
	if md.Active() {
		return nil
	}
	if md.Status != "verified" {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "verify the domain first: its DNS records are not confirmed yet"}
	}
	ms := s.MailServer()
	id, err := ms.CreateDomain(r.Context(), md.Domain, "platform:"+md.ID)
	if err != nil {
		if !stalwart.IsType(err, "primaryKeyViolation") {
			return mailErr(err)
		}
		// left over by an interrupted activation of this same domain?
		ds, lerr := ms.Domains(r.Context())
		if lerr != nil {
			return mailErr(lerr)
		}
		for _, d := range ds {
			if d.Name == md.Domain && d.Description == "platform:"+md.ID {
				id = d.ID
			}
		}
		if id == "" {
			return web.APIErr{Status: http.StatusConflict, Msg: "this domain is already used on the mail server"}
		}
	}
	zone, err := ms.WaitZoneFile(r.Context(), id)
	if err != nil {
		return mailErr(err)
	}
	md.StalwartID, md.DKIM = id, dkimLines(zone)
	if _, err := tx.Exec(r.Context(), `UPDATE mail_domains SET stalwart_id = $2, dkim = $3 WHERE id = $1`, md.ID, id, md.DKIM); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "mail.domain_activated", "mail-domain:"+md.Domain, nil)
	_, err = s.autoMailDomainTx(r.Context(), r, tx, o, md)
	return err
}

// dkimLines keeps the DKIM records of the mail server's zone file.
func dkimLines(zone string) string { return zoneDKIM(zone) }

// zoneDKIM returns the DKIM TXT records of a zone file, one per line, in zone file form.
func zoneDKIM(zone string) string {
	var b strings.Builder
	start := -1
	lines := strings.Split(zone, "\n")
	for i, l := range lines {
		if start < 0 && strings.Contains(l, "._domainkey.") {
			start = i
		}
		if start >= 0 {
			if strings.Contains(lines[start], "(") && !strings.Contains(l, ")") {
				continue
			}
			b.WriteString(strings.Join(lines[start:i+1], "\n") + "\n")
			start = -1
		}
	}
	return b.String()
}

// ClientHost is the name mail apps and the webmail use for this domain.
func (s *API) clientHost(md *mailDomain) string {
	if md.ServerName != "" {
		return md.ServerName
	}
	return s.Cfg.MailHost()
}

// mailRecords are the DNS records a hosted mail domain needs (names relative to the domain).
func (s *API) mailRecords(md *mailDomain) []mailRecord {
	host := s.Cfg.MailHost()
	client := s.clientHost(md)
	out := []mailRecord{{web.DNSRecordInput{Name: "@", Type: "MX", Value: host, Priority: 10}, "delivers the domain's mail to the platform", true}}
	if recs, _, err := web.ParseZoneFile(md.Domain, md.DKIM); err == nil {
		for _, in := range recs {
			if in.Type == "TXT" {
				out = append(out, mailRecord{in, "DKIM: signs the mail your mailboxes send", true})
			}
		}
	}
	spf := "v=spf1 mx"
	if m := s.SpfMechanism(); m != "" {
		spf += " " + m
	}
	out = append(out,
		mailRecord{web.DNSRecordInput{Name: "@", Type: "TXT", Value: spf + " ~all"}, "SPF: allows the platform to send for the domain (merge it into an SPF record you already have)", true},
		mailRecord{web.DNSRecordInput{Name: "_dmarc", Type: "TXT", Value: "v=DMARC1; p=none"}, "DMARC policy (keep yours if you have one)", false},
		mailRecord{web.DNSRecordInput{Name: "_imaps._tcp", Type: "SRV", Value: "1 993 " + client}, "lets mail apps find the IMAP server", false},
		mailRecord{web.DNSRecordInput{Name: "_submission._tcp", Type: "SRV", Value: "1 587 " + client}, "lets mail apps find the sending server", false},
		mailRecord{web.DNSRecordInput{Name: "_pop3s._tcp", Type: "SRV", Value: "1 995 " + client}, "lets mail apps find the POP3 server", false},
	)
	if md.ServerName != "" && s.Cfg.PublicIP != "" {
		out = append(out, mailRecord{web.DNSRecordInput{Name: strings.TrimSuffix(md.ServerName, "."+md.Domain), Type: "A", Value: s.Cfg.PublicIP},
			"your own server name: the webmail and mail apps use " + md.ServerName, true})
	}
	return out
}

type mailRecord struct {
	web.DNSRecordInput
	Purpose  string
	Required bool
}

// FQDN is the record's full name.
func (m mailRecord) FQDN(domain string) string {
	if m.Name == "@" {
		return domain
	}
	return m.Name + "." + domain
}

// autoMailDomainTx writes the domain's mail records when its zone is hosted here; it
// returns what it left out (a record of yours already uses the name).
func (s *API) autoMailDomainTx(ctx context.Context, r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain) ([]string, error) {
	z, rel, err := web.ZoneFor(ctx, tx, md.Domain)
	if err != nil || z == nil {
		return nil, err
	}
	var recs []web.DNSRecordInput
	for _, m := range s.mailRecords(md) {
		in := m.DNSRecordInput
		in.Name = web.Under(in.Name, rel)
		if m.Name == "@" {
			in.Name = rel
		}
		recs = append(recs, in)
	}
	return s.SetManagedTx(ctx, r, tx, o, z, "mail:"+md.ID, recs)
}

// deleteMailDomainTx removes the domain, its mailboxes (and their mail) and records. The
// sending domain stays.
func (s *API) deleteMailDomainTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain) error {
	if _, err := tx.Exec(r.Context(), `DELETE FROM mail_domains WHERE id = $1`, md.ID); err != nil {
		return err
	}
	if err := s.RemoveManagedTx(r.Context(), r, tx, o, "mail:"+md.ID); err != nil {
		return err
	}
	if md.Active() {
		if err := s.MailServer().DeleteDomain(r.Context(), md.StalwartID); err != nil {
			return mailErr(err)
		}
	}
	s.Audit(r, tx, o.ID, "mail.domain_deleted", "mail-domain:"+md.Domain, nil)
	return nil
}

var serverLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// setServerNameTx gives the domain its own server name (label.domain, such as
// mail.example.com) or removes it (label ""); its records are written again.
func (s *API) setServerNameTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, label string) error {
	label = strings.ToLower(strings.TrimSpace(label))
	name := ""
	if label != "" {
		if !serverLabelRe.MatchString(label) {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "server name: one word such as mail or webmail (letters, digits, dashes)"}
		}
		if !md.Active() {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "the domain is not receiving mail yet"}
		}
		name = label + "." + md.Domain
	}
	if _, err := tx.Exec(r.Context(), `UPDATE mail_domains SET server_name = $2 WHERE id = $1`, md.ID, name); err != nil {
		if web.IsUniqueViolation(err) {
			return web.APIErr{Status: http.StatusConflict, Msg: name + " is already used"}
		}
		return err
	}
	md.ServerName = name
	if _, err := s.autoMailDomainTx(r.Context(), r, tx, o, md); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "mail.server_name", "mail-domain:"+md.Domain, map[string]string{"name": name})
	_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`) // the proxy serves the name
	return err
}

func (s *API) setCatchAllTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, local string) error {
	local = strings.ToLower(strings.TrimSpace(local))
	addr := any(nil)
	if local != "" {
		var n int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM mailboxes WHERE mail_domain_id = $1 AND local_part = $2`, md.ID, local).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "choose one of the domain's mailboxes"}
		}
		addr = local + "@" + md.Domain
	}
	if !md.Active() {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "the domain is not receiving mail yet"}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE mail_domains SET catch_all = $2 WHERE id = $1`, md.ID, local); err != nil {
		return err
	}
	if err := s.MailServer().UpdateDomain(r.Context(), md.StalwartID, map[string]any{"catchAllAddress": addr}); err != nil {
		return mailErr(err)
	}
	s.Audit(r, tx, o.ID, "mail.catch_all", "mail-domain:"+md.Domain, map[string]string{"mailbox": local})
	return nil
}

// ---------- mailboxes ----------

type mailboxInput struct {
	LocalPart string `json:"name"`
	Name      string `json:"display_name"`
	QuotaMB   int    `json:"quota_mb"`
	Password  string `json:"password"` // empty: generated
}

func newMailboxPassword() string {
	t := strings.NewReplacer("-", "", "_", "").Replace(secure.NewToken())
	return t[:20]
}

func checkMailboxPassword(p string) error {
	if utf8.RuneCountInString(p) < minMailboxPassword || len(p) > 200 {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: fmt.Sprintf("the password needs at least %d characters", minMailboxPassword)}
	}
	return nil
}

// localPartTaken reports whether local is a mailbox or an alias of the domain.
func localPartTaken(ctx context.Context, tx pgx.Tx, mdID, local string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mailboxes WHERE mail_domain_id = $1 AND local_part = $2)
		OR EXISTS (SELECT 1 FROM mail_aliases WHERE mail_domain_id = $1 AND local_part = $2)`, mdID, local).Scan(&taken)
	return taken, err
}

// addMailboxTx creates a mailbox; it returns the password (generated if none was given).
func (s *API) addMailboxTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, in mailboxInput) (string, error) {
	bad := func(msg string) (string, error) {
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: msg}
	}
	if !md.Active() {
		return bad("the domain is not receiving mail yet: verify it first")
	}
	in.LocalPart = strings.ToLower(strings.TrimSpace(in.LocalPart))
	in.Name = strings.TrimSpace(in.Name)
	if !localPartRe.MatchString(in.LocalPart) {
		return bad("address: letters, digits, dots, dashes or underscores, such as anna or sales.team")
	}
	if utf8.RuneCountInString(in.Name) > 100 {
		return bad("name: at most 100 characters")
	}
	if in.QuotaMB == 0 {
		in.QuotaMB = 1024
	}
	if in.QuotaMB < 50 || in.QuotaMB > 1048576 {
		return bad("storage: from 50 MB")
	}
	if in.Password == "" {
		in.Password = newMailboxPassword()
	} else if err := checkMailboxPassword(in.Password); err != nil {
		return "", err
	}
	var count, used, maxBoxes, maxMB int
	if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM mailboxes), (SELECT coalesce(sum(quota_mb), 0) FROM mailboxes), max_mailboxes, max_mailbox_storage_mb
		FROM plans WHERE id = $1`, o.PlanID).Scan(&count, &used, &maxBoxes, &maxMB); err != nil {
		return "", err
	}
	if count >= maxBoxes {
		return bad(fmt.Sprintf("your plan allows %d mailboxes", maxBoxes))
	}
	if used+in.QuotaMB > maxMB {
		return bad(fmt.Sprintf("your plan allows %d MB of mailbox storage in all; %d MB are given to other mailboxes", maxMB, used))
	}
	if taken, err := localPartTaken(r.Context(), tx, md.ID, in.LocalPart); err != nil {
		return "", err
	} else if taken {
		return bad(in.LocalPart + "@" + md.Domain + " already exists")
	}
	ms := s.MailServer()
	id, err := ms.CreateAccount(r.Context(), md.StalwartID, in.LocalPart, in.Name, in.Password, int64(in.QuotaMB)<<20)
	if err != nil {
		return "", mailErr(err)
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO mailboxes (org_id, mail_domain_id, local_part, display_name, quota_mb, stalwart_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		o.ID, md.ID, in.LocalPart, in.Name, in.QuotaMB, id); err != nil {
		_ = ms.DeleteAccount(context.WithoutCancel(r.Context()), id)
		return "", err
	}
	s.Audit(r, tx, o.ID, "mail.mailbox_created", "mailbox:"+in.LocalPart+"@"+md.Domain, map[string]int{"quota_mb": in.QuotaMB})
	return in.Password, nil
}

func (s *API) updateMailboxTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, b *mailbox, name string, quotaMB int) error {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 100 {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "name: at most 100 characters"}
	}
	if quotaMB < 50 || quotaMB > 1048576 {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "storage: from 50 MB"}
	}
	var used, maxMB int
	if err := tx.QueryRow(r.Context(), `SELECT (SELECT coalesce(sum(quota_mb), 0) FROM mailboxes WHERE id <> $2), max_mailbox_storage_mb FROM plans WHERE id = $1`, o.PlanID, b.ID).Scan(&used, &maxMB); err != nil {
		return err
	}
	if used+quotaMB > maxMB {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: fmt.Sprintf("your plan allows %d MB of mailbox storage in all; %d MB are given to other mailboxes", maxMB, used)}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE mailboxes SET display_name = $2, quota_mb = $3 WHERE id = $1`, b.ID, name, quotaMB); err != nil {
		return err
	}
	ms := s.MailServer()
	if err := ms.SetDescription(r.Context(), b.StalwartID, name); err != nil {
		return mailErr(err)
	}
	if err := ms.SetQuota(r.Context(), b.StalwartID, int64(quotaMB)<<20); err != nil {
		return mailErr(err)
	}
	s.Audit(r, tx, o.ID, "mail.mailbox_updated", "mailbox:"+b.LocalPart+"@"+md.Domain, map[string]int{"quota_mb": quotaMB})
	return nil
}

// resetMailboxPasswordTx sets a new password (generated if empty) and returns it.
func (s *API) resetMailboxPasswordTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, b *mailbox, password string) (string, error) {
	if password == "" {
		password = newMailboxPassword()
	} else if err := checkMailboxPassword(password); err != nil {
		return "", err
	}
	if err := s.MailServer().SetPassword(r.Context(), b.StalwartID, password); err != nil {
		return "", mailErr(err)
	}
	s.Audit(r, tx, o.ID, "mail.mailbox_password_reset", "mailbox:"+b.LocalPart+"@"+md.Domain, nil)
	return password, nil
}

func (s *API) deleteMailboxTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, b *mailbox) error {
	if md.CatchAll == b.LocalPart {
		if err := s.setCatchAllTx(r, tx, o, md, ""); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM mailboxes WHERE id = $1`, b.ID); err != nil {
		return err
	}
	if err := s.MailServer().DeleteAccount(r.Context(), b.StalwartID); err != nil {
		return mailErr(err)
	}
	s.Audit(r, tx, o.ID, "mail.mailbox_deleted", "mailbox:"+b.LocalPart+"@"+md.Domain, nil)
	return nil
}

// syncAliases sends the mailbox's aliases to the mail server.
func (s *API) syncAliases(ctx context.Context, tx pgx.Tx, md *mailDomain, b *mailbox) error {
	rows, err := tx.Query(ctx, `SELECT local_part FROM mail_aliases WHERE mailbox_id = $1 ORDER BY local_part`, b.ID)
	if err != nil {
		return err
	}
	var as []stalwart.Alias
	for rows.Next() {
		var l string
		if rows.Scan(&l) == nil {
			as = append(as, stalwart.Alias{Name: l, DomainID: md.StalwartID})
		}
	}
	rows.Close()
	if err := s.MailServer().SetAliases(ctx, b.StalwartID, as); err != nil {
		return mailErr(err)
	}
	return nil
}

func (s *API) addAliasTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, b *mailbox, local string) error {
	local = strings.ToLower(strings.TrimSpace(local))
	if !localPartRe.MatchString(local) {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "alias: letters, digits, dots, dashes or underscores, such as sales"}
	}
	if taken, err := localPartTaken(r.Context(), tx, md.ID, local); err != nil {
		return err
	} else if taken {
		return web.APIErr{Status: http.StatusConflict, Msg: local + "@" + md.Domain + " already exists"}
	}
	var n int
	if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM mail_aliases WHERE mailbox_id = $1`, b.ID).Scan(&n); err != nil {
		return err
	}
	if n >= 50 {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "a mailbox has at most 50 aliases"}
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO mail_aliases (org_id, mailbox_id, mail_domain_id, local_part) VALUES ($1, $2, $3, $4)`, o.ID, b.ID, md.ID, local); err != nil {
		return err
	}
	if err := s.syncAliases(r.Context(), tx, md, b); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "mail.alias_added", "mailbox:"+b.LocalPart+"@"+md.Domain, map[string]string{"alias": local})
	return nil
}

func (s *API) deleteAliasTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, md *mailDomain, local string) error {
	var boxID string
	if err := tx.QueryRow(r.Context(), `DELETE FROM mail_aliases WHERE mail_domain_id = $1 AND local_part = $2 RETURNING mailbox_id`, md.ID, local).Scan(&boxID); err != nil {
		return err
	}
	var b mailbox
	if err := tx.QueryRow(r.Context(), `SELECT id, local_part, stalwart_id FROM mailboxes WHERE id = $1`, boxID).Scan(&b.ID, &b.LocalPart, &b.StalwartID); err != nil {
		return err
	}
	if err := s.syncAliases(r.Context(), tx, md, &b); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "mail.alias_removed", "mailbox:"+b.LocalPart+"@"+md.Domain, map[string]string{"alias": local})
	return nil
}

// fillUsage adds each mailbox's disk use (best effort: the page works without it).
func (s *API) fillUsage(ctx context.Context, bs []*mailbox) {
	ids := make([]string, 0, len(bs))
	for _, b := range bs {
		ids = append(ids, b.StalwartID)
	}
	accts, err := s.MailServer().Accounts(ctx, ids)
	if err != nil {
		return
	}
	used := map[string]int64{}
	for _, a := range accts {
		used[a.ID] = a.Used
	}
	for _, b := range bs {
		b.UsedMB = float64(used[b.StalwartID]) / (1 << 20)
	}
}

// ---------- console ----------

func (s *API) mailPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	var ds []*mailDomain
	var count, used, maxBoxes, maxMB int
	if err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if ds, err = loadMailDomains(r.Context(), tx); err != nil {
			return err
		}
		return tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM mailboxes), (SELECT coalesce(sum(quota_mb), 0) FROM mailboxes), max_mailboxes, max_mailbox_storage_mb
			FROM plans WHERE id = $1`, o.PlanID).Scan(&count, &used, &maxBoxes, &maxMB)
	}); err != nil {
		s.ServerError(w, r, err)
		return
	}
	s.Render(w, r, "mail", http.StatusOK, web.Data{"Org": o, "Domains": ds, "Enabled": s.MailHostingEnabled(),
		"Count": count, "UsedMB": used, "MaxBoxes": maxBoxes, "MaxMB": maxMB, "MailHost": s.Cfg.MailHost(), "Webmail": s.webmailURL(),
		"CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *API) addMailDomain(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	var md *mailDomain
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		md, err = s.addMailDomainTx(r, tx, o, r.PostFormValue("domain"))
		return err
	})
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/mail", "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	case md.Active():
		web.Back(w, r, o.Path()+"/mail/"+md.Domain, "Domain added: create its mailboxes.", "")
	default:
		web.Back(w, r, o.Path()+"/mail/"+md.Domain, "Domain added. Create the DNS records below; mailboxes can be created once it is verified.", "")
	}
}

// withMailDomain resolves the organization and the mail domain of the path.
func (s *API) withMailDomain(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *web.OrgInfo, md *mailDomain) error) (*web.OrgInfo, *mailDomain, error, bool) {
	o, ok := s.Org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var md *mailDomain
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if md, err = loadMailDomain(r.Context(), tx, r.PathValue("domain")); err != nil {
			return err
		}
		return fn(tx, o, md)
	})
	if md == nil && web.IsNoRows(err) {
		s.NotFound(w, r)
		return nil, nil, nil, false
	}
	return o, md, err, true
}

func (s *API) mailDomainPage(w http.ResponseWriter, r *http.Request) {
	s.renderMailDomain(w, r, nil)
}

func (s *API) renderMailDomain(w http.ResponseWriter, r *http.Request, extra web.Data) {
	var boxes []*mailbox
	var zone string
	var foreignMX bool
	o, md, err, ok := s.withMailDomain(w, r, "viewer", func(tx pgx.Tx, o *web.OrgInfo, md *mailDomain) error {
		var err error
		if boxes, err = loadMailboxes(r.Context(), tx, md.ID); err != nil {
			return err
		}
		if z, rel, err := web.ZoneFor(r.Context(), tx, md.Domain); err == nil && z != nil {
			zone = z.Domain
			_ = tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM dns_records WHERE zone_id = $1 AND name = $2 AND type = 'MX' AND managed_by IS DISTINCT FROM $3)`,
				z.ID, rel, "mail:"+md.ID).Scan(&foreignMX)
		}
		return nil
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	if md.Active() {
		s.fillUsage(r.Context(), boxes)
	}
	d := web.Data{"Org": o, "Domain": md, "Mailboxes": boxes, "Records": s.mailRecords(md), "Zone": zone, "ForeignMX": foreignMX,
		"MailHost": s.clientHost(md), "CanEdit": model.RoleAtLeast(o.Role, "developer"), "MinPassword": minMailboxPassword, "Webmail": s.domainWebmailURL(md),
		"PublicIP": s.Cfg.PublicIP}
	for k, v := range extra {
		d[k] = v
	}
	s.Render(w, r, "mail_domain", http.StatusOK, d)
}

// mailAction handles every change on a mail domain's page.
func (s *API) mailAction(w http.ResponseWriter, r *http.Request) {
	var msg, newPassword, forAddr string
	var deleted bool
	o, md, err, ok := s.withMailDomain(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, md *mailDomain) error {
		box := func() (*mailbox, error) { return loadMailbox(r.Context(), tx, md, r.PostFormValue("mailbox")) }
		switch r.PostFormValue("action") {
		case "activate":
			if err := s.activateMailDomainTx(r, tx, o, md); err != nil {
				return err
			}
			msg = "The domain receives mail now: create its mailboxes."
		case "records":
			if md.Active() { // the keys may have been rotated
				if zone, err := s.MailServer().ZoneFile(r.Context(), md.StalwartID); err == nil && strings.Contains(zone, "._domainkey.") {
					md.DKIM = dkimLines(zone)
					if _, err := tx.Exec(r.Context(), `UPDATE mail_domains SET dkim = $2 WHERE id = $1`, md.ID, md.DKIM); err != nil {
						return err
					}
				}
			}
			skipped, err := s.autoMailDomainTx(r.Context(), r, tx, o, md)
			if err != nil {
				return err
			}
			msg = "DNS records written again."
			if len(skipped) > 0 {
				msg += " Kept yours: " + strings.Join(skipped, ", ") + "."
			}
		case "add":
			q, _ := strconv.Atoi(r.PostFormValue("quota_mb"))
			p, err := s.addMailboxTx(r, tx, o, md, mailboxInput{LocalPart: r.PostFormValue("local_part"), Name: r.PostFormValue("display_name"), QuotaMB: q, Password: r.PostFormValue("password")})
			if err != nil {
				return err
			}
			forAddr = strings.ToLower(strings.TrimSpace(r.PostFormValue("local_part"))) + "@" + md.Domain
			if r.PostFormValue("password") == "" {
				newPassword = p
			}
			msg = forAddr + " created."
		case "update":
			b, err := box()
			if err != nil {
				return err
			}
			q, _ := strconv.Atoi(r.PostFormValue("quota_mb"))
			if err := s.updateMailboxTx(r, tx, o, md, b, r.PostFormValue("display_name"), q); err != nil {
				return err
			}
			msg = b.LocalPart + "@" + md.Domain + " saved."
		case "password":
			b, err := box()
			if err != nil {
				return err
			}
			p, err := s.resetMailboxPasswordTx(r, tx, o, md, b, r.PostFormValue("password"))
			if err != nil {
				return err
			}
			forAddr = b.LocalPart + "@" + md.Domain
			if r.PostFormValue("password") == "" {
				newPassword = p
			}
			msg = "New password set for " + forAddr + "."
		case "delete":
			b, err := box()
			if err != nil {
				return err
			}
			if r.PostFormValue("confirm") != b.LocalPart+"@"+md.Domain {
				return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "type the address to delete the mailbox"}
			}
			if err := s.deleteMailboxTx(r, tx, o, md, b); err != nil {
				return err
			}
			msg = b.LocalPart + "@" + md.Domain + " and its mail deleted."
		case "alias":
			b, err := box()
			if err != nil {
				return err
			}
			if err := s.addAliasTx(r, tx, o, md, b, r.PostFormValue("alias")); err != nil {
				return err
			}
			msg = "Alias added."
		case "unalias":
			if err := s.deleteAliasTx(r, tx, o, md, r.PostFormValue("alias")); err != nil {
				return err
			}
			msg = "Alias removed."
		case "catchall":
			if err := s.setCatchAllTx(r, tx, o, md, r.PostFormValue("mailbox")); err != nil {
				return err
			}
			msg = "Catch-all saved."
		case "servername":
			if err := s.setServerNameTx(r, tx, o, md, r.PostFormValue("label")); err != nil {
				return err
			}
			if md.ServerName == "" {
				msg = "Back to the platform's server name."
			} else {
				msg = md.ServerName + " saved. Once its DNS record points here, its certificate is issued within minutes; then the webmail and mail apps use it."
			}
		case "remove":
			if r.PostFormValue("confirm") != md.Domain {
				return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "type the domain to remove it"}
			}
			if err := s.deleteMailDomainTx(r, tx, o, md); err != nil {
				return err
			}
			deleted = true
		default:
			return web.APIErr{Status: http.StatusBadRequest, Msg: "unknown action"}
		}
		return nil
	})
	if !ok {
		return
	}
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/mail/"+md.Domain, "", ae.Msg)
	case web.IsNoRows(err):
		web.Back(w, r, o.Path()+"/mail/"+md.Domain, "", "That mailbox or alias does not exist.")
	case err != nil:
		s.ServerError(w, r, err)
	case deleted:
		web.Back(w, r, o.Path()+"/mail", md.Domain+" removed with its mailboxes.", "")
	case newPassword != "":
		s.renderMailDomain(w, r, web.Data{"NewPassword": newPassword, "PasswordFor": forAddr, "Msg": msg})
	default:
		web.Back(w, r, o.Path()+"/mail/"+md.Domain, msg, "")
	}
}

// ---------- API ----------

type apiMailDomain struct {
	Domain     string        `json:"domain"`
	Status     string        `json:"status"` // pending (verify the sending domain), active
	Mailboxes  int           `json:"mailboxes"`
	CatchAll   string        `json:"catch_all,omitempty"`
	ServerName string        `json:"server_name,omitempty"`
	Records    []apiMailDNS  `json:"dns_records"`
	Settings   apiMailClient `json:"client_settings"`
}

type apiMailDNS struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Priority int    `json:"priority,omitempty"`
	Purpose  string `json:"purpose"`
}

type apiMailClient struct {
	IMAP string `json:"imap"`
	POP3 string `json:"pop3"`
	SMTP string `json:"smtp"`
}

func (s *API) toAPIMailDomain(md *mailDomain) apiMailDomain {
	out := apiMailDomain{Domain: md.Domain, Status: "pending", Mailboxes: md.Mailboxes, Records: []apiMailDNS{}}
	if md.Active() {
		out.Status = "active"
	}
	if md.CatchAll != "" {
		out.CatchAll = md.CatchAll + "@" + md.Domain
	}
	for _, m := range s.mailRecords(md) {
		out.Records = append(out.Records, apiMailDNS{m.FQDN(md.Domain), m.Type, m.Value, m.Priority, m.Purpose})
	}
	h := s.clientHost(md)
	out.ServerName = md.ServerName
	out.Settings = apiMailClient{IMAP: h + ":993 (TLS)", POP3: h + ":995 (TLS)", SMTP: h + ":587 (STARTTLS)"}
	return out
}

type apiMailbox struct {
	Address  string   `json:"address"`
	Name     string   `json:"display_name"`
	QuotaMB  int      `json:"quota_mb"`
	UsedMB   float64  `json:"used_mb"`
	Aliases  []string `json:"aliases"`
	Password string   `json:"password,omitempty"` // only when the platform generated it
}

func toAPIMailbox(md *mailDomain, b *mailbox) apiMailbox {
	out := apiMailbox{Address: b.LocalPart + "@" + md.Domain, Name: b.Name, QuotaMB: b.QuotaMB, UsedMB: b.UsedMB, Aliases: []string{}}
	for _, a := range b.Aliases {
		out.Aliases = append(out.Aliases, a.LocalPart+"@"+md.Domain)
	}
	return out
}

func (s *API) apiMailDomains(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	out := []apiMailDomain{}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		ds, err := loadMailDomains(r.Context(), tx)
		for _, d := range ds {
			out = append(out, s.toAPIMailDomain(d))
		}
		return err
	})
	if web.APITx(w, s.Server, err) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiAddMailDomain(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Domain string `json:"domain"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	var out apiMailDomain
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		md, err := s.addMailDomainTx(r, tx, o, in.Domain)
		if err == nil {
			out = s.toAPIMailDomain(md)
		}
		return err
	})
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusCreated)
		web.WriteJSON(w, out)
	}
}

// apiMailDomainTx runs fn on the domain of the path inside an org transaction.
func (s *API) apiMailDomainTx(w http.ResponseWriter, r *http.Request, o *web.OrgInfo, fn func(tx pgx.Tx, md *mailDomain) error) bool {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		md, err := loadMailDomain(r.Context(), tx, r.PathValue("domain"))
		if err != nil {
			return err
		}
		return fn(tx, md)
	})
	return web.APITx(w, s.Server, err)
}

func (s *API) apiMailDomain(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out apiMailDomain
	if s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error {
		out = s.toAPIMailDomain(md)
		return nil
	}) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiActivateMailDomain(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var out apiMailDomain
	if s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error {
		if err := s.activateMailDomainTx(r, tx, o, md); err != nil {
			return err
		}
		out = s.toAPIMailDomain(md)
		return nil
	}) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiDeleteMailDomain(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error { return s.deleteMailDomainTx(r, tx, o, md) }) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *API) apiMailboxes(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	out := []apiMailbox{}
	var md *mailDomain
	var boxes []*mailbox
	if !s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, d *mailDomain) error {
		var err error
		md = d
		boxes, err = loadMailboxes(r.Context(), tx, d.ID)
		return err
	}) {
		return
	}
	if md.Active() {
		s.fillUsage(r.Context(), boxes)
	}
	for _, b := range boxes {
		out = append(out, toAPIMailbox(md, b))
	}
	web.WriteJSON(w, out)
}

func (s *API) apiAddMailbox(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in mailboxInput
	if !decodeJSON(w, r, &in) {
		return
	}
	var out apiMailbox
	if s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error {
		p, err := s.addMailboxTx(r, tx, o, md, in)
		if err != nil {
			return err
		}
		b, err := loadMailbox(r.Context(), tx, md, strings.ToLower(strings.TrimSpace(in.LocalPart)))
		if err != nil {
			return err
		}
		out = toAPIMailbox(md, b)
		if in.Password == "" {
			out.Password = p
		}
		return nil
	}) {
		w.WriteHeader(http.StatusCreated)
		web.WriteJSON(w, out)
	}
}

// apiMailboxTx runs fn on the mailbox of the path.
func (s *API) apiMailboxTx(w http.ResponseWriter, r *http.Request, o *web.OrgInfo, fn func(tx pgx.Tx, md *mailDomain, b *mailbox) error) bool {
	return s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error {
		b, err := loadMailbox(r.Context(), tx, md, r.PathValue("mailbox"))
		if err != nil {
			return err
		}
		return fn(tx, md, b)
	})
}

func (s *API) apiUpdateMailbox(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Name    *string `json:"display_name"`
		QuotaMB *int    `json:"quota_mb"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	var out apiMailbox
	if s.apiMailboxTx(w, r, o, func(tx pgx.Tx, md *mailDomain, b *mailbox) error {
		name, quota := b.Name, b.QuotaMB
		if in.Name != nil {
			name = *in.Name
		}
		if in.QuotaMB != nil {
			quota = *in.QuotaMB
		}
		if err := s.updateMailboxTx(r, tx, o, md, b, name, quota); err != nil {
			return err
		}
		b.Name, b.QuotaMB = strings.TrimSpace(name), quota
		out = toAPIMailbox(md, b)
		return nil
	}) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiMailboxPassword(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Password string `json:"password"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	var out map[string]string
	if s.apiMailboxTx(w, r, o, func(tx pgx.Tx, md *mailDomain, b *mailbox) error {
		p, err := s.resetMailboxPasswordTx(r, tx, o, md, b, in.Password)
		if err != nil {
			return err
		}
		out = map[string]string{"address": b.LocalPart + "@" + md.Domain}
		if in.Password == "" {
			out["password"] = p
		}
		return nil
	}) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiDeleteMailbox(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if s.apiMailboxTx(w, r, o, func(tx pgx.Tx, md *mailDomain, b *mailbox) error { return s.deleteMailboxTx(r, tx, o, md, b) }) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *API) apiAddMailAlias(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Alias string `json:"alias"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if s.apiMailboxTx(w, r, o, func(tx pgx.Tx, md *mailDomain, b *mailbox) error {
		return s.addAliasTx(r, tx, o, md, b, strings.TrimSuffix(strings.ToLower(in.Alias), "@"+md.Domain))
	}) {
		w.WriteHeader(http.StatusCreated)
		web.WriteJSON(w, map[string]string{"message": "alias added"})
	}
}

func (s *API) apiDeleteMailAlias(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	if s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error {
		return s.deleteAliasTx(r, tx, o, md, strings.TrimSuffix(strings.ToLower(r.PathValue("alias")), "@"+md.Domain))
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *API) apiSetCatchAll(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Mailbox string `json:"mailbox"` // local part or address; empty turns it off
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error {
		return s.setCatchAllTx(r, tx, o, md, strings.TrimSuffix(strings.ToLower(in.Mailbox), "@"+md.Domain))
	}) {
		web.WriteJSON(w, map[string]string{"message": "catch-all saved"})
	}
}

// decodeJSON reads a small JSON body into v, answering 400 itself when it cannot.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v); err != nil {
		web.APIError(w, http.StatusBadRequest, "the body must be JSON")
		return false
	}
	return true
}

func (s *API) apiSetServerName(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var in struct {
		Name string `json:"name"` // the label, such as "mail"; empty: the platform's name
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	var out apiMailDomain
	if s.apiMailDomainTx(w, r, o, func(tx pgx.Tx, md *mailDomain) error {
		if err := s.setServerNameTx(r, tx, o, md, strings.TrimSuffix(strings.ToLower(in.Name), "."+md.Domain)); err != nil {
			return err
		}
		out = s.toAPIMailDomain(md)
		return nil
	}) {
		web.WriteJSON(w, out)
	}
}

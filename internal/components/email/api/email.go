package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/mail"
	"platform/internal/lib/model"
	"platform/internal/lib/secure"
)

// Outgoing email (SES-like): verified sending domains with DKIM, a send API and a
// sending log. The agent delivers through the platform's SMTP relay.

var emailDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

const maxEmailBytes = 256 << 10

func loadSendDomain(ctx context.Context, tx pgx.Tx, domain string) (*web.SendDomain, error) {
	return web.ScanSendDomain(tx.QueryRow(ctx, `SELECT `+web.SendDomainCols+` FROM email_domains WHERE domain = $1`, domain))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *API) addEmailDomainTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, domain string) error {
	if !s.EmailEnabled() {
		return web.APIErr{Status: http.StatusServiceUnavailable, Msg: "outgoing email is not configured on this platform"}
	}
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if len(domain) > 253 || !emailDomainRe.MatchString(domain) {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "enter a domain name such as example.com"}
	}
	var n, max int
	if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM email_domains), max_email_domains FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return err
	}
	if n >= max {
		return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: fmt.Sprintf("your plan allows %d sending domains", max)}
	}
	priv, txt, err := mail.NewDKIMKey()
	if err != nil {
		return err
	}
	var id string
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO email_domains (org_id, domain, verify_token, dkim_selector, dkim_key_enc, dkim_txt) VALUES ($1, $2, $3, $4, '', $5) RETURNING id`,
		o.ID, domain, randomHex(16), "p"+randomHex(3), txt).Scan(&id); err != nil {
		if web.IsUniqueViolation(err) {
			return web.APIErr{Status: http.StatusConflict, Msg: "this domain is already added"}
		}
		return err
	}
	enc, err := s.Box.Seal(priv, "emaildkim:"+id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(), `UPDATE email_domains SET dkim_key_enc = $2 WHERE id = $1`, id, enc); err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'email')`); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "email.domain_added", "email-domain:"+domain, nil)
	// in a zone hosted here: the platform writes its records (and verifies it if it can)
	if d, err := loadSendDomain(r.Context(), tx, domain); err == nil {
		if _, err := s.AutoEmailDomainTx(r.Context(), r, tx, o, d); err != nil {
			return err
		}
	}
	return nil
}

func (s *API) deleteEmailDomainTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, d *web.SendDomain) error {
	var hosted bool
	if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM mail_domains WHERE email_domain_id = $1)`, d.ID).Scan(&hosted); err != nil {
		return err
	}
	if hosted {
		return web.APIErr{Status: http.StatusConflict, Msg: "this domain has mailboxes: remove it from Mailboxes first"}
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM email_domains WHERE id = $1`, d.ID); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, "email.domain_deleted", "email-domain:"+d.Domain, nil)
	return s.RemoveManagedTx(r.Context(), r, tx, o, "email:"+d.ID)
}

type sendRequest struct {
	From        string           `json:"from"`
	To          []string         `json:"to"`
	ReplyTo     string           `json:"reply_to"`
	Subject     string           `json:"subject"`
	Text        string           `json:"text"`
	HTML        string           `json:"html"`
	Attachments []sendAttachment `json:"attachments"`
}

type sendAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"` // base64
}

// queueEmailTx checks a message and queues it for the agent; returns its id.
func (s *API) queueEmailTx(r *http.Request, tx pgx.Tx, o *web.OrgInfo, m sendRequest) (string, error) {
	bad := func(msg string) (string, error) {
		return "", web.APIErr{Status: http.StatusUnprocessableEntity, Msg: msg}
	}
	if !s.EmailEnabled() {
		return "", web.APIErr{Status: http.StatusServiceUnavailable, Msg: "outgoing email is not configured on this platform"}
	}
	from, fromAddr, err := mail.ParseAddress(m.From)
	if err != nil {
		return bad("from: a valid address such as \"Shop <hello@example.com>\"")
	}
	domain := fromAddr[strings.LastIndex(fromAddr, "@")+1:]
	var domainID, status string
	if err := tx.QueryRow(r.Context(), `SELECT id, status FROM email_domains WHERE domain = $1`, domain).Scan(&domainID, &status); err != nil || status != "verified" {
		return bad("from: " + domain + " is not a verified sending domain")
	}
	if len(m.To) == 0 || len(m.To) > 50 {
		return bad("to: 1 to 50 recipients")
	}
	to := make([]string, 0, len(m.To))
	for _, t := range m.To {
		f, _, err := mail.ParseAddress(t)
		if err != nil {
			return bad("to: " + t + " is not a valid address")
		}
		to = append(to, f)
	}
	replyTo := ""
	if m.ReplyTo != "" {
		if replyTo, _, err = mail.ParseAddress(m.ReplyTo); err != nil {
			return bad("reply_to: not a valid address")
		}
	}
	switch {
	case strings.TrimSpace(m.Subject) == "" || len(m.Subject) > 998 || strings.ContainsAny(m.Subject, "\r\n"):
		return bad("subject: one line, 1 to 998 characters")
	case m.Text == "" && m.HTML == "":
		return bad("give a text or html body")
	}
	if len(m.Text)+len(m.HTML) > maxEmailBytes {
		return bad("the text and html bodies together are larger than 256 KB")
	}
	atts, msg := parseAttachments(m.Attachments)
	if msg != "" {
		return bad(msg)
	}
	var raw []byte
	size := len(m.Subject) + len(m.Text) + len(m.HTML)
	if len(atts) > 0 {
		raw = []byte(mail.Compose(mail.Message{From: from, ReplyTo: replyTo, To: to, Subject: m.Subject, Text: m.Text, HTML: m.HTML, Attachments: atts}, domain, time.Now()))
		size = len(raw)
	}
	source := "api"
	if r.URL.Path != "" && !strings.HasPrefix(r.URL.Path, "/api/") {
		source = "console"
	}
	return s.enqueueEmailTx(r.Context(), tx, o, queued{DomainID: domainID, From: from, To: to, ReplyTo: replyTo, Subject: m.Subject,
		Text: m.Text, HTML: m.HTML, Raw: raw, Size: size, Source: source})
}

// queued is a checked message ready to be stored.
type queued struct {
	DomainID, From, ReplyTo, Subject, Text, HTML, Source string
	To                                                   []string
	Raw                                                  []byte
	Size                                                 int
}

// enqueueEmailTx applies the plan's limits and stores the message and its recipients.
func (s *API) enqueueEmailTx(ctx context.Context, tx pgx.Tx, o *web.OrgInfo, q queued) (string, error) {
	var sent, maxDay, maxBytes int
	if err := tx.QueryRow(ctx, `
		SELECT coalesce((SELECT sum(cardinality(to_addrs)) FROM email_messages WHERE created_at > now() - interval '1 day'), 0), max_emails_day, max_email_bytes
		FROM plans WHERE id = $1`, o.PlanID).Scan(&sent, &maxDay, &maxBytes); err != nil {
		return "", err
	}
	if q.Size > maxBytes {
		return "", web.APIErr{Status: http.StatusRequestEntityTooLarge, Msg: fmt.Sprintf("the message is larger than %d MB", maxBytes>>20)}
	}
	if sent+len(q.To) > maxDay {
		return "", web.APIErr{Status: http.StatusTooManyRequests, Msg: fmt.Sprintf("your plan sends at most %d emails a day", maxDay)}
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO email_messages (org_id, domain_id, from_addr, to_addrs, reply_to, subject, body_text, body_html, raw, size_bytes, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		o.ID, q.DomainID, q.From, q.To, q.ReplyTo, q.Subject, q.Text, q.HTML, q.Raw, q.Size, q.Source).Scan(&id); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, t := range q.To {
		_, addr, err := mail.ParseAddress(t)
		if err != nil || seen[addr] {
			continue
		}
		seen[addr] = true
		if _, err := tx.Exec(ctx, `INSERT INTO email_recipients (message_id, org_id, address) VALUES ($1, $2, $3)`, id, o.ID, addr); err != nil {
			return "", err
		}
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'email')`)
	return id, err
}

// blockedExtensions are attachment types mail providers refuse (programs, scripts).
var blockedExtensions = map[string]bool{"exe": true, "bat": true, "cmd": true, "com": true, "scr": true, "pif": true, "vbs": true,
	"js": true, "jse": true, "wsf": true, "msi": true, "jar": true, "ps1": true, "cpl": true, "hta": true, "lnk": true}

// parseAttachments decodes and checks attachments; msg explains a refusal.
func parseAttachments(in []sendAttachment) (out []mail.Attachment, msg string) {
	if len(in) > 20 {
		return nil, "attachments: at most 20"
	}
	for i, a := range in {
		name := strings.TrimSpace(a.Filename)
		if name == "" || len(name) > 200 || strings.ContainsAny(name, "\r\n\"/\\") {
			return nil, fmt.Sprintf("attachments[%d].filename: 1 to 200 characters, no slashes or quotes", i)
		}
		if ext := strings.ToLower(name[strings.LastIndex(name, ".")+1:]); strings.Contains(name, ".") && blockedExtensions[ext] {
			return nil, fmt.Sprintf("attachments[%d]: .%s files are refused by mail providers", i, ext)
		}
		ct := strings.TrimSpace(a.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}
		if mt, _, err := mime.ParseMediaType(ct); err != nil || !strings.Contains(mt, "/") || strings.ContainsAny(ct, "\r\n") {
			return nil, fmt.Sprintf("attachments[%d].content_type: a media type such as application/pdf", i)
		} else {
			ct = mt
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Content))
		if err != nil || len(data) == 0 {
			return nil, fmt.Sprintf("attachments[%d].content: the file's bytes in base64", i)
		}
		out = append(out, mail.Attachment{Filename: name, ContentType: ct, Data: data})
	}
	return out, ""
}

type emailMessage struct {
	ID, From, Subject, Status, Error, Source string
	To                                       []string
	Created                                  time.Time
	Sent                                     *time.Time
	Delivered, Bounced, Suppressed, Failed   int
}

func loadEmailMessages(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]emailMessage, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.from_addr, m.to_addrs, m.subject, m.status, m.error, m.source, m.created_at, m.sent_at,
		       count(r.*) FILTER (WHERE r.status = 'delivered')::int, count(r.*) FILTER (WHERE r.status = 'bounced')::int,
		       count(r.*) FILTER (WHERE r.status = 'suppressed')::int, count(r.*) FILTER (WHERE r.status = 'failed')::int
		FROM email_messages m LEFT JOIN email_recipients r ON r.message_id = m.id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []emailMessage{}
	for rows.Next() {
		var m emailMessage
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Subject, &m.Status, &m.Error, &m.Source, &m.Created, &m.Sent,
			&m.Delivered, &m.Bounced, &m.Suppressed, &m.Failed); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- REST API ----------

type apiEmailDomain struct {
	Domain   string          `json:"domain"`
	Status   string          `json:"status"`
	Message  string          `json:"message,omitempty"`
	Records  []web.DNSRecord `json:"dns_records"`
	Verified *time.Time      `json:"verified_at"`
}

func (s *API) sendDomainView(d *web.SendDomain) apiEmailDomain {
	return apiEmailDomain{d.Domain, d.Status, d.Message, d.Records(s.SpfMechanism()), d.Verified}
}

func (s *API) apiEmailDomains(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	out := []apiEmailDomain{}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		ds, err := web.LoadSendDomains(r.Context(), tx)
		for _, d := range ds {
			out = append(out, s.sendDomainView(d))
		}
		return err
	})
	if web.APITx(w, s.Server, err) {
		web.WriteJSON(w, out)
	}
}

func (s *API) apiAddEmailDomain(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var b struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b); err != nil {
		web.APIError(w, http.StatusBadRequest, `body must be JSON, for example {"domain": "example.com"}`)
		return
	}
	var view apiEmailDomain
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		if err := s.addEmailDomainTx(r, tx, o, b.Domain); err != nil {
			return err
		}
		d, err := loadSendDomain(r.Context(), tx, strings.TrimSuffix(strings.ToLower(strings.TrimSpace(b.Domain)), "."))
		if err == nil {
			view = s.sendDomainView(d)
		}
		return err
	})
	if web.APITx(w, s.Server, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(view)
	}
}

func (s *API) apiDeleteEmailDomain(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		d, err := loadSendDomain(r.Context(), tx, r.PathValue("domain"))
		if err != nil {
			return err
		}
		return s.deleteEmailDomainTx(r, tx, o, d)
	})
	if web.APITx(w, s.Server, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *API) apiSendEmail(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var m sendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&m); err != nil { // 10 MB of attachments in base64
		web.APIError(w, http.StatusBadRequest, `body must be JSON: {"from": "...", "to": ["..."], "subject": "...", "text": "..."}`)
		return
	}
	var id string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		id, err = s.queueEmailTx(r, tx, o, m)
		return err
	})
	if web.APITx(w, s.Server, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "queued"})
	}
}

func (s *API) apiEmailMessage(w http.ResponseWriter, r *http.Request, o *web.OrgInfo) {
	var ms []emailMessage
	var rcpts []recipientView
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if ms, err = loadEmailMessages(r.Context(), tx, `WHERE m.id::text = $1 GROUP BY m.id`, r.PathValue("id")); err != nil || len(ms) == 0 {
			return err
		}
		rcpts, err = loadRecipients(r.Context(), tx, ms[0].ID)
		return err
	})
	if !web.APITx(w, s.Server, err) {
		return
	}
	if len(ms) == 0 {
		web.APIError(w, http.StatusNotFound, "not found")
		return
	}
	m := ms[0]
	web.WriteJSON(w, map[string]any{"id": m.ID, "from": m.From, "to": m.To, "subject": m.Subject, "status": m.Status, "error": m.Error,
		"source": m.Source, "created_at": m.Created, "sent_at": m.Sent, "recipients": rcpts})
}

// ---------- console ----------

func (s *API) emailPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	s.renderEmailPage(w, r, o, nil)
}

func (s *API) renderEmailPage(w http.ResponseWriter, r *http.Request, o *web.OrgInfo, extra web.Data) {
	var ds []*web.SendDomain
	var supps []suppression
	var hook *webhookView
	var msgs []emailMessage
	var services []string
	var sent, max int
	if err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if ds, err = web.LoadSendDomains(r.Context(), tx); err != nil {
			return err
		}
		if msgs, err = loadEmailMessages(r.Context(), tx, `GROUP BY m.id ORDER BY m.created_at DESC LIMIT 100`); err != nil {
			return err
		}
		if supps, err = loadSuppressions(r.Context(), tx, 200); err != nil {
			return err
		}
		if hook, err = loadEmailWebhook(r.Context(), tx); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), `
			SELECT coalesce((SELECT sum(cardinality(to_addrs)) FROM email_messages WHERE created_at > now() - interval '1 day'), 0), max_emails_day
			FROM plans WHERE id = $1`, o.PlanID).Scan(&sent, &max); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `SELECT p.slug || '/' || s.slug FROM services s JOIN projects p ON p.id = s.project_id WHERE s.preview_of IS NULL ORDER BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			services = append(services, n)
		}
		return rows.Err()
	}); err != nil {
		s.ServerError(w, r, err)
		return
	}
	var verified []string
	for _, d := range ds {
		if d.Status == "verified" {
			verified = append(verified, d.Domain)
		}
	}
	d := web.Data{"Org": o, "Domains": ds, "Verified": verified, "Messages": msgs, "Services": services,
		"SentToday": sent, "MaxDay": max, "Enabled": s.EmailEnabled(), "CanEdit": model.RoleAtLeast(o.Role, "developer"),
		"Suppressions": supps, "Webhook": hook, "Events": emailEvents, "Mode": s.Cfg.EmailMode(),
		"SMTPHost": s.Cfg.MailHost(), "SMTPEnabled": s.Cfg.SMTPSubmissionListen != "", "APIURL": s.Cfg.BaseURL + "/api/v1/email/send"}
	for k, v := range extra {
		d[k] = v
	}
	s.Render(w, r, "email", http.StatusOK, d)
}

func (s *API) addEmailDomain(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.PostFormValue("domain"))), ".")
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error { return s.addEmailDomainTx(r, tx, o, domain) })
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/email#domains", "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, o.Path()+"/email/d/"+domain, "Domain added. Create the DNS records below; verification runs every minute.", "")
	}
}

func (s *API) withEmailDomain(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *web.OrgInfo, d *web.SendDomain) error) (*web.OrgInfo, *web.SendDomain, error, bool) {
	o, ok := s.Org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var d *web.SendDomain
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if d, err = loadSendDomain(r.Context(), tx, r.PathValue("domain")); err != nil {
			return err
		}
		return fn(tx, o, d)
	})
	if web.IsNoRows(err) {
		s.NotFound(w, r)
		return nil, nil, nil, false
	}
	return o, d, err, true
}

func (s *API) emailDomainPage(w http.ResponseWriter, r *http.Request) {
	o, d, err, ok := s.withEmailDomain(w, r, "viewer", func(pgx.Tx, *web.OrgInfo, *web.SendDomain) error { return nil })
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	var zone string
	_ = db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT coalesce((SELECT z.domain FROM dns_records x JOIN dns_zones z ON z.id = x.zone_id WHERE x.managed_by = 'email:' || $1 LIMIT 1), '')`, d.ID).Scan(&zone)
	})
	s.Render(w, r, "email_domain", http.StatusOK, web.Data{"Org": o, "Domain": d, "Records": d.Records(s.SpfMechanism()), "CanEdit": model.RoleAtLeast(o.Role, "developer"), "Zone": zone})
}

func (s *API) checkEmailDomain(w http.ResponseWriter, r *http.Request) {
	o, d, err, ok := s.withEmailDomain(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, d *web.SendDomain) error {
		if _, err := tx.Exec(r.Context(), `UPDATE email_domains SET checked_at = NULL WHERE id = $1`, d.ID); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'email')`)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.ServerError(w, r, err)
		return
	}
	web.Back(w, r, o.Path()+"/email/d/"+d.Domain, "Checking the DNS records now; reload in a few seconds.", "")
}

func (s *API) deleteEmailDomain(w http.ResponseWriter, r *http.Request) {
	o, d, err, ok := s.withEmailDomain(w, r, "developer", func(tx pgx.Tx, o *web.OrgInfo, d *web.SendDomain) error {
		if r.PostFormValue("confirm") != d.Domain {
			return web.APIErr{Status: http.StatusUnprocessableEntity, Msg: "type the domain to delete it"}
		}
		return s.deleteEmailDomainTx(r, tx, o, d)
	})
	if !ok {
		return
	}
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/email/d/"+d.Domain, "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, o.Path()+"/email", "Domain "+d.Domain+" deleted.", "")
	}
}

func (s *API) sendEmailConsole(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	m := sendRequest{From: r.PostFormValue("from"), To: []string{strings.TrimSpace(r.PostFormValue("to"))}, Subject: r.PostFormValue("subject"), Text: r.PostFormValue("text")}
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		_, err := s.queueEmailTx(r, tx, o, m)
		return err
	})
	var ae web.APIErr
	switch {
	case errors.As(err, &ae):
		web.Back(w, r, o.Path()+"/email#send", "", ae.Msg)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		web.Back(w, r, o.Path()+"/email#log", "Email queued.", "")
	}
}

// emailAttach gives a service a token that only sends email, as EMAIL_API_URL and EMAIL_TOKEN secrets.
func (s *API) emailAttach(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "developer")
	if !ok {
		return
	}
	target := r.PostFormValue("service")
	var formErr string
	err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		ps, ss, _ := strings.Cut(target, "/")
		var serviceID string
		if err := tx.QueryRow(r.Context(), `SELECT s.id FROM services s JOIN projects p ON p.id = s.project_id WHERE p.slug = $1 AND s.slug = $2 AND s.preview_of IS NULL`,
			ps, ss).Scan(&serviceID); err != nil {
			formErr = "Choose a service."
			return nil
		}
		token := web.APITokenPrefix + secure.NewToken()
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO api_tokens (org_id, name, token_hash, role, created_by, email_only) VALUES ($1, $2, $3, 'developer', $4, true)`,
			o.ID, web.TruncateStr("email for "+target, 60), secure.HashToken(token), web.NullableUser(r)); err != nil {
			return err
		}
		vars := map[string]string{"EMAIL_API_URL": s.Cfg.BaseURL + "/api/v1/email/send", "EMAIL_TOKEN": token}
		if s.Cfg.SMTPSubmissionListen != "" { // for software that only speaks SMTP
			vars["SMTP_HOST"], vars["SMTP_PORT"], vars["SMTP_USERNAME"], vars["SMTP_PASSWORD"] = s.Cfg.MailHost(), "465", "token", token
		}
		for name, value := range vars {
			enc, err := s.Box.Seal([]byte(value), "secret:"+serviceID+":"+name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO secrets (org_id, service_id, name, value_enc, source) VALUES ($1, $2, $3, $4, 'email')
				ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, source = EXCLUDED.source, created_at = now()`, o.ID, serviceID, name, enc); err != nil {
				return err
			}
		}
		s.Audit(r, tx, o.ID, "email.attached", "service:"+target, nil)
		return nil
	})
	switch {
	case formErr != "":
		web.Back(w, r, o.Path()+"/email#connect", "", formErr)
	case err != nil:
		s.ServerError(w, r, err)
	default:
		names := "EMAIL_API_URL and EMAIL_TOKEN"
		if s.Cfg.SMTPSubmissionListen != "" {
			names = "EMAIL_API_URL, EMAIL_TOKEN and SMTP_HOST, SMTP_PORT, SMTP_USERNAME, SMTP_PASSWORD"
		}
		web.Back(w, r, o.Path()+"/email#connect", "Secrets "+names+" added to "+target+". Redeploy it to apply them.", "")
	}
}

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/mail"
	"platform/internal/model"
	"platform/internal/secure"
)

// Outgoing email (SES-like): verified sending domains with DKIM, a send API and a
// sending log. The agent delivers through the platform's SMTP relay.

var emailDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

const maxEmailBytes = 256 << 10

func (s *Server) emailEnabled() bool { return s.cfg.SMTPHost != "" }

type sendDomain struct {
	ID, Domain, Selector, Token, TXT, Status, Message string
	Checked, Verified                                 *time.Time
	Created                                           time.Time
}

type dnsRecord struct{ Name, Type, Value, Purpose string }

func (d *sendDomain) Records(spfInclude string) []dnsRecord {
	recs := []dnsRecord{
		{"_platform-verify." + d.Domain, "TXT", "platform-verify=" + d.Token, "proves you own the domain"},
		{d.Selector + "._domainkey." + d.Domain, "TXT", d.TXT, "DKIM key: receivers check the signature of each message"},
	}
	if spfInclude != "" {
		recs = append(recs, dnsRecord{d.Domain, "TXT", "v=spf1 include:" + spfInclude + " ~all", "SPF (recommended): merge with an existing v=spf1 record"})
	}
	return append(recs, dnsRecord{"_dmarc." + d.Domain, "TXT", "v=DMARC1; p=none", "DMARC (recommended)"})
}

const sendDomainCols = `id, domain, dkim_selector, verify_token, dkim_txt, status, message, checked_at, verified_at, created_at`

func scanSendDomain(row pgx.Row) (*sendDomain, error) {
	var d sendDomain
	err := row.Scan(&d.ID, &d.Domain, &d.Selector, &d.Token, &d.TXT, &d.Status, &d.Message, &d.Checked, &d.Verified, &d.Created)
	return &d, err
}

func loadSendDomains(ctx context.Context, tx pgx.Tx) ([]*sendDomain, error) {
	rows, err := tx.Query(ctx, `SELECT `+sendDomainCols+` FROM email_domains ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sendDomain
	for rows.Next() {
		d, err := scanSendDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func loadSendDomain(ctx context.Context, tx pgx.Tx, domain string) (*sendDomain, error) {
	return scanSendDomain(tx.QueryRow(ctx, `SELECT `+sendDomainCols+` FROM email_domains WHERE domain = $1`, domain))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) addEmailDomainTx(r *http.Request, tx pgx.Tx, o *orgInfo, domain string) error {
	if !s.emailEnabled() {
		return apiErr{http.StatusServiceUnavailable, "outgoing email is not configured on this platform"}
	}
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if len(domain) > 253 || !emailDomainRe.MatchString(domain) {
		return apiErr{http.StatusUnprocessableEntity, "enter a domain name such as example.com"}
	}
	var n, max int
	if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM email_domains), max_email_domains FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return err
	}
	if n >= max {
		return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d sending domains", max)}
	}
	priv, txt, err := mail.NewDKIMKey()
	if err != nil {
		return err
	}
	var id string
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO email_domains (org_id, domain, verify_token, dkim_selector, dkim_key_enc, dkim_txt) VALUES ($1, $2, $3, $4, '', $5) RETURNING id`,
		o.ID, domain, randomHex(16), "p"+randomHex(3), txt).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			return apiErr{http.StatusConflict, "this domain is already added"}
		}
		return err
	}
	enc, err := s.box.Seal(priv, "emaildkim:"+id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(), `UPDATE email_domains SET dkim_key_enc = $2 WHERE id = $1`, id, enc); err != nil {
		return err
	}
	if _, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'email')`); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "email.domain_added", "email-domain:"+domain, nil)
	return nil
}

func (s *Server) deleteEmailDomainTx(r *http.Request, tx pgx.Tx, o *orgInfo, d *sendDomain) error {
	if _, err := tx.Exec(r.Context(), `DELETE FROM email_domains WHERE id = $1`, d.ID); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "email.domain_deleted", "email-domain:"+d.Domain, nil)
	return nil
}

type sendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	ReplyTo string   `json:"reply_to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	HTML    string   `json:"html"`
}

// queueEmailTx checks a message and queues it for the agent; returns its id.
func (s *Server) queueEmailTx(r *http.Request, tx pgx.Tx, o *orgInfo, m sendRequest) (string, error) {
	bad := func(msg string) (string, error) { return "", apiErr{http.StatusUnprocessableEntity, msg} }
	if !s.emailEnabled() {
		return "", apiErr{http.StatusServiceUnavailable, "outgoing email is not configured on this platform"}
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
	size := len(m.Subject) + len(m.Text) + len(m.HTML)
	if size > maxEmailBytes {
		return bad("the message is larger than 256 KB")
	}
	var sent, max int
	if err := tx.QueryRow(r.Context(), `
		SELECT coalesce((SELECT sum(cardinality(to_addrs)) FROM email_messages WHERE created_at > now() - interval '1 day'), 0), max_emails_day
		FROM plans WHERE id = $1`, o.PlanID).Scan(&sent, &max); err != nil {
		return "", err
	}
	if sent+len(to) > max {
		return "", apiErr{http.StatusTooManyRequests, fmt.Sprintf("your plan sends at most %d emails a day", max)}
	}
	var id string
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO email_messages (org_id, domain_id, from_addr, to_addrs, reply_to, subject, body_text, body_html, size_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		o.ID, domainID, from, to, replyTo, m.Subject, m.Text, m.HTML, size).Scan(&id); err != nil {
		return "", err
	}
	_, err = tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'email')`)
	return id, err
}

type emailMessage struct {
	ID, From, Subject, Status, Error string
	To                               []string
	Created                          time.Time
	Sent                             *time.Time
}

func loadEmailMessages(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]emailMessage, error) {
	rows, err := tx.Query(ctx, `SELECT id, from_addr, to_addrs, subject, status, error, created_at, sent_at FROM email_messages `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []emailMessage{}
	for rows.Next() {
		var m emailMessage
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Subject, &m.Status, &m.Error, &m.Created, &m.Sent); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- REST API ----------

type apiEmailDomain struct {
	Domain   string      `json:"domain"`
	Status   string      `json:"status"`
	Message  string      `json:"message,omitempty"`
	Records  []dnsRecord `json:"dns_records"`
	Verified *time.Time  `json:"verified_at"`
}

func (s *Server) sendDomainView(d *sendDomain) apiEmailDomain {
	return apiEmailDomain{d.Domain, d.Status, d.Message, d.Records(s.cfg.EmailSPFInclude), d.Verified}
}

func (s *Server) apiEmailDomains(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	out := []apiEmailDomain{}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		ds, err := loadSendDomains(r.Context(), tx)
		for _, d := range ds {
			out = append(out, s.sendDomainView(d))
		}
		return err
	})
	if apiTx(w, s, err) {
		writeJSON(w, out)
	}
}

func (s *Server) apiAddEmailDomain(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var b struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"domain": "example.com"}`)
		return
	}
	var view apiEmailDomain
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if err := s.addEmailDomainTx(r, tx, o, b.Domain); err != nil {
			return err
		}
		d, err := loadSendDomain(r.Context(), tx, strings.TrimSuffix(strings.ToLower(strings.TrimSpace(b.Domain)), "."))
		if err == nil {
			view = s.sendDomainView(d)
		}
		return err
	})
	if apiTx(w, s, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(view)
	}
}

func (s *Server) apiDeleteEmailDomain(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		d, err := loadSendDomain(r.Context(), tx, r.PathValue("domain"))
		if err != nil {
			return err
		}
		return s.deleteEmailDomainTx(r, tx, o, d)
	})
	if apiTx(w, s, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) apiSendEmail(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var m sendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxEmailBytes+16<<10)).Decode(&m); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON: {"from": "...", "to": ["..."], "subject": "...", "text": "..."}`)
		return
	}
	var id string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		id, err = s.queueEmailTx(r, tx, o, m)
		return err
	})
	if apiTx(w, s, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "queued"})
	}
}

func (s *Server) apiEmailMessage(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var ms []emailMessage
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		ms, err = loadEmailMessages(r.Context(), tx, `WHERE id::text = $1`, r.PathValue("id"))
		return err
	})
	if !apiTx(w, s, err) {
		return
	}
	if len(ms) == 0 {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	m := ms[0]
	writeJSON(w, map[string]any{"id": m.ID, "from": m.From, "to": m.To, "subject": m.Subject, "status": m.Status, "error": m.Error, "created_at": m.Created, "sent_at": m.Sent})
}

// ---------- console ----------

func (s *Server) emailPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	var ds []*sendDomain
	var msgs []emailMessage
	var services []string
	var sent, max int
	if err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if ds, err = loadSendDomains(r.Context(), tx); err != nil {
			return err
		}
		if msgs, err = loadEmailMessages(r.Context(), tx, `ORDER BY created_at DESC LIMIT 100`); err != nil {
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
		s.serverError(w, r, err)
		return
	}
	var verified []string
	for _, d := range ds {
		if d.Status == "verified" {
			verified = append(verified, d.Domain)
		}
	}
	s.render(w, r, "email", http.StatusOK, data{"Org": o, "Domains": ds, "Verified": verified, "Messages": msgs, "Services": services,
		"SentToday": sent, "MaxDay": max, "Enabled": s.emailEnabled(), "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *Server) addEmailDomain(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.PostFormValue("domain"))), ".")
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error { return s.addEmailDomainTx(r, tx, o, domain) })
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/email#domains", "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/email/d/"+domain, "Domain added. Create the DNS records below; verification runs every minute.", "")
	}
}

func (s *Server) withEmailDomain(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *orgInfo, d *sendDomain) error) (*orgInfo, *sendDomain, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var d *sendDomain
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if d, err = loadSendDomain(r.Context(), tx, r.PathValue("domain")); err != nil {
			return err
		}
		return fn(tx, o, d)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, false
	}
	return o, d, err, true
}

func (s *Server) emailDomainPage(w http.ResponseWriter, r *http.Request) {
	o, d, err, ok := s.withEmailDomain(w, r, "viewer", func(pgx.Tx, *orgInfo, *sendDomain) error { return nil })
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "email_domain", http.StatusOK, data{"Org": o, "Domain": d, "Records": d.Records(s.cfg.EmailSPFInclude), "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *Server) checkEmailDomain(w http.ResponseWriter, r *http.Request) {
	o, d, err, ok := s.withEmailDomain(w, r, "developer", func(tx pgx.Tx, o *orgInfo, d *sendDomain) error {
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
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/email/d/"+d.Domain, "Checking the DNS records now; reload in a few seconds.", "")
}

func (s *Server) deleteEmailDomain(w http.ResponseWriter, r *http.Request) {
	o, d, err, ok := s.withEmailDomain(w, r, "developer", func(tx pgx.Tx, o *orgInfo, d *sendDomain) error {
		if r.PostFormValue("confirm") != d.Domain {
			return apiErr{http.StatusUnprocessableEntity, "type the domain to delete it"}
		}
		return s.deleteEmailDomainTx(r, tx, o, d)
	})
	if !ok {
		return
	}
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/email/d/"+d.Domain, "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/email", "Domain "+d.Domain+" deleted.", "")
	}
}

func (s *Server) sendEmailConsole(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	m := sendRequest{From: r.PostFormValue("from"), To: []string{strings.TrimSpace(r.PostFormValue("to"))}, Subject: r.PostFormValue("subject"), Text: r.PostFormValue("text")}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		_, err := s.queueEmailTx(r, tx, o, m)
		return err
	})
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/email#send", "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/email#log", "Email queued.", "")
	}
}

// emailAttach gives a service a token that only sends email, as EMAIL_API_URL and EMAIL_TOKEN secrets.
func (s *Server) emailAttach(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	target := r.PostFormValue("service")
	var formErr string
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		ps, ss, _ := strings.Cut(target, "/")
		var serviceID string
		if err := tx.QueryRow(r.Context(), `SELECT s.id FROM services s JOIN projects p ON p.id = s.project_id WHERE p.slug = $1 AND s.slug = $2 AND s.preview_of IS NULL`,
			ps, ss).Scan(&serviceID); err != nil {
			formErr = "Choose a service."
			return nil
		}
		token := apiTokenPrefix + secure.NewToken()
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO api_tokens (org_id, name, token_hash, role, created_by, email_only) VALUES ($1, $2, $3, 'developer', $4, true)`,
			o.ID, truncateStr("email for "+target, 60), secure.HashToken(token), nullableUser(r)); err != nil {
			return err
		}
		vars := map[string]string{"EMAIL_API_URL": s.cfg.BaseURL + "/api/v1/email/send", "EMAIL_TOKEN": token}
		for name, value := range vars {
			enc, err := s.box.Seal([]byte(value), "secret:"+serviceID+":"+name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO secrets (org_id, service_id, name, value_enc) VALUES ($1, $2, $3, $4)
				ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, created_at = now()`, o.ID, serviceID, name, enc); err != nil {
				return err
			}
		}
		s.audit(r, tx, o.ID, "email.attached", "service:"+target, nil)
		return nil
	})
	switch {
	case formErr != "":
		back(w, r, o.Path()+"/email#connect", "", formErr)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/email#connect", "Secrets EMAIL_API_URL and EMAIL_TOKEN added to "+target+". Redeploy it to apply them.", "")
	}
}

package web

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Server) EmailEnabled() bool { return s.Cfg.EmailMode() != "off" }

// spfMechanism is what tenants add to their domain's SPF record: the relay's include, or
// this server's address when it delivers directly.
func (s *Server) SpfMechanism() string {
	switch {
	case s.Cfg.EmailSPFInclude != "":
		return "include:" + s.Cfg.EmailSPFInclude
	case s.Cfg.EmailMode() == "direct" && strings.Contains(s.Cfg.PublicIP, ":"):
		return "ip6:" + s.Cfg.PublicIP
	case s.Cfg.EmailMode() == "direct" && s.Cfg.PublicIP != "":
		return "ip4:" + s.Cfg.PublicIP
	}
	return ""
}

type SendDomain struct {
	ID, Domain, Selector, Token, TXT, Status, Message string
	Checked, Verified                                 *time.Time
	Created                                           time.Time
}

type DNSRecord struct{ Name, Type, Value, Purpose string }

func (d *SendDomain) Records(spf string) []DNSRecord {
	recs := []DNSRecord{
		{"_platform-verify." + d.Domain, "TXT", "platform-verify=" + d.Token, "proves you own the domain"},
		{d.Selector + "._domainkey." + d.Domain, "TXT", d.TXT, "DKIM key: receivers check the signature of each message"},
	}
	if spf != "" {
		recs = append(recs, DNSRecord{d.Domain, "TXT", "v=spf1 " + spf + " ~all", "SPF (recommended): merge with an existing v=spf1 record"})
	}
	return append(recs, DNSRecord{"_dmarc." + d.Domain, "TXT", "v=DMARC1; p=none", "DMARC (recommended)"})
}

const SendDomainCols = `id, domain, dkim_selector, verify_token, dkim_txt, status, message, checked_at, verified_at, created_at`

func ScanSendDomain(row pgx.Row) (*SendDomain, error) {
	var d SendDomain
	err := row.Scan(&d.ID, &d.Domain, &d.Selector, &d.Token, &d.TXT, &d.Status, &d.Message, &d.Checked, &d.Verified, &d.Created)
	return &d, err
}

func LoadSendDomains(ctx context.Context, tx pgx.Tx) ([]*SendDomain, error) {
	rows, err := tx.Query(ctx, `SELECT `+SendDomainCols+` FROM email_domains ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SendDomain
	for rows.Next() {
		d, err := ScanSendDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

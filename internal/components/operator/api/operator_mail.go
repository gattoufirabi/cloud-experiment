package api

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
	"platform/internal/lib/stalwart"
)

// The admin panel's Mail and DNS pages. Mail: the mail server's state, deliverability
// checks, the outgoing queue size, banned addresses (with unban) and every organization's
// mail domains (counts only). DNS: every hosted zone and custom domain with its status.

type mailCheck struct {
	Name, Detail string
	OK           bool
}

func (s *API) operatorMailPage(w http.ResponseWriter, r *http.Request) {
	type md struct {
		Org, Domain string
		Active      bool
		Mailboxes   int
		QuotaMB     int64
		Created     time.Time
	}
	var domains []md
	if rows, err := s.Pool.Query(r.Context(), `SELECT * FROM operator_mail_domains()`); err == nil {
		domains, _ = pgx.CollectRows(rows, pgx.RowToStructByPos[md])
	}
	d := web.Data{"OpSection": "mail", "Enabled": s.MailHostingEnabled(), "Domains": domains, "MailHost": s.Cfg.MailHost(), "PublicIP": s.Cfg.PublicIP,
		"Blocked": []stalwart.BlockedIP{}}
	if s.MailHostingEnabled() {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		ms := s.MailServer()
		var checks []mailCheck
		if err := ms.Ping(ctx); err != nil {
			checks = append(checks, mailCheck{"Mail server answers", err.Error(), false})
		} else {
			checks = append(checks, mailCheck{"Mail server answers", "its admin API responds", true})
			if n, err := ms.QueueSize(ctx); err == nil {
				d["Queue"] = n
			}
			if p, err := ms.QueueProblems(ctx); err == nil && len(p) > 0 {
				d["QueueProblems"] = p
			}
			if ips, err := ms.BlockedIPs(ctx); err == nil {
				d["Blocked"] = ips
			}
		}
		checks = append(checks, s.deliverabilityChecks(ctx)...)
		d["Checks"] = checks
	}
	s.Render(w, r, "operator_mail", http.StatusOK, d)
}

// deliverabilityChecks: the mail host resolves to the server and the server's reverse DNS
// names the mail host (receiving servers check both).
func (s *API) deliverabilityChecks(ctx context.Context) []mailCheck {
	host, ip := strings.ToLower(s.Cfg.MailHost()), s.Cfg.PublicIP
	var out []mailCheck
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	switch {
	case err != nil:
		out = append(out, mailCheck{"Mail host resolves", host + ": " + err.Error(), false})
	case ip != "" && !web.Contains(addrs, ip):
		out = append(out, mailCheck{"Mail host resolves", host + " → " + strings.Join(addrs, ", ") + ", not " + ip, false})
	default:
		out = append(out, mailCheck{"Mail host resolves", host + " → " + strings.Join(addrs, ", "), true})
	}
	if ip == "" {
		return append(out, mailCheck{"Reverse DNS", "PUBLIC_IP is not set: cannot check", false})
	}
	names, err := net.DefaultResolver.LookupAddr(ctx, ip)
	ok := false
	for _, n := range names {
		ok = ok || strings.EqualFold(strings.TrimSuffix(n, "."), host)
	}
	detail := ip + " → " + strings.Join(names, ", ")
	if err != nil {
		detail = ip + ": " + err.Error()
	}
	if !ok {
		detail += " (set it to " + host + " at the hosting provider)"
	}
	return append(out, mailCheck{"Reverse DNS (PTR)", detail, ok})
}

func (s *API) operatorUnban(w http.ResponseWriter, r *http.Request) {
	id, addr := r.PostFormValue("id"), r.PostFormValue("address")
	if id == "" || !s.MailHostingEnabled() {
		web.Back(w, r, "/operator/mail#blocked", "", "Nothing to unban.")
		return
	}
	if err := s.MailServer().Unban(r.Context(), id); err != nil && !stalwart.IsType(err, "notFound") {
		web.Back(w, r, "/operator/mail#blocked", "", "The mail server refused: "+err.Error())
		return
	}
	_ = db.Audit(r.Context(), s.Pool, "", web.SessionFrom(r).UserID, "operator.mail_unban", "ip:"+addr, nil, web.ClientIP(r))
	web.Back(w, r, "/operator/mail#blocked", addr+" can connect to the mail server again.", "")
}

func (s *API) operatorDNSPage(w http.ResponseWriter, r *http.Request) {
	type zone struct {
		Org, Domain, Status, Delegation string
		Records                         int
		Active                          *time.Time
	}
	type dom struct {
		Org, Hostname, Service string
		Verified               bool
		Created                time.Time
	}
	var zones []zone
	var doms []dom
	if rows, err := s.Pool.Query(r.Context(), `SELECT * FROM operator_dns_zones()`); err == nil {
		zones, _ = pgx.CollectRows(rows, pgx.RowToStructByPos[zone])
	}
	if rows, err := s.Pool.Query(r.Context(), `SELECT * FROM operator_custom_domains()`); err == nil {
		doms, _ = pgx.CollectRows(rows, pgx.RowToStructByPos[dom])
	}
	s.Render(w, r, "operator_dns", http.StatusOK, web.Data{"OpSection": "dns", "Zones": zones, "Domains": doms,
		"Nameservers": s.Cfg.DNSNameservers, "Enabled": s.DNSEnabled()})
}

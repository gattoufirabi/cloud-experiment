package api

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Traffic protection per service: WAF (OWASP Core Rule Set), a request limit per visitor
// IP and a list of blocked addresses. The API only stores the settings; the agent turns
// them into proxy configuration.

type protection struct {
	WAF       bool
	RPM       *int
	Inflight  int // requests in progress per instance before 503; 0 = no limit
	Blocked   []string
	BlockedTx string // textarea content
}

func loadProtection(r *http.Request, tx pgx.Tx, serviceID string) (protection, error) {
	var p protection
	err := tx.QueryRow(r.Context(), `SELECT waf_enabled, rate_limit_rpm, max_inflight, blocked_ips::text[] FROM services WHERE id = $1`, serviceID).
		Scan(&p.WAF, &p.RPM, &p.Inflight, &p.Blocked)
	p.BlockedTx = strings.Join(p.Blocked, "\n")
	return p, err
}

const maxBlocked = 100

// parseBlocked accepts IPv4/IPv6 addresses or CIDR ranges, one per line or comma separated.
func parseBlocked(in string) ([]string, string) {
	seen := map[string]bool{}
	out := []string{}
	for _, f := range strings.FieldsFunc(in, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' || r == '\r' || r == '\t' }) {
		var pfx netip.Prefix
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, "Not an address or range: " + f
			}
			pfx = p.Masked()
		} else {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, "Not an address or range: " + f
			}
			pfx = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		if s := pfx.String(); !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) > maxBlocked {
		return nil, "At most " + strconv.Itoa(maxBlocked) + " blocked addresses or ranges."
	}
	return out, ""
}

func (s *Server) protectionSettings(w http.ResponseWriter, r *http.Request) {
	waf := r.PostFormValue("waf") == "1"
	var rpm *int
	if v := strings.TrimSpace(r.PostFormValue("rpm")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 100000 {
			back(w, r, r.URL.Path[:strings.LastIndex(r.URL.Path, "/")]+"#security", "", "The request limit must be between 10 and 100000 per minute, or empty for no limit.")
			return
		}
		rpm = &n
	}
	inflight := 0
	if v := strings.TrimSpace(r.PostFormValue("inflight")); v != "" && v != "0" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100000 {
			back(w, r, r.URL.Path[:strings.LastIndex(r.URL.Path, "/")]+"#security", "", "Requests in progress per instance: from 1 to 100000, or empty for no limit.")
			return
		}
		inflight = n
	}
	blocked, msg := parseBlocked(r.PostFormValue("blocked"))
	if msg != "" {
		back(w, r, r.URL.Path[:strings.LastIndex(r.URL.Path, "/")]+"#security", "", msg)
		return
	}
	o, p, sv, err, ok := s.withService(w, r, "developer", func(tx pgx.Tx, o *orgInfo, p *projectInfo, sv *serviceInfo) error {
		if _, err := tx.Exec(r.Context(), `UPDATE services SET waf_enabled = $2, rate_limit_rpm = $3, blocked_ips = $4::cidr[], max_inflight = $5 WHERE id = $1`,
			sv.ID, waf, rpm, blocked, inflight); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "service.protection", "service:"+sv.Slug, map[string]any{"waf": waf, "rate_limit_rpm": rpm, "blocked": len(blocked), "max_inflight": inflight})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', '')`) // agent refreshes the proxy
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, svcPath(o, p, sv)+"#security", "Traffic protection saved; it applies within a few seconds.", "")
}

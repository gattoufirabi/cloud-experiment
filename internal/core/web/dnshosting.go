package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/miekg/dns"
)

// DNS hosting: zones delegated to the platform's name servers and their records. The
// agent answers for active zones; a zone becomes active when its delegation is seen.

var (
	DNSDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)
	dnsNameRe   = regexp.MustCompile(`^(\*\.)?([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)*[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?$`)
	dnsHostRe   = regexp.MustCompile(`^([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{1,63}\.?$`)
	DNSTypes    = []string{"A", "AAAA", "CNAME", "TXT", "MX", "CAA", "NS", "SRV"}
)

type DNSZoneView struct {
	ID, Domain, Status string
	Checked, Active    *time.Time
	Records            int
	Created            time.Time
	Delegation         string // what is wrong with the delegation ("" when it is complete)
}

func (s *Server) DNSEnabled() bool { return len(s.Cfg.DNSNameservers) > 0 }

type DNSRecordInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Priority int    `json:"priority"`
	TTL      int    `json:"ttl"`
}

// normalizeRecord checks a record and returns its stored form.
func NormalizeRecord(zone string, in DNSRecordInput) (DNSRecordInput, error) {
	bad := func(msg string) (DNSRecordInput, error) { return in, APIErr{http.StatusUnprocessableEntity, msg} }
	in.Name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(in.Name)), ".")
	in.Name = strings.TrimSuffix(in.Name, "."+zone)
	if in.Name == "" || in.Name == zone {
		in.Name = "@"
	}
	if in.Name != "@" && in.Name != "*" && !dnsNameRe.MatchString(in.Name) {
		return bad("name: @ for the domain itself, or a name such as www or api.v2")
	}
	in.Type = strings.ToUpper(strings.TrimSpace(in.Type))
	in.Value = strings.TrimSpace(in.Value)
	if in.TTL == 0 {
		in.TTL = 300
	}
	if in.TTL < 60 || in.TTL > 86400 {
		return bad("TTL: 60 to 86400 seconds")
	}
	switch in.Type {
	case "A":
		if ip := net.ParseIP(in.Value); ip == nil || ip.To4() == nil {
			return bad("A: an IPv4 address such as 203.0.113.10")
		}
	case "AAAA":
		if ip := net.ParseIP(in.Value); ip == nil || ip.To4() != nil {
			return bad("AAAA: an IPv6 address such as 2001:db8::1")
		}
	case "CNAME", "NS", "MX":
		in.Value = strings.TrimSuffix(strings.ToLower(in.Value), ".")
		if !dnsHostRe.MatchString(in.Value + ".") {
			return bad(in.Type + ": a host name such as target.example.com")
		}
		if in.Type == "CNAME" && in.Name == "@" {
			return bad("CNAME: not allowed for the domain itself (@); use an A record, or the service shortcut")
		}
		if in.Type == "NS" && in.Name == "@" {
			return bad("NS: the domain's own name servers are the platform's; NS records delegate a sub-domain")
		}
		if in.Type == "MX" && (in.Priority < 0 || in.Priority > 65535) {
			return bad("MX: a priority from 0 to 65535")
		}
	case "TXT":
		if len(in.Value) > 1000 {
			return bad("TXT: at most 1000 characters")
		}
		in.Value = strings.Trim(in.Value, `"`)
	case "CAA":
		if _, err := dns.NewRR("x.test. 300 IN CAA " + in.Value); err != nil {
			return bad(`CAA: such as 0 issue "letsencrypt.org"`)
		}
	case "SRV": // value "weight port target"; the priority is the SRV priority
		f := strings.Fields(in.Value)
		if len(f) != 3 || in.Priority < 0 || in.Priority > 65535 {
			return bad("SRV: a value such as \"0 443 target.example.com\" (weight, port, target) and a priority from 0 to 65535")
		}
		w, werr := strconv.Atoi(f[0])
		port, perr := strconv.Atoi(f[1])
		target := strings.TrimSuffix(strings.ToLower(f[2]), ".")
		if werr != nil || perr != nil || w < 0 || w > 65535 || port < 0 || port > 65535 || (target != "" && !dnsHostRe.MatchString(target+".")) {
			return bad("SRV: a value such as \"0 443 target.example.com\" (weight, port, target)")
		}
		in.Value = fmt.Sprintf("%d %d %s", w, port, target)
	default:
		return bad("type: one of " + strings.Join(DNSTypes, ", "))
	}
	return in, nil
}

func (s *Server) ZoneChanged(ctx context.Context, r *http.Request, tx pgx.Tx, o *OrgInfo, z *DNSZoneView, action string, detail any) error {
	if _, err := tx.Exec(ctx, `UPDATE dns_zones SET serial = greatest(serial + 1, extract(epoch FROM now())::bigint) WHERE id = $1`, z.ID); err != nil {
		return err
	}
	s.Audit(r, tx, o.ID, action, "zone:"+z.Domain, detail)
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'dns')`)
	return err
}

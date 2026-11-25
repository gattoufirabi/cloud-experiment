package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/miekg/dns"
)

// Importing a zone file (the BIND format registrars and DNS providers export): before
// moving a domain's name servers to the platform, its existing records (websites, mail,
// verification records) are copied into the hosted zone, so nothing stops answering.
// The zone's SOA and its own NS records are the platform's and are left out.

const MaxZoneFile = 256 << 10

// parseZoneFile turns a zone file into records of zone; skipped explains what is left out.
func ParseZoneFile(zone, text string) (recs []DNSRecordInput, skipped []string, err error) {
	if len(text) > MaxZoneFile {
		return nil, nil, APIErr{http.StatusRequestEntityTooLarge, "the zone file is larger than 256 KB"}
	}
	zp := dns.NewZoneParser(strings.NewReader(text), dns.Fqdn(zone), "")
	zp.SetDefaultTTL(3600)
	zp.SetIncludeAllowed(false)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		h := rr.Header()
		name := strings.TrimSuffix(strings.ToLower(h.Name), ".")
		if name != zone && !strings.HasSuffix(name, "."+zone) {
			skipped = append(skipped, name+" (outside "+zone+")")
			continue
		}
		rel := "@"
		if name != zone {
			rel = strings.TrimSuffix(name, "."+zone)
		}
		ttl := int(h.Ttl)
		ttl = max(60, min(ttl, 86400))
		in := DNSRecordInput{Name: rel, TTL: ttl}
		switch v := rr.(type) {
		case *dns.SOA:
			continue // the platform's own
		case *dns.NS:
			if rel == "@" {
				continue // the domain's name servers are the platform's
			}
			in.Type, in.Value = "NS", v.Ns
		case *dns.A:
			in.Type, in.Value = "A", v.A.String()
		case *dns.AAAA:
			in.Type, in.Value = "AAAA", v.AAAA.String()
		case *dns.CNAME:
			in.Type, in.Value = "CNAME", v.Target
		case *dns.MX:
			in.Type, in.Value, in.Priority = "MX", v.Mx, int(v.Preference)
		case *dns.TXT:
			in.Type, in.Value = "TXT", strings.Join(v.Txt, "")
		case *dns.CAA:
			in.Type, in.Value = "CAA", fmt.Sprintf("%d %s %q", v.Flag, v.Tag, v.Value)
		case *dns.SRV:
			in.Type, in.Value, in.Priority = "SRV", fmt.Sprintf("%d %d %s", v.Weight, v.Port, v.Target), int(v.Priority)
		default:
			skipped = append(skipped, rel+" "+dns.TypeToString[h.Rrtype]+" (type not supported)")
			continue
		}
		recs = append(recs, in)
	}
	if err := zp.Err(); err != nil {
		return nil, nil, APIErr{http.StatusUnprocessableEntity, "the zone file does not parse: " + err.Error()}
	}
	return recs, skipped, nil
}

package agent

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Authoritative DNS for the organizations' hosted zones (primary node, DNS_LISTEN).
// Active zones and their records are kept in memory, reloaded when they change and
// every 30 seconds. SOA and NS records come from the platform's name servers; names
// in no hosted zone are refused (this is not a resolver). Pending zones are activated
// when their delegation to the platform's name servers is seen, and dropped after 14
// days without it.

type dnsZone struct {
	name    string // fqdn, lower case
	serial  uint32
	records map[string][]dns.RR // fqdn -> records
	cuts    map[string]bool     // names delegated elsewhere (NS records below the apex)
}

type dnsData struct{ zones map[string]*dnsZone }

func (a *Agent) dnsNameservers() []string {
	var out []string
	for _, n := range a.cfg.DNSNameservers {
		out = append(out, dns.Fqdn(strings.ToLower(n)))
	}
	return out
}

// buildRR makes the record of one row; nil when the value does not parse.
func buildRR(fqdn, typ, value string, priority, ttl int) dns.RR {
	hdr := dns.RR_Header{Name: fqdn, Class: dns.ClassINET, Ttl: uint32(ttl)}
	switch typ {
	case "A":
		ip := net.ParseIP(value).To4()
		if ip == nil {
			return nil
		}
		hdr.Rrtype = dns.TypeA
		return &dns.A{Hdr: hdr, A: ip}
	case "AAAA":
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() != nil {
			return nil
		}
		hdr.Rrtype = dns.TypeAAAA
		return &dns.AAAA{Hdr: hdr, AAAA: ip}
	case "CNAME":
		hdr.Rrtype = dns.TypeCNAME
		return &dns.CNAME{Hdr: hdr, Target: dns.Fqdn(strings.ToLower(value))}
	case "NS":
		hdr.Rrtype = dns.TypeNS
		return &dns.NS{Hdr: hdr, Ns: dns.Fqdn(strings.ToLower(value))}
	case "MX":
		hdr.Rrtype = dns.TypeMX
		return &dns.MX{Hdr: hdr, Preference: uint16(priority), Mx: dns.Fqdn(strings.ToLower(value))}
	case "TXT":
		hdr.Rrtype = dns.TypeTXT
		var chunks []string
		for len(value) > 255 {
			chunks, value = append(chunks, value[:255]), value[255:]
		}
		return &dns.TXT{Hdr: hdr, Txt: append(chunks, value)}
	case "CAA":
		rr, err := dns.NewRR(fqdn + " " + fmt.Sprint(ttl) + " IN CAA " + value)
		if err != nil {
			return nil
		}
		return rr
	}
	return nil
}

func recordFqdn(name, zone string) string {
	if name == "@" {
		return zone
	}
	return dns.Fqdn(strings.ToLower(name) + "." + strings.TrimSuffix(zone, "."))
}

func (a *Agent) loadDNS(ctx context.Context) {
	rows, err := a.pool.Query(ctx, `
		SELECT z.domain, z.serial, coalesce(r.name, ''), coalesce(r.type, ''), coalesce(r.value, ''), coalesce(r.priority, 0), coalesce(r.ttl, 300)
		FROM dns_zones z JOIN orgs o ON o.id = z.org_id LEFT JOIN dns_records r ON r.zone_id = z.id
		WHERE z.status = 'active' AND NOT o.suspended`)
	if err != nil {
		a.log.Warn("dns zones", "err", err)
		return
	}
	defer rows.Close()
	data := &dnsData{zones: map[string]*dnsZone{}}
	for rows.Next() {
		var domain, name, typ, value string
		var serial int64
		var prio, ttl int
		if rows.Scan(&domain, &serial, &name, &typ, &value, &prio, &ttl) != nil {
			continue
		}
		zn := dns.Fqdn(domain)
		z := data.zones[zn]
		if z == nil {
			z = &dnsZone{name: zn, serial: uint32(serial), records: map[string][]dns.RR{}, cuts: map[string]bool{}}
			data.zones[zn] = z
		}
		if typ == "" {
			continue
		}
		fq := recordFqdn(name, zn)
		if rr := buildRR(fq, typ, value, prio, ttl); rr != nil {
			z.records[fq] = append(z.records[fq], rr)
			if typ == "NS" && fq != zn {
				z.cuts[fq] = true
			}
		}
	}
	a.dnsData.Store(data)
}

func (a *Agent) pokeDNS() {
	select {
	case a.dnsWake <- struct{}{}:
	default:
	}
}

func (a *Agent) dnsLoop(ctx context.Context) {
	if !a.isPrimary() || a.cfg.DNSListen == "" || len(a.cfg.DNSNameservers) == 0 {
		return
	}
	a.loadDNS(ctx)
	mux := dns.NewServeMux()
	mux.HandleFunc(".", a.serveDNS)
	for _, netw := range []string{"udp", "tcp"} {
		srv := &dns.Server{Addr: a.cfg.DNSListen, Net: netw, Handler: mux, UDPSize: 1232}
		go func() {
			if err := srv.ListenAndServe(); err != nil {
				a.log.Error("dns server", "net", srv.Net, "err", err)
			}
		}()
		go func() { <-ctx.Done(); _ = srv.Shutdown() }()
	}
	a.log.Info("dns server started", "listen", a.cfg.DNSListen)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var lastCheck time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.dnsWake:
		}
		if time.Since(lastCheck) > 2*time.Minute {
			lastCheck = time.Now()
			a.checkDelegations(ctx)
		}
		a.loadDNS(ctx)
	}
}

// checkDelegations activates pending zones delegated to the platform's name servers.
func (a *Agent) checkDelegations(ctx context.Context) {
	_, _ = a.pool.Exec(ctx, `DELETE FROM dns_zones WHERE status = 'pending' AND created_at < now() - interval '14 days'`)
	rows, err := a.pool.Query(ctx, `SELECT id, org_id, domain FROM dns_zones WHERE status = 'pending' ORDER BY checked_at NULLS FIRST LIMIT 50`)
	if err != nil {
		return
	}
	type pz struct{ ID, OrgID, Domain string }
	var list []pz
	for rows.Next() {
		var z pz
		if rows.Scan(&z.ID, &z.OrgID, &z.Domain) == nil {
			list = append(list, z)
		}
	}
	rows.Close()
	ours := map[string]bool{}
	for _, n := range a.dnsNameservers() {
		ours[n] = true
	}
	for _, z := range list {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		nss, err := net.DefaultResolver.LookupNS(lctx, z.Domain)
		cancel()
		delegated := false
		for _, ns := range nss {
			if ours[dns.Fqdn(strings.ToLower(ns.Host))] {
				delegated = true
			}
		}
		if err == nil && delegated {
			_, _ = a.pool.Exec(ctx, `UPDATE dns_zones SET status = 'active', active_at = now(), checked_at = now() WHERE id = $1`, z.ID)
			_ = auditAgent(ctx, a, z.OrgID, "dns.zone_active", "zone:"+z.Domain, nil)
		} else {
			_, _ = a.pool.Exec(ctx, `UPDATE dns_zones SET checked_at = now() WHERE id = $1`, z.ID)
		}
	}
}

func (a *Agent) soa(z *dnsZone) dns.RR {
	ns := a.dnsNameservers()
	return &dns.SOA{Hdr: dns.RR_Header{Name: z.name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300},
		Ns: ns[0], Mbox: dns.Fqdn("hostmaster." + strings.TrimSuffix(z.name, ".")), Serial: z.serial,
		Refresh: 3600, Retry: 600, Expire: 604800, Minttl: 300}
}

func (a *Agent) apexNS(z *dnsZone) []dns.RR {
	var out []dns.RR
	for _, n := range a.dnsNameservers() {
		out = append(out, &dns.NS{Hdr: dns.RR_Header{Name: z.name, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 3600}, Ns: n})
	}
	return out
}

func (a *Agent) serveDNS(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Compress = true
	defer func() {
		if opt := req.IsEdns0(); opt != nil {
			m.SetEdns0(1232, false)
		}
		if w.RemoteAddr().Network() == "udp" {
			m.Truncate(1232)
		}
		_ = w.WriteMsg(m)
	}()
	if len(req.Question) != 1 || req.Opcode != dns.OpcodeQuery {
		m.Rcode = dns.RcodeNotImplemented
		return
	}
	q := req.Question[0]
	name := strings.ToLower(q.Name)
	data, _ := a.dnsData.Load().(*dnsData)
	var z *dnsZone
	if data != nil {
		for n := name; ; {
			if zz := data.zones[n]; zz != nil {
				z = zz
				break
			}
			i := strings.IndexByte(n, '.')
			if i < 0 || i == len(n)-1 {
				break
			}
			n = n[i+1:]
		}
	}
	if z == nil {
		m.Rcode = dns.RcodeRefused // not a hosted zone: this is not a resolver
		return
	}
	m.Authoritative = true
	// Below a delegation: refer to the delegated name servers.
	for n := name; n != z.name && strings.Contains(n, "."); n = n[strings.IndexByte(n, '.')+1:] {
		if z.cuts[n] {
			m.Authoritative = false
			for _, rr := range z.records[n] {
				if rr.Header().Rrtype == dns.TypeNS {
					m.Ns = append(m.Ns, rr)
				}
			}
			return
		}
	}
	if q.Qtype == dns.TypeANY { // RFC 8482: a small answer instead of everything
		m.Answer = append(m.Answer, &dns.HINFO{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeHINFO, Class: dns.ClassINET, Ttl: 3600}, Cpu: "RFC8482"})
		return
	}
	if name == z.name {
		switch q.Qtype {
		case dns.TypeSOA:
			m.Answer = append(m.Answer, a.soa(z))
			return
		case dns.TypeNS:
			m.Answer = append(m.Answer, a.apexNS(z)...)
			return
		}
	}
	rrs, exists := z.records[name]
	if !exists && name != z.name { // wildcard: *.<closest parent>
		for n := name; strings.Contains(n, "."); {
			n = n[strings.IndexByte(n, '.')+1:]
			if wc, ok := z.records["*."+n]; ok {
				for _, rr := range wc {
					c := dns.Copy(rr)
					c.Header().Name = q.Name
					rrs = append(rrs, c)
				}
				exists = true
				break
			}
			if n == z.name || !strings.HasSuffix(n, z.name) {
				break
			}
		}
	}
	for _, rr := range rrs {
		if rr.Header().Rrtype == dns.TypeCNAME && q.Qtype != dns.TypeCNAME {
			m.Answer = append(m.Answer, rr)
			target := rr.(*dns.CNAME).Target
			for _, t := range z.records[target] { // follow within the zone, one step
				if t.Header().Rrtype == q.Qtype {
					m.Answer = append(m.Answer, t)
				}
			}
			return
		}
	}
	for _, rr := range rrs {
		if rr.Header().Rrtype == q.Qtype {
			m.Answer = append(m.Answer, rr)
		}
	}
	if len(m.Answer) > 0 {
		return
	}
	if !exists && name != z.name {
		m.Rcode = dns.RcodeNameError
	}
	m.Ns = append(m.Ns, a.soa(z))
}

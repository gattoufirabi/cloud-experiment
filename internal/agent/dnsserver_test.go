package agent

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"

	"platform/internal/config"
)

func TestDNSServer(t *testing.T) {
	a := &Agent{cfg: config.Config{DNSNameservers: []string{"ns1.platform.test", "ns2.platform.test"}}}
	z := &dnsZone{name: "shop.test.", serial: 7, records: map[string][]dns.RR{}, cuts: map[string]bool{}}
	add := func(name, typ, value string, prio int) {
		fq := recordFqdn(name, z.name)
		rr := buildRR(fq, typ, value, prio, 300)
		if rr == nil {
			t.Fatalf("bad record %s %s %s", name, typ, value)
		}
		z.records[fq] = append(z.records[fq], rr)
		if typ == "NS" && fq != z.name {
			z.cuts[fq] = true
		}
	}
	add("@", "A", "203.0.113.10", 0)
	add("www", "CNAME", "@.shop.test", 0) // CNAME into the zone is fine; validated by the API
	z.records["www.shop.test."][0].(*dns.CNAME).Target = "shop.test."
	add("@", "MX", "mail.shop.test", 10)
	add("@", "TXT", "v=spf1 -all", 0)
	add("*.apps", "A", "203.0.113.20", 0)
	add("dev", "NS", "ns.elsewhere.test", 0)
	a.dnsData.Store(&dnsData{zones: map[string]*dnsZone{z.name: z}})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(a.serveDNS)}
	go func() { _ = srv.ActivateAndServe() }()
	defer srv.Shutdown()
	time.Sleep(50 * time.Millisecond)
	c := &dns.Client{Timeout: 2 * time.Second}
	ask := func(name string, qt uint16) *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion(name, qt)
		r, _, err := c.Exchange(m, pc.LocalAddr().String())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r
	}
	if r := ask("shop.test.", dns.TypeA); !r.Authoritative || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "203.0.113.10" {
		t.Fatalf("apex A: %v", r)
	}
	if r := ask("WWW.shop.test.", dns.TypeA); len(r.Answer) != 2 || r.Answer[0].Header().Rrtype != dns.TypeCNAME {
		t.Fatalf("CNAME chain: %v", r.Answer)
	}
	if r := ask("shop.test.", dns.TypeNS); len(r.Answer) != 2 {
		t.Fatalf("apex NS: %v", r.Answer)
	}
	if r := ask("shop.test.", dns.TypeSOA); len(r.Answer) != 1 || r.Answer[0].(*dns.SOA).Serial != 7 {
		t.Fatalf("SOA: %v", r.Answer)
	}
	if r := ask("x.apps.shop.test.", dns.TypeA); len(r.Answer) != 1 || r.Answer[0].Header().Name != "x.apps.shop.test." {
		t.Fatalf("wildcard: %v", r.Answer)
	}
	if r := ask("nothing.shop.test.", dns.TypeA); r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 {
		t.Fatalf("NXDOMAIN: %v", r)
	}
	if r := ask("shop.test.", dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
		t.Fatalf("NODATA: %v", r)
	}
	if r := ask("a.dev.shop.test.", dns.TypeA); r.Authoritative || len(r.Ns) != 1 {
		t.Fatalf("delegation: %v", r)
	}
	if r := ask("example.org.", dns.TypeA); r.Rcode != dns.RcodeRefused {
		t.Fatalf("not hosted must be refused: %v", r.Rcode)
	}
	if r := ask("shop.test.", dns.TypeTXT); len(r.Answer) != 1 || r.Answer[0].(*dns.TXT).Txt[0] != "v=spf1 -all" {
		t.Fatalf("TXT: %v", r.Answer)
	}
}

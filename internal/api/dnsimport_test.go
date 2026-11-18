package api

import (
	"strings"
	"testing"
)

func TestParseZoneFile(t *testing.T) {
	zone := `$TTL 3600
@	IN SOA dns109.ovh.net. tech.ovh.net. (2095024096 86400 3600 3600000 60)
        IN NS     ns1.platform.test.
        IN NS     dns109.ovh.net.
        IN MX     5 mx1.mail.ovh.net.
        IN A     51.255.45.52
        IN TXT     "v=spf1 include:mx.ovh.com ~all"
_autodiscover._tcp        IN SRV     0 0 443 autodiscover.mail.ovh.net.
_dmarc        IN TXT     "v=DMARC1; p=none; rua=mailto:contact@example.test"
admin-test    300 IN A     51.255.45.52
api.flight        IN A     51.255.45.52
ftp        IN CNAME     example.test.
sub        IN NS     ns.elsewhere.test.
ovhmo-selector-1._domainkey        IN CNAME     ovhmo-selector-1._domainkey.4862379.ms.dkim.mail.ovh.net.
other.org.  IN A 192.0.2.1
`
	recs, skipped, err := parseZoneFile("example.test", zone)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]dnsRecordInput{}
	for _, r := range recs {
		got[r.Name+" "+r.Type] = r
	}
	for _, want := range []string{"@ MX", "@ A", "@ TXT", "_autodiscover._tcp SRV", "_dmarc TXT", "admin-test A", "api.flight A", "ftp CNAME", "sub NS", "ovhmo-selector-1._domainkey CNAME"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	if _, ok := got["@ NS"]; ok {
		t.Error("the apex NS records must be left out (the platform's own)")
	}
	if r := got["_autodiscover._tcp SRV"]; r.Value != "0 443 autodiscover.mail.ovh.net." || r.Priority != 0 {
		t.Errorf("SRV = %+v", r)
	}
	if r := got["@ MX"]; r.Priority != 5 || r.Value != "mx1.mail.ovh.net." {
		t.Errorf("MX = %+v", r)
	}
	if r := got["admin-test A"]; r.TTL != 300 {
		t.Errorf("TTL = %d", r.TTL)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "outside") {
		t.Errorf("skipped = %v", skipped)
	}
	// the stored form of SRV
	n, err := normalizeRecord("example.test", got["_autodiscover._tcp SRV"])
	if err != nil || n.Value != "0 443 autodiscover.mail.ovh.net" {
		t.Errorf("normalized SRV = %+v, %v", n, err)
	}
}

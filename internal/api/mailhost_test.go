package api

import (
	"strings"
	"testing"
)

// A zone file as the mail server gives it (DKIM keys first, the RSA one on several lines).
const stalwartZone = `v1-ed25519-20261119._domainkey.shop.test. IN TXT "v=DKIM1; k=ed25519; h=sha256; p=HxVl8bxifoIDBUNhOxeyEu0IvZeCbaovfzV/AoRH+lM="
v1-rsa-20261119._domainkey.shop.test. IN TXT (
    "v=DKIM1; k=rsa; h=sha256; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA577aXzhkt/1eo/f6phx8FZkhUi5MFycxYJMt5LtqZV7n5YvVXqYCiAGRpDyTSNWBhg58nrVgtIrWD4y+56DmyY1ZXK90f7M+vaEpeSqv9/vuXFw2ckgqoKo9VlSn/BVHWlSldrULuNHAgOw/EoXsp173+r2lYBl4ePVJ/+OxWPb7lOf32UgqOGZ"
    "Hx+TbU9VJPUXewxg2k2B2ZMVU/0qXL8ilSVgln8YGBKfcxkR86Zpdr2ld6KnoRfoo3D7TJuAu9SGCD5QqxiTynSvn9VjX/V4TqRfa3bRxu4Uy1bsg6AHAK/x4CvUnAn1DrsOyyi1w8ot/kccQNlRrmiapkosOPwIDAQAB"
)
shop.test. IN TXT "v=spf1 mx -all"
shop.test. IN MX 10 mail.platform.test.
_dmarc.shop.test. IN TXT "v=DMARC1; p=reject; rua=mailto:postmaster@shop.test"
`

func TestMailDKIMRecords(t *testing.T) {
	dkim := zoneDKIM(stalwartZone)
	if strings.Contains(dkim, "spf1") || strings.Count(dkim, "_domainkey") != 2 {
		t.Fatalf("zoneDKIM kept %q", dkim)
	}
	s := &Server{}
	recs := s.mailRecords(&mailDomain{Domain: "shop.test", DKIM: dkim})
	n := 0
	for _, r := range recs {
		if r.Type == "TXT" && strings.HasSuffix(r.Name, "._domainkey") {
			if _, err := normalizeRecord("shop.test", r.dnsRecordInput); err != nil {
				t.Errorf("%s refused: %v", r.Name, err)
			}
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d DKIM records, want 2: %+v", n, recs)
	}
}

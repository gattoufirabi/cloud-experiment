package mail

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// RFC 6376 section 3.4.5 example.
func TestRelaxedCanonicalization(t *testing.T) {
	fields, body := splitMessage("A: X\r\nB : Y\t\r\n\tZ  \r\n\r\n C \r\nD \t E\r\n\r\n\r\n")
	var got []string
	for _, f := range fields {
		got = append(got, relaxedHeader(f))
	}
	if strings.Join(got, "\r\n")+"\r\n" != "a:X\r\nb:Y Z\r\n" {
		t.Fatalf("headers: %q", got)
	}
	if rb := relaxedBody(body); rb != " C\r\nD E\r\n" {
		t.Fatalf("body: %q", rb)
	}
	if relaxedBody("\r\n\r\n") != "" {
		t.Fatal("empty body must canonicalize to nothing")
	}
}

func TestDKIMSignVerifies(t *testing.T) {
	priv, txt, err := NewDKIMKey()
	if err != nil {
		t.Fatal(err)
	}
	msg := "From: App <hello@shop.example.test>\r\nTo: buyer@example.test\r\nSubject:  Your   order\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThanks  for your order.\r\n\r\n"
	sig, err := DKIMSign(msg, "shop.example.test", "plat1", priv, time.Unix(1795000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sig, "DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=shop.example.test; s=plat1; t=1795000000; h=from:to:subject:date:mime-version:content-type; bh=") {
		t.Fatalf("signature header: %s", sig)
	}

	// Verify independently: a relay may re-fold and re-space headers (relaxed survives that).
	relayed := strings.Replace(sig+msg, "Subject:  Your   order", "Subject: Your\r\n order", 1)
	fields, body := splitMessage(relayed)
	tags := map[string]string{}
	_, rest, _ := strings.Cut(strings.TrimSuffix(fields[0], "\r\n"), ":")
	for _, kv := range strings.Split(rest, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		tags[k] = v
	}
	bh := sha256.Sum256([]byte(relaxedBody(body)))
	if tags["bh"] != base64.StdEncoding.EncodeToString(bh[:]) {
		t.Fatal("body hash mismatch")
	}
	var data strings.Builder
	for _, h := range strings.Split(tags["h"], ":") {
		for _, f := range fields[1:] {
			if n, _, _ := strings.Cut(f, ":"); strings.EqualFold(strings.TrimSpace(n), h) {
				data.WriteString(relaxedHeader(f) + "\r\n")
			}
		}
	}
	bAt := strings.LastIndex(fields[0], "; b=") + len("; b=")
	unsigned := fields[0][:bAt]
	data.WriteString(relaxedHeader(unsigned))
	sum := sha256.Sum256([]byte(data.String()))
	der, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(txt, "v=DKIM1; k=rsa; p="))
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := base64.StdEncoding.DecodeString(tags["b"])
	if err := rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, sum[:], b); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

package mail

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DKIM (RFC 6376) signing with rsa-sha256 and relaxed/relaxed canonicalization.
// Standard library only.

// DKIMSignedHeaders are signed when present, in this order.
var DKIMSignedHeaders = []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type", "Reply-To"}

// NewDKIMKey returns a 2048-bit RSA key as PEM and the DNS TXT value publishing it.
func NewDKIMKey() (privPEM []byte, txt string, err error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return nil, "", err
	}
	privPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return privPEM, "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der), nil
}

func parseDKIMKey(privPEM []byte) (*rsa.PrivateKey, error) {
	b, _ := pem.Decode(privPEM)
	if b == nil {
		return nil, errors.New("dkim: no PEM key")
	}
	return x509.ParsePKCS1PrivateKey(b.Bytes)
}

var wsp = regexp.MustCompile(`[ \t]+`)

// relaxedBody canonicalizes a CRLF body (RFC 6376 3.4.4).
func relaxedBody(body string) string {
	lines := strings.Split(body, "\r\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(wsp.ReplaceAllString(l, " "), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// relaxedHeader canonicalizes one header field, "Name: value" possibly folded (RFC 6376 3.4.2).
func relaxedHeader(field string) string {
	name, value, _ := strings.Cut(field, ":")
	value = strings.ReplaceAll(value, "\r\n", "")
	value = strings.TrimSpace(wsp.ReplaceAllString(value, " "))
	return strings.ToLower(strings.TrimSpace(name)) + ":" + value
}

// splitMessage returns the header fields (unfolded lines kept with their folding)
// and the body of a CRLF message.
func splitMessage(msg string) (fields []string, body string) {
	head, body, _ := strings.Cut(msg, "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(fields) > 0 {
			fields[len(fields)-1] += "\r\n" + line
			continue
		}
		fields = append(fields, line)
	}
	return fields, body
}

// DKIMSign returns the DKIM-Signature header field (with its trailing CRLF) for a
// complete CRLF message, to be prepended to it.
func DKIMSign(msg, domain, selector string, privPEM []byte, now time.Time) (string, error) {
	key, err := parseDKIMKey(privPEM)
	if err != nil {
		return "", err
	}
	fields, body := splitMessage(msg)
	bh := sha256.Sum256([]byte(relaxedBody(body)))
	byName := map[string][]string{}
	for _, f := range fields {
		n, _, _ := strings.Cut(f, ":")
		byName[strings.ToLower(strings.TrimSpace(n))] = append(byName[strings.ToLower(strings.TrimSpace(n))], f)
	}
	var names []string
	var data strings.Builder
	for _, h := range DKIMSignedHeaders {
		fs := byName[strings.ToLower(h)]
		if len(fs) == 0 {
			continue
		}
		// The last instance is signed (RFC 6376 5.4.2); messages here have one of each.
		data.WriteString(relaxedHeader(fs[len(fs)-1]) + "\r\n")
		names = append(names, strings.ToLower(h))
	}
	sig := fmt.Sprintf("DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; d=%s; s=%s; t=%s; h=%s; bh=%s; b=",
		domain, selector, strconv.FormatInt(now.Unix(), 10), strings.Join(names, ":"), base64.StdEncoding.EncodeToString(bh[:]))
	data.WriteString(relaxedHeader(sig))
	sum := sha256.Sum256([]byte(data.String()))
	b, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return sig + base64.StdEncoding.EncodeToString(b) + "\r\n", nil
}

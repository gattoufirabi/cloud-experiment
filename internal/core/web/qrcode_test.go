package web

import (
	"strings"
	"testing"

	"rsc.io/qr"

	"platform/internal/lib/secure"
)

func TestQRSVGEncodesTheTOTPLink(t *testing.T) {
	uri := secure.TOTPURI("Platform", "owner@example.test", "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP")
	svg, err := QrSVG(uri, "QR code <setup>")
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") || !strings.Contains(s, `<path d="M`) {
		t.Fatalf("not an SVG QR code: %.80s", s)
	}
	if strings.Contains(s, "<setup>") || !strings.Contains(s, "&lt;setup&gt;") {
		t.Error("label must be escaped")
	}
	// The SVG's modules must match the encoder's matrix (white border of 4 modules).
	code, _ := qr.Encode(uri, qr.M)
	if want := code.Size + 8; !strings.Contains(s, `viewBox="0 0 `+Itoa(want)+" "+Itoa(want)+`"`) {
		t.Errorf("unexpected size, want %d modules with the border", want)
	}
}

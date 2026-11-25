package web

import (
	"fmt"
	"html/template"
	"strings"

	"rsc.io/qr"
)

// qrSVG renders text as a QR code in inline SVG: no image request, no script, so it
// works under the strict Content-Security-Policy. Error correction M (15%) keeps an
// otpauth link small enough for phone cameras. The SVG is built only from numbers we
// compute, so it is safe to mark as trusted HTML.
func QrSVG(text, label string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4 // white border, in modules, required by the QR spec
	n := code.Size
	var path strings.Builder
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	size := n + 2*quiet
	return template.HTML(fmt.Sprintf(
		`<svg class="qr" viewBox="0 0 %d %d" role="img" aria-label="%s" shape-rendering="crispEdges" xmlns="http://www.w3.org/2000/svg">`+
			`<rect width="%d" height="%d" fill="#fff"/><path d="%s" fill="#000"/></svg>`,
		size, size, template.HTMLEscapeString(label), size, size, path.String())), nil
}

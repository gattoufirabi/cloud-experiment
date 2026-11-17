package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Message is an email a tenant sends.
type Message struct {
	From, ReplyTo string
	To            []string
	Subject       string
	Text, HTML    string
}

// ParseAddress validates one address ("Name <a@b>" or "a@b") and returns its
// canonical form and the bare address.
func ParseAddress(s string) (formatted, addr string, err error) {
	if strings.ContainsAny(s, "\r\n") {
		return "", "", errors.New("line break in address")
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", "", err
	}
	return a.String(), strings.ToLower(a.Address), nil
}

func qp(s string) string {
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")))
	_ = w.Close()
	return b.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Compose builds the CRLF message (headers and body) for m; addresses must have
// been checked with ParseAddress. domain names the Message-ID.
func Compose(m Message, domain string, now time.Time) string {
	var h strings.Builder
	h.WriteString("From: " + m.From + "\r\n")
	h.WriteString("To: " + strings.Join(m.To, ", ") + "\r\n")
	if m.ReplyTo != "" {
		h.WriteString("Reply-To: " + m.ReplyTo + "\r\n")
	}
	h.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", m.Subject) + "\r\n")
	h.WriteString("Date: " + now.UTC().Format(time.RFC1123Z) + "\r\n")
	h.WriteString("Message-ID: <" + randHex(12) + "@" + domain + ">\r\n")
	h.WriteString("MIME-Version: 1.0\r\n")
	switch {
	case m.HTML == "":
		h.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		h.WriteString(qp(m.Text))
	case m.Text == "":
		h.WriteString("Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		h.WriteString(qp(m.HTML))
	default:
		boundary := "b" + randHex(12)
		h.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
		h.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		h.WriteString(qp(m.Text) + "\r\n")
		h.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		h.WriteString(qp(m.HTML) + "\r\n")
		h.WriteString("--" + boundary + "--\r\n")
	}
	return h.String()
}

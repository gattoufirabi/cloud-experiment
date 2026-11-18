package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
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
	Attachments   []Attachment
}

// Attachment is a file sent with a message.
type Attachment struct {
	Filename, ContentType string
	Data                  []byte
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

// body renders the text and HTML parts (one of them, or both as alternatives) and
// returns their Content-Type header value (with the transfer encoding) and content.
func body(m Message) (contentType, content string) {
	switch {
	case m.HTML == "":
		return "text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable", qp(m.Text)
	case m.Text == "":
		return "text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable", qp(m.HTML)
	}
	boundary := "b" + randHex(12)
	var b strings.Builder
	b.WriteString("--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.WriteString(qp(m.Text) + "\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	b.WriteString(qp(m.HTML) + "\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return "multipart/alternative; boundary=\"" + boundary + "\"", b.String()
}

// b64 encodes data in lines of 76 characters (RFC 2045).
func b64(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
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
	ct, content := body(m)
	if len(m.Attachments) == 0 {
		h.WriteString("Content-Type: " + ct + "\r\n\r\n" + content)
		return h.String()
	}
	boundary := "m" + randHex(12)
	h.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
	h.WriteString("--" + boundary + "\r\nContent-Type: " + ct + "\r\n\r\n" + content + "\r\n")
	for _, a := range m.Attachments {
		name := mime.QEncoding.Encode("utf-8", a.Filename)
		h.WriteString("--" + boundary + "\r\n")
		h.WriteString("Content-Type: " + a.ContentType + "; name=\"" + name + "\"\r\n")
		h.WriteString("Content-Transfer-Encoding: base64\r\n")
		h.WriteString("Content-Disposition: attachment; filename=\"" + name + "\"\r\n\r\n")
		h.WriteString(b64(a.Data))
	}
	h.WriteString("--" + boundary + "--\r\n")
	return h.String()
}

// PrepareRaw checks and normalizes a complete message received over SMTP: line endings
// become CRLF, Bcc headers are removed (they must not reach recipients), and a Date and
// a Message-ID are added when missing. It returns the message and its From address.
func PrepareRaw(raw []byte, domain string, now time.Time) (msg, from string, err error) {
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n", "\r\n")
	head, rest, ok := strings.Cut(text, "\r\n\r\n")
	if !ok {
		head, rest = strings.TrimSuffix(text, "\r\n"), ""
	}
	var fields []string // header fields; folded lines stay with their field
	for _, line := range strings.Split(head, "\r\n") {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(fields) > 0 {
			fields[len(fields)-1] += "\r\n" + line
			continue
		}
		if !strings.Contains(line, ":") {
			return "", "", errors.New("malformed header line")
		}
		fields = append(fields, line)
	}
	var kept []string
	has := map[string]bool{}
	for _, f := range fields {
		colon := strings.Index(f, ":")
		name := strings.ToLower(strings.TrimSpace(f[:colon]))
		has[name] = true
		switch name {
		case "bcc":
			continue
		case "from":
			if from != "" {
				return "", "", errors.New("more than one From header")
			}
			v := strings.TrimSpace(strings.ReplaceAll(f[colon+1:], "\r\n", ""))
			_, addr, perr := ParseAddress(v)
			if perr != nil {
				return "", "", errors.New("the From header is not a single valid address")
			}
			from = addr
		}
		kept = append(kept, f)
	}
	if from == "" {
		return "", "", errors.New("the message has no From header")
	}
	if !has["date"] {
		kept = append(kept, "Date: "+now.UTC().Format(time.RFC1123Z))
	}
	if !has["message-id"] {
		kept = append(kept, "Message-ID: <"+randHex(12)+"@"+domain+">")
	}
	return strings.Join(kept, "\r\n") + "\r\n\r\n" + rest, from, nil
}

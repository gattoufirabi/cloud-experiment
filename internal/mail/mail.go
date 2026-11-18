// Package mail composes, signs and delivers email: the platform's own notices and the
// messages tenants send. Delivery goes through an SMTP relay (SMTP_HOST: your own
// Postfix, Stalwart, Mailcow… or a provider) or straight to the recipients' mail servers
// (deliver.go). Standard library only.
package mail

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"

	"platform/internal/config"
)

var ErrNotConfigured = errors.New("no SMTP server configured (SMTP_HOST, SMTP_FROM)")

// Configured reports whether the platform can send its own emails: a sender address
// (SMTP_FROM) and a way out (a relay, or direct delivery).
func Configured(cfg config.Config) bool {
	return cfg.SMTPFrom != "" && (cfg.SMTPHost != "" || cfg.EmailMode() == "direct")
}

// Relay reports whether the platform's own emails go through the SMTP relay (sent at
// once); otherwise they are queued and delivered directly by the agent.
func Relay(cfg config.Config) bool { return cfg.SMTPHost != "" && cfg.SMTPFrom != "" }

// Plain composes a plain-text notice from SMTP_FROM. It returns the message, the
// envelope sender and the sender's domain. Header injection is impossible: the address
// and the subject are refused if they contain line breaks.
func Plain(cfg config.Config, to, subject, body string) (msg, envFrom, domain string, err error) {
	for _, v := range []string{to, subject, cfg.SMTPFrom} {
		if strings.ContainsAny(v, "\r\n") {
			return "", "", "", errors.New("invalid header value")
		}
	}
	envFrom = cfg.SMTPFrom
	if i := strings.LastIndex(envFrom, "<"); i >= 0 {
		envFrom = strings.TrimSuffix(envFrom[i+1:], ">")
	}
	domain = strings.ToLower(envFrom[strings.LastIndex(envFrom, "@")+1:])
	idBytes := make([]byte, 12)
	_, _ = rand.Read(idBytes)
	msg = "From: " + cfg.SMTPFrom + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"Message-ID: <" + hex.EncodeToString(idBytes) + "@" + domain + ">\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n")
	return msg, envFrom, domain, nil
}

// Send delivers one notice through the relay at once.
func Send(cfg config.Config, to, subject, body string) error {
	if !Relay(cfg) {
		return ErrNotConfigured
	}
	msg, envFrom, _, err := Plain(cfg, to, subject, body)
	if err != nil {
		return err
	}
	return SendRaw(cfg, envFrom, []string{to}, msg)
}

// SendRaw hands a complete CRLF message to the SMTP relay; it fails unless every
// recipient was accepted.
func SendRaw(cfg config.Config, envelopeFrom string, to []string, msg string) error {
	if cfg.SMTPHost == "" {
		return ErrNotConfigured
	}
	for _, r := range SendRelay(cfg, envelopeFrom, to, msg) {
		if r.Status != Delivered {
			return fmt.Errorf("recipient %s: %s", r.Addr, r.Detail)
		}
	}
	return nil
}

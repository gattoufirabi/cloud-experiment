// Package mail sends plain-text email through any SMTP server (your own Postfix,
// Stalwart, Mailcow… or a relay). Standard library only.
package mail

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"platform/internal/config"
)

var ErrNotConfigured = errors.New("no SMTP server configured (SMTP_HOST, SMTP_FROM)")

func Configured(cfg config.Config) bool { return cfg.SMTPHost != "" && cfg.SMTPFrom != "" }

// Send delivers one message. Header injection is impossible: addresses and the
// subject are refused if they contain line breaks.
func Send(cfg config.Config, to, subject, body string) error {
	if !Configured(cfg) {
		return ErrNotConfigured
	}
	for _, v := range []string{to, subject, cfg.SMTPFrom} {
		if strings.ContainsAny(v, "\r\n") {
			return errors.New("invalid header value")
		}
	}
	idBytes := make([]byte, 12)
	_, _ = rand.Read(idBytes)
	domain := cfg.SMTPFrom[strings.LastIndex(cfg.SMTPFrom, "@")+1:]
	msg := "From: " + cfg.SMTPFrom + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"Message-ID: <" + hex.EncodeToString(idBytes) + "@" + strings.TrimSuffix(domain, ">") + ">\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n")
	from := cfg.SMTPFrom
	if i := strings.LastIndex(from, "<"); i >= 0 {
		from = strings.TrimSuffix(from[i+1:], ">")
	}
	return SendRaw(cfg, from, []string{to}, msg)
}

// SendRaw hands a complete CRLF message to the SMTP server for the given envelope.
func SendRaw(cfg config.Config, envelopeFrom string, to []string, msg string) error {
	if cfg.SMTPHost == "" {
		return ErrNotConfigured
	}
	host := cfg.SMTPHost
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.SMTPPort))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if cfg.SMTPTLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.SMTPTLS == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if cfg.SMTPUser != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(envelopeFrom); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("recipient %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

package mail

import (
	"bufio"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

// fakeSMTP answers one session: RCPT for addresses starting with "unknown" gets 550,
// "busy" gets 451, others 250.
func fakeSMTP(t *testing.T) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got = make(chan string, 1)
	go func() {
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "MAIL"):
				say("250 ok")
			case strings.HasPrefix(cmd, "RCPT TO:<UNKNOWN"):
				say("550 5.1.1 no such user")
			case strings.HasPrefix(cmd, "RCPT TO:<BUSY"):
				say("451 4.3.0 try later")
			case strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				got <- data.String()
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestTransactPerRecipient(t *testing.T) {
	addr, got := fakeSMTP(t)
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	out := transact(c, "a@example.com", []string{"ok@example.org", "unknown@example.org", "busy@example.org"}, "Subject: x\r\n\r\nhello\r\n")
	_ = c.Quit()
	want := map[string]string{"ok@example.org": Delivered, "unknown@example.org": Bounced, "busy@example.org": Deferred}
	for _, r := range out {
		if want[r.Addr] != r.Status {
			t.Errorf("%s: %s (%s), want %s", r.Addr, r.Status, r.Detail, want[r.Addr])
		}
	}
	select {
	case d := <-got:
		if !strings.Contains(d, "hello") {
			t.Errorf("data = %q", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no data received")
	}
}

func TestComposeAttachment(t *testing.T) {
	m := Compose(Message{From: "a@example.com", To: []string{"b@example.org"}, Subject: "Invoice", Text: "See attached.",
		Attachments: []Attachment{{Filename: "invoice.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 test")}}}, "example.com", time.Now())
	for _, want := range []string{"multipart/mixed", `filename="invoice.pdf"`, "Content-Transfer-Encoding: base64", "JVBERi0xLjQgdGVzdA=="} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q", want)
		}
	}
}

func TestPrepareRaw(t *testing.T) {
	msg, from, err := PrepareRaw([]byte("From: Shop <Hello@Example.com>\nTo: b@example.org\nBcc: secret@example.org\nSubject: hi\n\nbody\n"), "example.com", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if from != "hello@example.com" {
		t.Errorf("from = %q", from)
	}
	if strings.Contains(msg, "secret@") || !strings.Contains(msg, "Message-ID:") || !strings.Contains(msg, "\r\n\r\nbody\r\n") {
		t.Errorf("prepared message:\n%s", msg)
	}
	if _, _, err := PrepareRaw([]byte("To: b@example.org\n\nbody"), "example.com", time.Now()); err == nil {
		t.Error("a message without From was accepted")
	}
}

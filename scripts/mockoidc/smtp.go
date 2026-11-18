package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

// A mail server for the end-to-end test of direct delivery and bounces: recipients
// whose address starts with "nobody" are refused for good (550 5.1.1), others accepted.
// And a webhook receiver: POST /hook keeps the body and its signature header; GET
// /hook/log lists them, one "signature body" per line.

func serveSMTP(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock SMTP on %s", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go smtpSession(c)
	}
}

func smtpSession(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
	say("220 mock.e2e.test ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 mock.e2e.test")
		case strings.HasPrefix(cmd, "RCPT TO:<NOBODY"):
			say("550 5.1.1 The email account that you tried to reach does not exist")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"), strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
			say("250 2.0.0 ok")
		case cmd == "DATA":
			say("354 go ahead")
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
			}
			say("250 2.0.0 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("502 5.5.1 not implemented")
		}
	}
}

type hookLog struct {
	mu    sync.Mutex
	lines []string
}

func (h *hookLog) handlers() {
	http.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		h.mu.Lock()
		h.lines = append(h.lines, r.Header.Get("X-Platform-Signature")+" "+strings.ReplaceAll(string(b), "\n", " "))
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	http.HandleFunc("/hook/log", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		fmt.Fprint(w, strings.Join(h.lines, "\n"))
	})
}

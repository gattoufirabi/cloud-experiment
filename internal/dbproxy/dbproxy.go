// Package dbproxy is the public endpoint of a managed database: a small TCP proxy run
// next to the database (its own container, on the project network, with the public port
// published). It lets in only the allowed client addresses, ends TLS with the database's
// own certificate, and forwards the connection to the database, which never faces the
// internet itself.
//
//   - PostgreSQL: the client's SSLRequest is answered "S" and TLS starts (sslmode=require,
//     verify-ca or verify-full); a direct TLS connection (sslnegotiation=direct, libpq 17)
//     works too. Plain-text connections are refused.
//   - Redis and MongoDB: TLS from the first byte (rediss://, tls=true).
package dbproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Config is read from the environment: DBPROXY_LISTEN, DBPROXY_UPSTREAM, DBPROXY_ENGINE,
// DBPROXY_ALLOW (comma-separated CIDRs), DBPROXY_CERT and DBPROXY_KEY (PEM).
type Config struct {
	Listen, Upstream, Engine string
	Allow                    []netip.Prefix
	Cert                     tls.Certificate
}

func FromEnv() (Config, error) {
	c := Config{Listen: os.Getenv("DBPROXY_LISTEN"), Upstream: os.Getenv("DBPROXY_UPSTREAM"), Engine: os.Getenv("DBPROXY_ENGINE")}
	if c.Listen == "" {
		c.Listen = ":6000"
	}
	if c.Upstream == "" {
		return c, errors.New("DBPROXY_UPSTREAM is not set")
	}
	switch c.Engine {
	case "postgres", "redis", "mongodb":
	default:
		return c, fmt.Errorf("engine %q has no public endpoint", c.Engine)
	}
	for _, s := range strings.Split(os.Getenv("DBPROXY_ALLOW"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return c, fmt.Errorf("allow-list: %w", err)
		}
		c.Allow = append(c.Allow, p.Masked())
	}
	if len(c.Allow) == 0 {
		return c, errors.New("the allow-list is empty")
	}
	cert, err := tls.X509KeyPair([]byte(os.Getenv("DBPROXY_CERT")), []byte(os.Getenv("DBPROXY_KEY")))
	if err != nil {
		return c, fmt.Errorf("certificate: %w", err)
	}
	c.Cert = cert
	return c, nil
}

func (c Config) allowed(addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	for _, p := range c.Allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Run serves until ctx ends.
func Run(ctx context.Context, c Config) error {
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{c.Cert}, MinVersion: tls.VersionTLS12}
	if c.Engine == "postgres" {
		tlsCfg.NextProtos = []string{"postgresql"} // direct TLS negotiation (libpq 17)
	}
	slog.Info("database endpoint listening", "listen", c.Listen, "engine", c.Engine, "allowed", len(c.Allow))
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if !c.allowed(conn.RemoteAddr()) {
			slog.Info("refused: address not on the allow-list", "remote", conn.RemoteAddr().String())
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.serve(conn, tlsCfg); err != nil {
				slog.Info("connection closed", "remote", conn.RemoteAddr().String(), "err", err)
			}
		}()
	}
}

// prefixed replays bytes already read before the rest of the connection.
type prefixed struct {
	net.Conn
	r io.Reader
}

func (p *prefixed) Read(b []byte) (int, error) { return p.r.Read(b) }

func (c Config) serve(conn net.Conn, tlsCfg *tls.Config) error {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second)) // handshake
	var client net.Conn = conn
	if c.Engine == "postgres" {
		br := bufio.NewReader(conn)
		for {
			head, err := br.Peek(8)
			if err != nil {
				return err
			}
			if head[0] == 0x16 { // a TLS ClientHello: direct negotiation
				break
			}
			n, code := binary.BigEndian.Uint32(head[:4]), binary.BigEndian.Uint32(head[4:])
			if n != 8 {
				return errors.New("plain-text connection refused: use sslmode=require")
			}
			_, _ = br.Discard(8)
			switch code {
			case 80877103: // SSLRequest
				if _, err := conn.Write([]byte{'S'}); err != nil {
					return err
				}
			case 80877104: // GSSENCRequest: not offered, the client goes on with SSLRequest
				if _, err := conn.Write([]byte{'N'}); err != nil {
					return err
				}
				continue
			default:
				return errors.New("unexpected request")
			}
			break
		}
		client = &prefixed{Conn: conn, r: br}
	}
	tc := tls.Server(client, tlsCfg)
	if err := tc.Handshake(); err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	up, err := net.DialTimeout("tcp", c.Upstream, 10*time.Second)
	if err != nil {
		return fmt.Errorf("database unreachable: %w", err)
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, tc); done <- struct{}{} }()
	go func() { _, _ = io.Copy(tc, up); done <- struct{}{} }()
	<-done
	return nil
}

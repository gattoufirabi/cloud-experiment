package dbproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"
)

func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "db.test"}, DNSNames: []string{"db.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	c, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// echo is the "database": it sends back what it receives.
func echo(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func start(t *testing.T, engine string, allow string) string {
	t.Helper()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c := Config{Listen: addr, Upstream: echo(t), Engine: engine, Allow: []netip.Prefix{netip.MustParsePrefix(allow)}, Cert: testCert(t)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Run(ctx, c)
	time.Sleep(100 * time.Millisecond)
	return addr
}

func TestPostgresSSLRequest(t *testing.T) {
	addr := start(t, "postgres", "127.0.0.0/8")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := make([]byte, 8)
	binary.BigEndian.PutUint32(req, 8)
	binary.BigEndian.PutUint32(req[4:], 80877103)
	conn.Write(req)
	b := make([]byte, 1)
	if _, err := io.ReadFull(conn, b); err != nil || b[0] != 'S' {
		t.Fatalf("answer %q, %v", b, err)
	}
	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	tc.Write([]byte("startup"))
	got := make([]byte, 7)
	if _, err := io.ReadFull(tc, got); err != nil || string(got) != "startup" {
		t.Fatalf("through the proxy: %q, %v", got, err)
	}
}

func TestPlainPostgresRefused(t *testing.T) {
	addr := start(t, "postgres", "127.0.0.0/8")
	conn, _ := net.Dial("tcp", addr)
	defer conn.Close()
	startup := make([]byte, 8)
	binary.BigEndian.PutUint32(startup, 40)
	binary.BigEndian.PutUint32(startup[4:], 196608) // protocol 3.0 startup, no TLS
	conn.Write(startup)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a plain-text connection was served")
	}
}

func TestRawTLSAndAllowList(t *testing.T) {
	addr := start(t, "redis", "127.0.0.0/8")
	tc, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	tc.Write([]byte("PING"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(tc, got); err != nil || string(got) != "PING" {
		t.Fatalf("got %q, %v", got, err)
	}
	tc.Close()
	refused := start(t, "redis", "10.9.9.0/24") // 127.0.0.1 is not allowed
	if c, err := tls.Dial("tcp", refused, &tls.Config{InsecureSkipVerify: true}); err == nil {
		c.Close()
		t.Fatal("an address outside the allow-list connected")
	}
}

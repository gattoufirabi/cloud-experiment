package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDNSAnswersTXT(t *testing.T) {
	store := &txtStore{recs: map[string][]rec{}}
	long := "v=DKIM1; k=rsa; p=" + strings.Repeat("A", 392)
	store.recs["p1._domainkey.shop.example.test"] = []rec{{"TXT", long}}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	go serveDNS(addr, store)
	time.Sleep(100 * time.Millisecond)
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", addr)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	txts, err := r.LookupTXT(ctx, "P1._domainkey.shop.example.test")
	if err != nil || len(txts) != 1 || txts[0] != long {
		t.Fatalf("got %q, %v", txts, err)
	}
	if _, err := r.LookupTXT(ctx, "missing.example.test"); err == nil {
		t.Fatal("missing name resolved")
	}
	// direct email delivery: a wildcard MX and the mail server's address
	store.recs["*"] = []rec{{"MX", "mx.e2e.test"}}
	store.recs["mx.e2e.test"] = []rec{{"A", "10.1.2.3"}}
	mxs, err := r.LookupMX(ctx, "anything.example.org")
	if err != nil || len(mxs) != 1 || mxs[0].Host != "mx.e2e.test." {
		t.Fatalf("MX: %v, %v", mxs, err)
	}
	ips, err := r.LookupIP(ctx, "ip4", "mx.e2e.test")
	if err != nil || len(ips) != 1 || ips[0].String() != "10.1.2.3" {
		t.Fatalf("A: %v, %v", ips, err)
	}
}

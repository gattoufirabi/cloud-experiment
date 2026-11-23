package api

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// The pool skips a connection that died while idle, then opens a stream on the next.
func TestTunnelOpen(t *testing.T) {
	p := newTunnelPool()
	dead, deadPeer := net.Pipe()
	deadPeer.Close()
	p.put("n1", dead)
	srv, node := net.Pipe()
	p.put("n1", srv)
	go func() {
		line, err := readLine(node)
		if err != nil || line != "OPEN dep-1" {
			node.Close()
			return
		}
		io.WriteString(node, "OK\n")
		b := make([]byte, 4)
		io.ReadFull(node, b)
		node.Write(b)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := p.open(ctx, "n1", "dep-1")
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping"))
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("stream: %q %v", b, err)
	}
	if p.idleCount("n1") != 0 {
		t.Fatal("used connections must leave the pool")
	}
}

// A server that answers ERR fails the request with its reason; no server = an error after the wait.
func TestTunnelOpenErrors(t *testing.T) {
	p := newTunnelPool()
	srv, node := net.Pipe()
	p.put("n2", srv)
	go func() { readLine(node); io.WriteString(node, "ERR no running instance\n") }()
	if _, err := p.open(context.Background(), "n2", "dep"); err == nil || err.Error() != "server: no running instance" {
		t.Fatalf("got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.open(ctx, "absent", "dep"); err == nil {
		t.Fatal("expected an error without a connected server")
	}
	p.put("n3", srv)
	p.dropNode("n3")
	if p.idleCount("n3") != 0 {
		t.Fatal("dropNode must close idle connections")
	}
}

package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	TunnelHeader       = "X-Platform-Tunnel" // set by the proxy (internal/core/worker/caddy.go)
	MaxIdleTunnels     = 32                  // per server
	tunnelWaitTimeout  = 5 * time.Second
	tunnelOpenAttempts = 3
)

type TunnelPool struct {
	Mu   sync.Mutex
	Idle map[string]chan net.Conn
	// deployment -> server, cached briefly (the proxy only routes live deployments)
	Routes map[string]TunnelRoute
}

type TunnelRoute struct {
	Node    string
	Expires time.Time
}

func (p *TunnelPool) ch(node string) chan net.Conn {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	c, ok := p.Idle[node]
	if !ok {
		c = make(chan net.Conn, MaxIdleTunnels)
		p.Idle[node] = c
	}
	return c
}

func (p *TunnelPool) Put(node string, c net.Conn) {
	select {
	case p.ch(node) <- c:
	default:
		_ = c.Close() // enough idle connections already
	}
}

func (p *TunnelPool) IdleCount(node string) int {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	return len(p.Idle[node])
}

// dropNode closes every idle connection of a removed server.
func (p *TunnelPool) DropNode(node string) {
	p.Mu.Lock()
	c := p.Idle[node]
	delete(p.Idle, node)
	for k, r := range p.Routes {
		if r.Node == node {
			delete(p.Routes, k)
		}
	}
	p.Mu.Unlock()
	if c == nil {
		return
	}
	for {
		select {
		case conn := <-c:
			_ = conn.Close()
		default:
			return
		}
	}
}

// open returns a stream to one instance of the deployment on the server.
func (p *TunnelPool) Open(ctx context.Context, node, deployment string) (net.Conn, error) {
	c := p.ch(node)
	var lastErr error = errors.New("the server is not connected")
	for attempt := 0; attempt < tunnelOpenAttempts; attempt++ {
		var conn net.Conn
		select {
		case conn = <-c:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(tunnelWaitTimeout):
			return nil, lastErr
		}
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprintf(conn, "OPEN %s\n", deployment); err != nil {
			_ = conn.Close()
			lastErr = err
			continue // a connection that died while idle: try the next one
		}
		line, err := ReadLine(conn)
		if err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		if line != "OK" {
			_ = conn.Close()
			return nil, fmt.Errorf("server: %s", strings.TrimPrefix(line, "ERR "))
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
	return nil, lastErr
}

// readLine reads one "\n"-terminated line byte by byte, so nothing after it is consumed.
func ReadLine(c net.Conn) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < 512 {
		if _, err := c.Read(one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return strings.TrimRight(string(b), "\r"), nil
		}
		b = append(b, one[0])
	}
	return "", errors.New("tunnel: line too long")
}

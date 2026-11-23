package api

// Tunnels to customer-owned servers. A server keeps a few connections open to
// GET /node/v1/tunnel (an HTTP Upgrade through the proxy); they wait here, idle. When a
// visitor's request for one of its applications arrives, the proxy sends it to the
// tunnel listener (TunnelAddr, internal only) with the deployment in a header; the
// listener takes an idle connection of the server running that deployment, asks the
// server to open a stream to one of its instances, and forwards the request over it.
// The server never accepts incoming connections, and the platform never connects to it.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"platform/internal/nodeproto"
)

const (
	tunnelHeader       = "X-Platform-Tunnel" // set by the proxy (internal/agent/caddy.go)
	maxIdleTunnels     = 32                  // per server
	tunnelWaitTimeout  = 5 * time.Second
	tunnelOpenAttempts = 3
)

type tunnelPool struct {
	mu   sync.Mutex
	idle map[string]chan net.Conn
	// deployment -> server, cached briefly (the proxy only routes live deployments)
	routes map[string]tunnelRoute
}

type tunnelRoute struct {
	node    string
	expires time.Time
}

func newTunnelPool() *tunnelPool {
	return &tunnelPool{idle: map[string]chan net.Conn{}, routes: map[string]tunnelRoute{}}
}

func (p *tunnelPool) ch(node string) chan net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.idle[node]
	if !ok {
		c = make(chan net.Conn, maxIdleTunnels)
		p.idle[node] = c
	}
	return c
}

func (p *tunnelPool) put(node string, c net.Conn) {
	select {
	case p.ch(node) <- c:
	default:
		_ = c.Close() // enough idle connections already
	}
}

func (p *tunnelPool) idleCount(node string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle[node])
}

// dropNode closes every idle connection of a removed server.
func (p *tunnelPool) dropNode(node string) {
	p.mu.Lock()
	c := p.idle[node]
	delete(p.idle, node)
	for k, r := range p.routes {
		if r.node == node {
			delete(p.routes, k)
		}
	}
	p.mu.Unlock()
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
func (p *tunnelPool) open(ctx context.Context, node, deployment string) (net.Conn, error) {
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
		line, err := readLine(conn)
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
func readLine(c net.Conn) (string, error) {
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

// nodeTunnel upgrades the server's request into an idle tunnel connection.
func (s *Server) nodeTunnel(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), nodeproto.TunnelProtocol) {
		apiError(w, http.StatusBadRequest, "expected Upgrade: "+nodeproto.TunnelProtocol)
		return
	}
	if n.Suspended {
		apiError(w, http.StatusForbidden, "the organization is suspended")
		return
	}
	if s.tunnels.idleCount(n.ID) >= maxIdleTunnels {
		apiError(w, http.StatusTooManyRequests, "enough tunnel connections are open")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		apiError(w, http.StatusInternalServerError, "tunnels are not supported here")
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{}) // the server's read/write timeouts do not apply to a tunnel
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + nodeproto.TunnelProtocol + "\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	if err := rw.Flush(); err != nil || rw.Reader.Buffered() > 0 { // the server sends nothing before OPEN
		_ = conn.Close()
		return
	}
	s.tunnels.put(n.ID, conn)
}

// tunnelRoutes is the internal tunnel listener: only the proxy reaches it.
func (s *Server) tunnelRoutes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dep := r.Header.Get(tunnelHeader)
		if !uuidRe.MatchString(dep) {
			http.Error(w, "No application is published at this address.", http.StatusNotFound)
			return
		}
		node, err := s.tunnelNode(r.Context(), dep)
		if err != nil {
			http.Error(w, "No application is published at this address.", http.StatusNotFound)
			return
		}
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = "app"
				req.Header.Del(tunnelHeader)
				if _, ok := req.Header["User-Agent"]; !ok {
					req.Header.Set("User-Agent", "")
				}
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return s.tunnels.open(ctx, node, dep)
				},
				DisableKeepAlives:     true, // one stream per connection
				ResponseHeaderTimeout: 5 * time.Minute,
				DisableCompression:    true,
			},
			FlushInterval: -1, // stream responses (server-sent events, downloads) as they come
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				s.log.Warn("tunnel", "deployment", dep, "node", node, "err", err)
				http.Error(w, "The server running this application is not reachable right now.", http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}

func (s *Server) tunnelNode(ctx context.Context, dep string) (string, error) {
	s.tunnels.mu.Lock()
	rt, ok := s.tunnels.routes[dep]
	s.tunnels.mu.Unlock()
	if ok && time.Now().Before(rt.expires) {
		return rt.node, nil
	}
	var node, org string
	if err := s.pool.QueryRow(ctx, `SELECT node_id, org_id FROM customer_deployment_node($1)`, dep).Scan(&node, &org); err != nil {
		return "", err
	}
	s.tunnels.mu.Lock()
	if len(s.tunnels.routes) > 10000 {
		s.tunnels.routes = map[string]tunnelRoute{}
	}
	s.tunnels.routes[dep] = tunnelRoute{node: node, expires: time.Now().Add(10 * time.Second)}
	s.tunnels.mu.Unlock()
	return node, nil
}

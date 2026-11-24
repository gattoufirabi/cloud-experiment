package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"platform/internal/components/servers/nodeproto"
	"platform/internal/core/web"
)

func NewTunnelPool() *web.TunnelPool {
	return &web.TunnelPool{Idle: map[string]chan net.Conn{}, Routes: map[string]web.TunnelRoute{}}
}

// nodeTunnel upgrades the server's request into an idle tunnel connection.
func (s *API) nodeTunnel(w http.ResponseWriter, r *http.Request, n *nodeInfo) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), nodeproto.TunnelProtocol) {
		web.APIError(w, http.StatusBadRequest, "expected Upgrade: "+nodeproto.TunnelProtocol)
		return
	}
	if n.Suspended {
		web.APIError(w, http.StatusForbidden, "the organization is suspended")
		return
	}
	if s.Tunnels.IdleCount(n.ID) >= web.MaxIdleTunnels {
		web.APIError(w, http.StatusTooManyRequests, "enough tunnel connections are open")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		web.APIError(w, http.StatusInternalServerError, "tunnels are not supported here")
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
	s.Tunnels.Put(n.ID, conn)
}

// tunnelRoutes is the internal tunnel listener: only the proxy reaches it.
func (s *API) TunnelRoutes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dep := r.Header.Get(web.TunnelHeader)
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
				req.Header.Del(web.TunnelHeader)
				if _, ok := req.Header["User-Agent"]; !ok {
					req.Header.Set("User-Agent", "")
				}
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return s.Tunnels.Open(ctx, node, dep)
				},
				DisableKeepAlives:     true, // one stream per connection
				ResponseHeaderTimeout: 5 * time.Minute,
				DisableCompression:    true,
			},
			FlushInterval: -1, // stream responses (server-sent events, downloads) as they come
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				s.Log.Warn("tunnel", "deployment", dep, "node", node, "err", err)
				http.Error(w, "The server running this application is not reachable right now.", http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}

func (s *API) tunnelNode(ctx context.Context, dep string) (string, error) {
	s.Tunnels.Mu.Lock()
	rt, ok := s.Tunnels.Routes[dep]
	s.Tunnels.Mu.Unlock()
	if ok && time.Now().Before(rt.Expires) {
		return rt.Node, nil
	}
	var node, org string
	if err := s.Pool.QueryRow(ctx, `SELECT node_id, org_id FROM customer_deployment_node($1)`, dep).Scan(&node, &org); err != nil {
		return "", err
	}
	s.Tunnels.Mu.Lock()
	if len(s.Tunnels.Routes) > 10000 {
		s.Tunnels.Routes = map[string]web.TunnelRoute{}
	}
	s.Tunnels.Routes[dep] = web.TunnelRoute{Node: node, Expires: time.Now().Add(10 * time.Second)}
	s.Tunnels.Mu.Unlock()
	return node, nil
}

// Package api starts the internet-facing console and API. It has no access to Docker or
// the host: it only reads and writes desired state in PostgreSQL, scoped by RLS.
//
// Sessions, rendering, authorization and the code features share are in
// internal/core/web; each feature's pages and endpoints are in
// internal/components/<feature>/api. This package only wires them together.
package api

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	auth "platform/internal/components/auth/api"
	backups "platform/internal/components/backups/api"
	billing "platform/internal/components/billing/api"
	builds "platform/internal/components/builds/api"
	databases "platform/internal/components/databases/api"
	dns "platform/internal/components/dns/api"
	docs "platform/internal/components/docs/api"
	email "platform/internal/components/email/api"
	functions "platform/internal/components/functions/api"
	monitoring "platform/internal/components/monitoring/api"
	operator "platform/internal/components/operator/api"
	orgs "platform/internal/components/orgs/api"
	projects "platform/internal/components/projects/api"
	queues "platform/internal/components/queues/api"
	registry "platform/internal/components/registry/api"
	restapi "platform/internal/components/restapi/api"
	secrets "platform/internal/components/secrets/api"
	servers "platform/internal/components/servers/api"
	services "platform/internal/components/services/api"
	storage "platform/internal/components/storage/api"
	"platform/internal/core/web"
	"platform/internal/lib/config"
	"platform/internal/lib/db"
	regkeys "platform/internal/lib/registry"
	"platform/internal/lib/secure"
)

// component is the part of a feature served by the API.
type component interface {
	// Routes registers the component's pages and API endpoints on the public listener.
	Routes(m *http.ServeMux)
}

// internalRouter: a component with endpoints on the internal listener (never published).
type internalRouter interface {
	InternalRoutes(m *http.ServeMux)
}

// New returns the console's handlers: the public, internal and tunnel listeners.
func New(s *web.Server) (public, internal, tunnel http.Handler) {
	fns := &functions.API{Server: s}
	srv := &servers.API{Server: s}
	comps := []component{
		&auth.API{Server: s}, &orgs.API{Server: s}, &projects.API{Server: s}, &services.API{Server: s},
		&builds.API{Server: s}, srv, &databases.API{Server: s}, &backups.API{Server: s},
		&storage.API{Server: s}, fns, &queues.API{Server: s}, &secrets.API{Server: s},
		&dns.API{Server: s}, &email.API{Server: s}, &monitoring.API{Server: s}, &billing.API{Server: s},
		&operator.API{Server: s}, &docs.API{Server: s}, &registry.API{Server: s}, &restapi.API{Server: s},
	}
	pm := http.NewServeMux()
	s.CoreRoutes(pm)
	im := http.NewServeMux()
	s.CoreInternalRoutes(im)
	for _, c := range comps {
		c.Routes(pm)
		if ir, ok := c.(internalRouter); ok {
			ir.InternalRoutes(im)
		}
	}
	return fns.FunctionHosts(s.SecureHeaders(pm)), im, srv.TunnelRoutes()
}

// Templates are the pages of the components that have some (the core adds its own).
func Templates() []fs.FS {
	return []fs.FS{
		auth.Templates, orgs.Templates, projects.Templates, services.Templates, builds.Templates,
		servers.Templates, databases.Templates, storage.Templates, functions.Templates, queues.Templates,
		secrets.Templates, dns.Templates, email.Templates, monitoring.Templates, billing.Templates,
		operator.Templates, docs.Templates,
	}
}

func Run(ctx context.Context, cfg config.Config) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	box, err := secure.LoadBox(cfg.MasterKeyFile)
	if err != nil {
		return err
	}
	pages, err := web.ParseTemplates(Templates()...)
	if err != nil {
		return err
	}
	var regKey *ecdsa.PrivateKey
	if cfg.RegistryHost != "" {
		if regKey, err = regkeys.LoadKey(cfg.RegistryTokenKeyFile); err != nil {
			return fmt.Errorf("registry token key: %w", err)
		}
	}
	site, err := docs.LoadDocs(cfg)
	if err != nil {
		return fmt.Errorf("load docs: %w", err)
	}
	wa, err := auth.NewWebAuthn(cfg)
	if err != nil {
		return fmt.Errorf("passkeys: %w", err)
	}
	dummy, _ := secure.HashPassword(secure.NewToken())
	s := &web.Server{
		Cfg: cfg, Pool: pool, Box: box, Pages: pages,
		Limiter: web.NewRateLimiter(), Log: slog.Default().With("component", "api"),
		DummyHash: dummy, HTTP: &http.Client{Timeout: 10 * time.Second}, RegKey: regKey,
		AssetV: web.AssetVersion(), Docs: site,
		WebAuthn: wa, Ceremonies: &web.CeremonyStore{M: map[string]web.Ceremony{}}, Pow: auth.NewPowStore(),
		Tunnels:   servers.NewTunnelPool(),
		BuildWake: servers.NewBuildWake(),
	}
	pubH, intH, tunH := New(s)
	public := &http.Server{
		Addr: cfg.ListenAddr, Handler: pubH,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	internal := &http.Server{Addr: cfg.InternalAddr, Handler: intH, ReadHeaderTimeout: 5 * time.Second}
	// Requests for applications on customer servers, from the proxy only (never published).
	tunnel := &http.Server{Addr: cfg.TunnelAddr, Handler: tunH, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}

	go s.Housekeeping(ctx)
	go (&functions.API{Server: s}).ListenFnDone(ctx)
	go (&email.API{Server: s}).ServeSMTP(ctx)
	errc := make(chan error, 3)
	go func() { errc <- public.ListenAndServe() }()
	go func() { errc <- internal.ListenAndServe() }()
	go func() { errc <- tunnel.ListenAndServe() }()
	s.Log.Info("listening", "public", cfg.ListenAddr, "internal", cfg.InternalAddr)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = public.Shutdown(sctx)
	_ = internal.Shutdown(sctx)
	_ = tunnel.Shutdown(sctx)
	return nil
}

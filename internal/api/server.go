// Package api is the internet-facing console and API. It has no access to Docker or
// the host: it only reads and writes desired state in PostgreSQL, scoped by RLS.
package api

import (
	"github.com/go-webauthn/webauthn/webauthn"

	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"platform/internal/billing"
	"platform/internal/config"
	"platform/internal/db"
	"platform/internal/registry"
	"platform/internal/secure"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg        config.Config
	pool       *pgxpool.Pool
	box        *secure.Box
	pages      map[string]*template.Template
	limiter    *rateLimiter
	log        *slog.Logger
	dummyHash  string
	hc         *http.Client
	regKey     *ecdsa.PrivateKey // signs registry tokens; nil when the registry is disabled
	assetV     string            // content hash of static files, for cache busting
	docs       map[string]docPage
	wa         *webauthn.WebAuthn
	ceremonies *ceremonyStore
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
	pages, err := parseTemplates()
	if err != nil {
		return err
	}
	var regKey *ecdsa.PrivateKey
	if cfg.RegistryHost != "" {
		if regKey, err = registry.LoadKey(cfg.RegistryTokenKeyFile); err != nil {
			return fmt.Errorf("registry token key: %w", err)
		}
	}
	docs, err := loadDocs(cfg)
	if err != nil {
		return fmt.Errorf("load docs: %w", err)
	}
	wa, err := newWebAuthn(cfg)
	if err != nil {
		return fmt.Errorf("passkeys: %w", err)
	}
	dummy, _ := secure.HashPassword(secure.NewToken())
	s := &Server{
		cfg: cfg, pool: pool, box: box, pages: pages,
		limiter: newRateLimiter(), log: slog.Default().With("component", "api"),
		dummyHash: dummy, hc: &http.Client{Timeout: 10 * time.Second}, regKey: regKey,
		assetV: assetVersion(), docs: docs,
		wa: wa, ceremonies: &ceremonyStore{m: map[string]ceremony{}},
	}

	public := &http.Server{
		Addr: cfg.ListenAddr, Handler: s.routes(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	internal := &http.Server{Addr: cfg.InternalAddr, Handler: s.internalRoutes(), ReadHeaderTimeout: 5 * time.Second}

	go s.housekeeping(ctx)
	errc := make(chan error, 2)
	go func() { errc <- public.ListenAndServe() }()
	go func() { errc <- internal.ListenAndServe() }()
	s.log.Info("listening", "public", cfg.ListenAddr, "internal", cfg.InternalAddr)
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
	return nil
}

func (s *Server) housekeeping(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() OR last_seen < now() - interval '2 hours'`)
			s.limiter.sweep()
		}
	}
}

func (s *Server) routes() http.Handler {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	m.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/docs/user", http.StatusSeeOther) })
	m.HandleFunc("GET /docs/{guide}", s.docsPage)
	m.HandleFunc("GET /status/{org}/{project}", s.statusPage)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.loginSubmit)
	m.HandleFunc("POST /logout", s.auth(authAny, s.logout))
	m.HandleFunc("GET /totp/setup", s.auth(authPassword, s.totpSetupPage))
	m.HandleFunc("POST /totp/setup", s.auth(authPassword, s.totpSetupSubmit))
	m.HandleFunc("GET /totp/verify", s.auth(authPassword, s.totpVerifyPage))
	m.HandleFunc("POST /totp/verify", s.auth(authPassword, s.totpVerifySubmit))
	m.HandleFunc("GET /invite/{token}", s.invitePage)
	m.HandleFunc("POST /invite/{token}", s.inviteSubmit)

	m.HandleFunc("GET /{$}", s.auth(authFull, s.home))
	m.HandleFunc("GET /o/{org}", s.auth(authFull, s.orgPage))
	m.HandleFunc("GET /o/{org}/audit", s.auth(authFull, s.auditPage))
	m.HandleFunc("GET /o/{org}/billing", s.auth(authFull, s.billingPage))
	m.HandleFunc("POST /o/{org}/billing/details", s.auth(authFull, s.billingDetails))
	m.HandleFunc("GET /o/{org}/invoices/{number}", s.auth(authFull, s.invoicePage))
	m.HandleFunc("POST /o/{org}/projects", s.auth(authFull, s.createProject))
	m.HandleFunc("POST /o/{org}/invites", s.auth(authFull, s.createInvite))
	m.HandleFunc("POST /o/{org}/members/{user}/remove", s.auth(authFull, s.removeMember))
	m.HandleFunc("POST /o/{org}/registry-tokens", s.auth(authFull, s.createRegistryToken))
	m.HandleFunc("POST /o/{org}/api-tokens", s.auth(authFull, s.createAPIToken))
	m.HandleFunc("POST /o/{org}/api-tokens/{id}/delete", s.auth(authFull, s.deleteAPIToken))
	m.HandleFunc("POST /o/{org}/alerts/channels", s.auth(authFull, s.createChannel))
	m.HandleFunc("POST /o/{org}/alerts/channels/{id}/delete", s.auth(authFull, s.deleteChannel))
	m.HandleFunc("POST /o/{org}/alerts/channels/{id}/test", s.auth(authFull, s.testChannel))
	m.HandleFunc("POST /o/{org}/registry-tokens/{id}/delete", s.auth(authFull, s.deleteRegistryToken))

	// Registry token endpoint (Docker registry auth). Caddy exposes it on the registry host.
	m.HandleFunc("GET /token", s.registryToken)

	m.HandleFunc("GET /o/{org}/p/{project}", s.auth(authFull, s.projectPage))
	m.HandleFunc("POST /o/{org}/p/{project}/services", s.auth(authFull, s.createService))
	m.HandleFunc("POST /o/{org}/p/{project}/delete", s.auth(authFull, s.deleteProject))
	m.HandleFunc("POST /o/{org}/p/{project}/status-page", s.auth(authFull, s.toggleStatusPage))

	m.HandleFunc("POST /o/{org}/p/{project}/databases", s.auth(authFull, s.createDatabase))
	const dbp = "/o/{org}/p/{project}/d/{db}"
	m.HandleFunc("GET "+dbp, s.auth(authFull, s.databasePage))
	m.HandleFunc("POST "+dbp+"/settings", s.auth(authFull, s.databaseSettings))
	m.HandleFunc("POST "+dbp+"/attach", s.auth(authFull, s.attachDatabase))
	m.HandleFunc("POST "+dbp+"/backups", s.auth(authFull, s.databaseBackupNow))
	m.HandleFunc("POST "+dbp+"/backups/restore/{snap}", s.auth(authFull, s.databaseRestore))
	m.HandleFunc("POST "+dbp+"/delete", s.auth(authFull, s.deleteDatabase))

	const svc = "/o/{org}/p/{project}/s/{service}"
	m.HandleFunc("GET "+svc, s.auth(authFull, s.servicePage))
	m.HandleFunc("POST "+svc+"/settings", s.auth(authFull, s.saveSettings))
	m.HandleFunc("POST "+svc+"/deploy", s.auth(authFull, s.deployService))
	m.HandleFunc("POST "+svc+"/rollback/{rev}", s.auth(authFull, s.rollback))
	m.HandleFunc("POST "+svc+"/secrets", s.auth(authFull, s.setSecret))
	m.HandleFunc("POST "+svc+"/secrets/{name}/delete", s.auth(authFull, s.deleteSecret))
	m.HandleFunc("POST "+svc+"/domains", s.auth(authFull, s.addDomain))
	m.HandleFunc("POST "+svc+"/domains/{id}/verify", s.auth(authFull, s.verifyDomain))
	m.HandleFunc("POST "+svc+"/domains/{id}/delete", s.auth(authFull, s.deleteDomain))
	m.HandleFunc("POST "+svc+"/delete", s.auth(authFull, s.deleteService))
	m.HandleFunc("POST "+svc+"/backups", s.auth(authFull, s.backupNow))
	m.HandleFunc("POST "+svc+"/alerts", s.auth(authFull, s.alertThresholds))
	m.HandleFunc("POST "+svc+"/protection", s.auth(authFull, s.protectionSettings))
	m.HandleFunc("POST "+svc+"/backups/settings", s.auth(authFull, s.backupSettings))
	m.HandleFunc("POST "+svc+"/backups/restore/{snap}", s.auth(authFull, s.restoreSnapshot))
	m.HandleFunc("GET "+svc+"/status.json", s.auth(authFull, s.statusJSON))
	m.HandleFunc("GET "+svc+"/logs.json", s.auth(authFull, s.logsJSON))
	m.HandleFunc("GET "+svc+"/metrics.json", s.auth(authFull, s.metricsJSON))

	s.apiRoutes(m)
	s.passkeyRoutes(m)
	return s.secureHeaders(m)
}

func assetVersion() string {
	h := sha256.New()
	for _, f := range []string{"static/app.css", "static/app.js"} {
		b, _ := staticFS.ReadFile(f)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- sessions ----------

type session struct {
	TokenHash   []byte
	UserID      string
	Email       string
	Name        string
	CSRF        string
	TOTPOK      bool
	TOTPEnabled bool
}

type ctxKey int

const sessKey ctxKey = 1

const (
	authAny = iota
	authPassword
	authFull
)

func (s *Server) cookieName() string {
	if s.cfg.CookieSecure {
		return "__Host-sid"
	}
	return "sid"
}

func (s *Server) loadSession(r *http.Request) *session {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil
	}
	sess := &session{TokenHash: secure.HashToken(c.Value)}
	err = s.pool.QueryRow(r.Context(), `
		UPDATE sessions se SET last_seen = now()
		FROM users u
		WHERE se.token_hash = $1 AND u.id = se.user_id
		  AND se.expires_at > now() AND se.last_seen > now() - interval '2 hours'
		RETURNING se.user_id, u.email, u.name, se.csrf, se.totp_ok, u.totp_enabled`, sess.TokenHash).
		Scan(&sess.UserID, &sess.Email, &sess.Name, &sess.CSRF, &sess.TOTPOK, &sess.TOTPEnabled)
	if err != nil {
		return nil
	}
	return sess
}

func sessionFrom(r *http.Request) *session {
	s, _ := r.Context().Value(sessKey).(*session)
	return s
}

func (s *Server) auth(level int, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.loadSession(r)
		if sess == nil {
			if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, ".json") {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "session expired, please log in again", http.StatusUnauthorized)
			}
			return
		}
		if level == authFull && !sess.TOTPOK {
			if sess.TOTPEnabled {
				http.Redirect(w, r, "/totp/verify", http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
			}
			return
		}
		if r.Method == http.MethodPost && !s.validCSRF(r, sess) {
			http.Error(w, "invalid or missing CSRF token: reload the page and try again", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), sessKey, sess)))
	}
}

func (s *Server) validCSRF(r *http.Request, sess *session) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" && o != s.cfg.BaseURL {
		return false
	}
	tok := r.Header.Get("X-CSRF-Token") // JSON requests (passkeys)
	if tok == "" {
		tok = r.PostFormValue("csrf")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRF)) == 1
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string, totpOK bool) error {
	token := secure.NewToken()
	_, err := s.pool.Exec(r.Context(), `
		INSERT INTO sessions (token_hash, user_id, csrf, totp_ok, ip, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + interval '12 hours')`,
		secure.HashToken(token), userID, secure.NewToken(), totpOK, clientIP(r))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", MaxAge: 12 * 3600,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
}

// clientIP: the API is only reachable through Caddy, which sets X-Forwarded-For to the
// real client address (it does not trust incoming values), so the last entry is used.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- rendering ----------

func parseTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"ago": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return pluralize(int(d.Minutes()), "minute") + " ago"
			case d < 48*time.Hour:
				return pluralize(int(d.Hours()), "hour") + " ago"
			default:
				return t.Format("2 Jan 2006")
			}
		},
		"datetime":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
		"roleAtLeast": func(role, min string) bool { return roleAtLeast(role, min) },
		"money":       func(cents int64, cur string) string { return billing.Money(cents, cur) },
		"qty":         func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) },
		"unitprice":   func(c float64) string { return strconv.FormatFloat(c/100, 'f', -1, 64) },
		"lines":       func(s string) []string { return strings.Split(s, "\n") },
		"month":       func(t time.Time) string { return t.Format("January 2006") },
		"day":         func(t time.Time) string { return t.Format("2 Jan 2006") },
		"dbStatus": func(s string) string {
			if s == "running" {
				return "live"
			}
			return s
		},
		"hours": func() []int {
			h := make([]int, 24)
			for i := range h {
				h[i] = i
			}
			return h
		},
	}
	pages := map[string]*template.Template{}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		page := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if page == "layout" {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n)
		if err != nil {
			return nil, err
		}
		pages[page] = t
	}
	return pages, nil
}

func pluralize(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strings.TrimSpace(strings.Join([]string{itoa(n), word + "s"}, " "))
}

type data map[string]any

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, d data) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "page not found", http.StatusInternalServerError)
		return
	}
	if d == nil {
		d = data{}
	}
	d["V"] = s.assetV
	s.addNav(r, d)
	if sess := sessionFrom(r); sess != nil {
		d["User"] = sess
		d["CSRF"] = sess.CSRF
	}
	if _, ok := d["Msg"]; !ok {
		d["Msg"] = r.URL.Query().Get("msg")
	}
	if _, ok := d["Err"]; !ok {
		d["Err"] = r.URL.Query().Get("err")
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", d); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// back redirects with a flash message (msg) or error (errMsg).
func back(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	frag := ""
	if i := strings.Index(path, "#"); i >= 0 {
		path, frag = path[:i], path[i:]
	}
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	http.Redirect(w, r, path+frag, http.StatusSeeOther)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "error", http.StatusNotFound, data{"Title": "Not found", "Text": "This page does not exist, or you do not have access to it."})
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "error", http.StatusForbidden, data{"Title": "Not allowed", "Text": "Your role in this organization does not allow this action."})
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	s.render(w, r, "error", http.StatusInternalServerError, data{"Title": "Something went wrong", "Text": "The error was logged. Please try again."})
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ---------- rate limiting (fixed window, in memory) ----------

type window struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	mu sync.Mutex
	m  map[string]*window
}

func newRateLimiter() *rateLimiter { return &rateLimiter{m: map[string]*window{}} }

func (l *rateLimiter) allow(key string, limit int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.m[key]
	if !ok || now.After(w.reset) {
		w = &window{reset: now.Add(per)}
		l.m[key] = w
	}
	w.count++
	return w.count <= limit
}

func (l *rateLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, w := range l.m {
		if now.After(w.reset) {
			delete(l.m, k)
		}
	}
}

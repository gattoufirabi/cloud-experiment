// Package web is the core of the console and API: the server, sessions and
// authorization, page rendering and the layout, the REST API's token checks, and the
// code several components share. It has no access to Docker or the host: it only reads
// and writes desired state in PostgreSQL, scoped by RLS.
//
// Each feature's pages and endpoints are in internal/components/<feature>/api, as a
// type that embeds *Server; internal/api wires them together. Files here are named
// after the feature they serve: they hold the parts more than one component uses.
package web

import (
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

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"platform/internal/lib/billing"
	"platform/internal/lib/config"
	"platform/internal/lib/oidc"
	"platform/internal/lib/secure"
)

//go:embed templates/*.html
var TemplateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	Cfg           config.Config
	Pool          *pgxpool.Pool
	Box           *secure.Box
	Pages         map[string]*template.Template
	Limiter       *rateLimiter
	Log           *slog.Logger
	DummyHash     string
	HTTP          *http.Client
	RegKey        *ecdsa.PrivateKey // signs registry tokens; nil when the registry is disabled
	AssetV        string            // content hash of static files, for cache busting
	Docs          *DocsSite
	WebAuthn      *webauthn.WebAuthn
	settingsCache settingsCache
	Ceremonies    *CeremonyStore
	Pow           *PowStore
	FnWait        fnWaiters // requests waiting for a function's result
	OIDC          *oidc.Client
	OIDCOnce      sync.Once
	BuildWake     *BuildWake  // wakes build servers waiting for a build (serverbuilds.go)
	Tunnels       *TunnelPool // idle tunnel connections of customer servers
}

func (s *Server) Housekeeping(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.Pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() OR last_seen < now() - interval '2 hours'`)
			s.Limiter.sweep()
		}
	}
}

func AssetVersion() string {
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

func (s *Server) SecureHeaders(next http.Handler) http.Handler {
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
		if s.Cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if r.Method == http.MethodPost {
			limit := int64(1 << 20)
			switch {
			case strings.HasSuffix(r.URL.Path, "/objects/upload"): // console uploads to a bucket
				limit = MaxUploadBytes + 1<<20
			case strings.HasSuffix(r.URL.Path, "/code"): // function code archive
				limit = 11 << 20
			case strings.HasSuffix(r.URL.Path, "/invoke"): // function event
				limit = 7 << 20
			case strings.HasSuffix(r.URL.Path, "/zip"): // zip deployment
				limit = MaxZipDeployBytes + 1<<20
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- sessions ----------

type Session struct {
	TokenHash   []byte
	UserID      string
	Email       string
	Name        string
	CSRF        string
	TOTPOK      bool
	TOTPEnabled bool
	IsOperator  bool
}

type ctxKey int

const SessKey ctxKey = 1

const (
	AuthAny = iota
	AuthPassword
	AuthFull
)

func (s *Server) cookieName() string {
	if s.Cfg.CookieSecure {
		return "__Host-sid"
	}
	return "sid"
}

func (s *Server) LoadSession(r *http.Request) *Session {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil
	}
	sess := &Session{TokenHash: secure.HashToken(c.Value)}
	err = s.Pool.QueryRow(r.Context(), `
		UPDATE sessions se SET last_seen = now()
		FROM users u
		WHERE se.token_hash = $1 AND u.id = se.user_id AND u.disabled_at IS NULL
		  AND se.expires_at > now() AND se.last_seen > now() - interval '2 hours'
		RETURNING se.user_id, u.email, u.name, se.csrf, se.totp_ok, u.totp_enabled, u.is_operator`, sess.TokenHash).
		Scan(&sess.UserID, &sess.Email, &sess.Name, &sess.CSRF, &sess.TOTPOK, &sess.TOTPEnabled, &sess.IsOperator)
	if err != nil {
		return nil
	}
	return sess
}

func SessionFrom(r *http.Request) *Session {
	s, _ := r.Context().Value(SessKey).(*Session)
	return s
}

func (s *Server) Auth(level int, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.LoadSession(r)
		if sess == nil {
			if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, ".json") {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "session expired, please log in again", http.StatusUnauthorized)
			}
			return
		}
		if level == AuthFull && !sess.TOTPOK {
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
		h(w, r.WithContext(context.WithValue(r.Context(), SessKey, sess)))
	}
}

func (s *Server) validCSRF(r *http.Request, sess *Session) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" && o != s.Cfg.BaseURL {
		return false
	}
	tok := r.Header.Get("X-CSRF-Token") // JSON requests (passkeys)
	if tok == "" {
		tok = r.PostFormValue("csrf")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRF)) == 1
}

func (s *Server) StartSession(w http.ResponseWriter, r *http.Request, userID string, totpOK bool) error {
	token := secure.NewToken()
	// A disabled account gets no session, whatever the way it signed in.
	tag, err := s.Pool.Exec(r.Context(), `
		INSERT INTO sessions (token_hash, user_id, csrf, totp_ok, ip, expires_at)
		SELECT $1, id, $3, $4, $5, now() + interval '12 hours' FROM users WHERE id = $2 AND disabled_at IS NULL`,
		secure.HashToken(token), userID, secure.NewToken(), totpOK, ClientIP(r))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAccountDisabled
	}
	_, _ = s.Pool.Exec(r.Context(), `UPDATE users SET last_login_at = now() WHERE id = $1`, userID)
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", MaxAge: 12 * 3600,
		HttpOnly: true, Secure: s.Cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

var ErrAccountDisabled = errors.New("account disabled")

// sessionError answers a failed startSession: a clear message for a disabled account.
func (s *Server) SessionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrAccountDisabled) {
		s.Render(w, r, "login", http.StatusForbidden, Data{"Err": "This account is disabled. Contact the platform's support."})
		return
	}
	s.ServerError(w, r, err)
}

func (s *Server) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.Cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
}

// clientIP: the API is only reachable through Caddy, which sets X-Forwarded-For to the
// real client address (it does not trust incoming values), so the last entry is used.
func ClientIP(r *http.Request) string {
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

// ParseTemplates parses the core's pages and those of the components (each an FS with
// templates/*.html); every page is rendered inside the core's layout.
func ParseTemplates(components ...fs.FS) (map[string]*template.Template, error) {
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
		"buildStatus": func(s string) string {
			switch s {
			case "done":
				return "live"
			case "running":
				return "deploying"
			case "queued":
				return "pending"
			}
			return s
		},
		"jobStatus":  func(s string) string { return s }, // succeeded, failed, timed_out, skipped, running
		"engineName": EngineName,
		"bytes":      humanBytes,
		"icon":       icon,
		"trimSuffix": strings.TrimSuffix,
		"runtime":    runtimeLabel,
		"list":       func(xs ...int) []int { return xs },
		"contains":   strings.Contains,
		"join":       strings.Join,
		"strs":       func(xs ...string) []string { return xs },
		"pct":        percentOf,
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"hoursOf": func(sec int) int { return max(1, sec/3600) },
		"int64":   func(i int) int64 { return int64(i) },
		"ms":      itoaPtr,
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
	layout, err := TemplateFS.ReadFile("templates/layout.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, src := range append([]fs.FS{TemplateFS}, components...) {
		names, err := fs.Glob(src, "templates/*.html")
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			page := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
			if page == "layout" {
				continue
			}
			if _, dup := pages[page]; dup {
				return nil, fmt.Errorf("two components define the page %q", page)
			}
			t := template.New("").Funcs(funcs)
			if _, err := t.New("layout.html").Parse(string(layout)); err != nil {
				return nil, err
			}
			if _, err := t.ParseFS(src, n); err != nil {
				return nil, err
			}
			pages[page] = t
		}
	}
	return pages, nil
}

func pluralize(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strings.TrimSpace(strings.Join([]string{Itoa(n), word + "s"}, " "))
}

type Data map[string]any

// viewKey marks a request for the settings page of a resource (its settings tab, on a page of its own).
type viewKey struct{}

// settingsPanes are the tabs that open on their own page.
var settingsPanes = map[string]bool{"#settings": true, "#db-settings": true, "#project-settings": true, "#configuration": true}

// setTheme remembers light, dark or "system" (follow the device) in a cookie and goes back.
func (s *Server) setTheme(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("set")
	c := &http.Cookie{Name: "theme", Value: v, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.Cfg.CookieSecure}
	if v != "light" && v != "dark" {
		c.Value, c.MaxAge = "", -1
	}
	http.SetCookie(w, c)
	to := r.URL.Query().Get("back")
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.Contains(to, "\\") {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func SettingsView(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), viewKey{}, "settings")))
	}
}

func (s *Server) Render(w http.ResponseWriter, r *http.Request, page string, status int, d Data) {
	t, ok := s.Pages[page]
	if !ok {
		http.Error(w, "page not found", http.StatusInternalServerError)
		return
	}
	if d == nil {
		d = Data{}
	}
	d["V"] = s.AssetV
	d["Product"] = s.Cfg.ProductName
	// pages whose settings open on their own page (…/settings)
	d["PagePath"] = strings.TrimSuffix(r.URL.Path, "/settings")
	if v, _ := r.Context().Value(viewKey{}).(string); v != "" {
		d["View"] = v
	}
	if c, err := r.Cookie("theme"); err == nil && (c.Value == "light" || c.Value == "dark") {
		d["Theme"] = c.Value
	}
	d["Here"] = r.URL.RequestURI()
	ps := s.Settings(r.Context())
	d["SignupEnabled"] = ps.SignupEnabled
	if ps.BannerMessage != "" {
		d["Banner"], d["BannerLevel"] = ps.BannerMessage, ps.BannerLevel
	}
	s.addNav(r, d)
	if sess := SessionFrom(r); sess != nil {
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
		s.Log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// renderPartial renders one shared template (defined in layout.html) without the page
// around it: the HTML fragments htmx swaps in place.
func (s *Server) RenderPartial(w http.ResponseWriter, r *http.Request, name string, d any) {
	t, ok := s.Pages["project"]
	if !ok {
		http.Error(w, "page not found", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, d); err != nil {
		s.Log.Error("render partial", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// back redirects with a flash message (msg) or error (errMsg).
func Back(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	frag := ""
	if i := strings.Index(path, "#"); i >= 0 {
		path, frag = path[:i], path[i:]
	}
	if settingsPanes[frag] && strings.HasPrefix(path, "/o/") && !strings.HasSuffix(path, "/settings") {
		path += "/settings" // these tabs are pages of their own
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

func (s *Server) NotFound(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, "error", http.StatusNotFound, Data{"Title": "Not found", "Text": "This page does not exist, or you do not have access to it."})
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, "error", http.StatusForbidden, Data{"Title": "Not allowed", "Text": "Your role in this organization does not allow this action."})
}

func (s *Server) ServerError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "path", r.URL.Path, "err", err)
	s.Render(w, r, "error", http.StatusInternalServerError, Data{"Title": "Something went wrong", "Text": "The error was logged. Please try again."})
}

func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ---------- rate limiting (fixed window, in memory) ----------

type window struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	mu sync.Mutex
	m  map[string]*window
}

func NewRateLimiter() *rateLimiter { return &rateLimiter{m: map[string]*window{}} }

func (l *rateLimiter) Allow(key string, limit int, per time.Duration) bool {
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

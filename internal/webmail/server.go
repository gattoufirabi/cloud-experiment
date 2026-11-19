// Package webmail is the platform's webmail: anyone with a mailbox hosted on the
// platform signs in with their address and password, outside the console. It talks to
// the mail server over JMAP with the person's own credentials (kept in memory for the
// session only), so it has no database and no privilege of its own.
package webmail

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Config of the webmail.
type Config struct {
	Listen       string // ":8090"
	MailURL      string // the mail server's HTTP (JMAP), such as http://stalwart:8080
	CookieSecure bool
	ConsoleURL   string // shown on the sign-in page ("" hides it)
	ProductName  string // the name people see ("Staysa Cloud")
}

const (
	sessionTTL  = 12 * time.Hour
	idleTTL     = 2 * time.Hour
	pageSize    = 50
	maxUploadMB = 20
)

type session struct {
	c                   *jmapClient
	Email, Name, csrf   string
	created, lastActive time.Time
}

// Server is the webmail HTTP server.
type Server struct {
	cfg      Config
	log      *slog.Logger
	pages    map[string]*template.Template
	mu       sync.Mutex
	sessions map[string]*session
	fails    map[string][]time.Time // sign-in failures by client address and by account
}

func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// New prepares the server.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, log: log, pages: map[string]*template.Template{}, sessions: map[string]*session{}, fails: map[string][]time.Time{}}
	funcs := template.FuncMap{
		"when": when, "size": humanSize, "add": func(a, b int) int { return a + b },
		"initial": func(a address) string {
			n := strings.TrimSpace(a.Short())
			if n == "" {
				return "?"
			}
			return strings.ToUpper(string([]rune(n)[:1]))
		},
	}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if strings.HasSuffix(n, "/layout.html") {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n)
		if err != nil {
			return nil, err
		}
		s.pages[strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")] = t
	}
	return s, nil
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	st, _ := fs.Sub(staticFS, "static")
	m := http.NewServeMux()
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(http.FileServerFS(st))))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	m.HandleFunc("GET /{$}", s.home)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.login)
	m.HandleFunc("POST /logout", s.withSession(s.logout))
	m.HandleFunc("GET /theme", s.setTheme)
	m.HandleFunc("GET /f/{folder}", s.withSession(s.folderPage))
	m.HandleFunc("GET /search", s.withSession(s.searchPage))
	m.HandleFunc("GET /m/{id}", s.withSession(s.messagePage))
	m.HandleFunc("GET /m/{id}/body", s.withSession(s.messageBody))
	m.HandleFunc("GET /m/{id}/file/{blob}", s.withSession(s.file))
	m.HandleFunc("POST /m/{id}", s.withSession(s.messageAction))
	m.HandleFunc("POST /bulk", s.withSession(s.bulkAction))
	m.HandleFunc("GET /compose", s.withSession(s.composePage))
	m.HandleFunc("POST /compose", s.withSession(s.sendMessage))
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.secure(m), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
	go s.sweep(ctx)
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	s.log.Info("webmail listening", "addr", s.cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// secure adds the security headers of every answer.
func (s *Server) secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if !strings.HasSuffix(r.URL.Path, "/body") {
			hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; frame-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
			hd.Set("X-Frame-Options", "DENY")
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			hd.Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}

// sweep forgets expired sessions and old sign-in failures.
func (s *Server) sweep(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		for k, se := range s.sessions {
			if time.Since(se.created) > sessionTTL || time.Since(se.lastActive) > idleTTL {
				delete(s.sessions, k)
			}
		}
		for k, v := range s.fails {
			if len(v) == 0 || time.Since(v[len(v)-1]) > 30*time.Minute {
				delete(s.fails, k)
			}
		}
		s.mu.Unlock()
	}
}

// clientIP is the visitor's address as the proxy in front of the webmail saw it.
func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		parts := strings.Split(xf, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

// ---------- sessions ----------

const cookieName = "wm_session"

func (s *Server) current(r *http.Request) *session {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.sessions[c.Value]
	if se == nil || time.Since(se.created) > sessionTTL || time.Since(se.lastActive) > idleTTL {
		delete(s.sessions, c.Value)
		return nil
	}
	se.lastActive = time.Now()
	return se
}

type sessKey struct{}

func sessionFrom(r *http.Request) *session {
	se, _ := r.Context().Value(sessKey{}).(*session)
	return se
}

// withSession requires a signed-in person (and a valid form token on POST).
func (s *Server) withSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		se := s.current(r)
		if se == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseMultipartForm(maxUploadMB << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
				s.fail(w, r, http.StatusBadRequest, "The form could not be read (attachments: "+strconv.Itoa(maxUploadMB)+" MB at most).")
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(se.csrf)) != 1 {
				s.fail(w, r, http.StatusForbidden, "This form expired: go back, reload the page and try again.")
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), sessKey{}, se)))
	}
}

// tooMany reports whether key failed to sign in too often recently.
func (s *Server) tooMany(key string, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keep []time.Time
	for _, t := range s.fails[key] {
		if time.Since(t) < 15*time.Minute {
			keep = append(keep, t)
		}
	}
	s.fails[key] = keep
	return len(keep) >= max
}

func (s *Server) noteFail(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		s.fails[k] = append(s.fails[k], time.Now())
	}
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if s.current(r) != nil {
		http.Redirect(w, r, "/f/inbox", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.current(r) != nil {
		http.Redirect(w, r, "/f/inbox", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", http.StatusOK, data{"Next": safeNext(r.URL.Query().Get("next")), "Email": r.URL.Query().Get("email")})
}

func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.Contains(n, `\`) {
		return ""
	}
	return n
}

var emailRe = regexp.MustCompile(`^[^@\s]{1,64}@[a-z0-9.-]{1,253}$`)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	pass := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	again := func(status int, msg string) {
		s.render(w, r, "login", status, data{"Err": msg, "Email": email, "Next": next})
	}
	if !emailRe.MatchString(email) || pass == "" || len(pass) > 1024 {
		again(http.StatusUnprocessableEntity, "Enter your full email address and your password.")
		return
	}
	ip := clientIP(r)
	if s.tooMany("ip:"+ip, 10) || s.tooMany("acct:"+email, 5) {
		again(http.StatusTooManyRequests, "Too many attempts. Wait 15 minutes, then try again.")
		return
	}
	c, err := connect(r.Context(), s.cfg.MailURL, email, pass)
	if err != nil {
		if errors.Is(err, errAuth) {
			s.noteFail("ip:"+ip, "acct:"+email)
			again(http.StatusUnauthorized, "Wrong email address or password.")
			return
		}
		s.log.Warn("webmail sign-in", "err", err)
		again(http.StatusBadGateway, "The mail server is not answering. Try again in a minute.")
		return
	}
	name := email
	if ids, err := c.identities(r.Context()); err == nil && len(ids) > 0 && ids[0].Name != "" {
		name = ids[0].Name
	}
	sid := randToken()
	s.mu.Lock()
	s.sessions[sid] = &session{c: c, Email: email, Name: name, csrf: randToken(), created: time.Now(), lastActive: time.Now()}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: sid, Path: "/", HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
	if next == "" {
		next = "/f/inbox"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) setTheme(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("set")
	c := &http.Cookie{Name: "theme", Value: v, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cfg.CookieSecure}
	if v != "light" && v != "dark" {
		c.Value, c.MaxAge = "", -1
	}
	http.SetCookie(w, c)
	to := safeNext(r.URL.Query().Get("back"))
	if to == "" {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// ---------- rendering ----------

type data map[string]any

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, d data) {
	t := s.pages[page]
	if t == nil {
		http.Error(w, "page not found", http.StatusInternalServerError)
		return
	}
	if d == nil {
		d = data{}
	}
	if se := sessionFrom(r); se != nil {
		d["Me"], d["CSRF"] = se, se.csrf
	} else if se := s.current(r); se != nil {
		d["Me"], d["CSRF"] = se, se.csrf
	}
	if c, err := r.Cookie("theme"); err == nil && (c.Value == "light" || c.Value == "dark") {
		d["Theme"] = c.Value
	}
	d["Here"] = r.URL.RequestURI()
	d["Console"] = s.cfg.ConsoleURL
	d["Product"] = s.cfg.ProductName
	if d["Product"] == "" {
		d["Product"] = "Mail"
	}
	if m := r.URL.Query().Get("msg"); m != "" && len(m) < 300 {
		d["Msg"] = m
	}
	var b strings.Builder
	if err := t.ExecuteTemplate(&b, "layout", d); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, b.String())
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, r, "error", status, data{"Text": msg})
}

// mailErr answers a mail server failure (signing out when the password changed).
func (s *Server) mailErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errAuth):
		if c, e := r.Cookie(cookieName); e == nil {
			s.mu.Lock()
			delete(s.sessions, c.Value)
			s.mu.Unlock()
		}
		http.Redirect(w, r, "/login?msg="+url.QueryEscape("Your password changed: sign in again."), http.StatusSeeOther)
	case errors.Is(err, errNotFound):
		s.fail(w, r, http.StatusNotFound, "This message does not exist any more.")
	default:
		s.log.Warn("webmail", "err", err)
		s.fail(w, r, http.StatusBadGateway, "The mail server is not answering. Try again in a minute.")
	}
}

func when(t time.Time) string {
	now := time.Now()
	switch {
	case t.IsZero():
		return ""
	case now.Sub(t) < 24*time.Hour && now.Day() == t.Day():
		return t.Local().Format("15:04")
	case now.Year() == t.Year():
		return t.Local().Format("2 Jan")
	}
	return t.Local().Format("2 Jan 2006")
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// ---------- mail ----------

// folderBy finds a folder by its URL key (role or id).
func folderBy(fs []folder, key string) *folder {
	for i := range fs {
		if fs[i].Key() == key || fs[i].ID == key {
			return &fs[i]
		}
	}
	return nil
}

func roleID(fs []folder, role string) string {
	for _, f := range fs {
		if f.Role == role {
			return f.ID
		}
	}
	return ""
}

func (s *Server) folderPage(w http.ResponseWriter, r *http.Request) {
	se := sessionFrom(r)
	fs, err := se.c.folders(r.Context())
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	f := folderBy(fs, r.PathValue("folder"))
	if f == nil {
		s.fail(w, r, http.StatusNotFound, "This folder does not exist.")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 0)
	msgs, total, err := se.c.list(r.Context(), f.ID, "", page*pageSize, pageSize)
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	s.render(w, r, "list", http.StatusOK, data{"Folders": fs, "Folder": f, "Messages": msgs, "Total": total, "Page": page,
		"HasNext": (page+1)*pageSize < total, "Title": f.Label()})
}

func (s *Server) searchPage(w http.ResponseWriter, r *http.Request) {
	se := sessionFrom(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		q = q[:200]
	}
	fs, err := se.c.folders(r.Context())
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	var msgs []message
	total := 0
	if q != "" {
		if msgs, total, err = se.c.list(r.Context(), "", q, 0, pageSize); err != nil {
			s.mailErr(w, r, err)
			return
		}
	}
	s.render(w, r, "list", http.StatusOK, data{"Folders": fs, "Messages": msgs, "Total": total, "Q": q, "Title": "Search"})
}

func (s *Server) messagePage(w http.ResponseWriter, r *http.Request) {
	se := sessionFrom(r)
	m, err := se.c.get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	if !m.Seen() {
		_ = se.c.update(r.Context(), []string{m.ID}, map[string]any{"keywords/$seen": true})
		m.Keywords["$seen"] = true
	}
	fs, err := se.c.folders(r.Context())
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	var in *folder
	for id := range m.MailboxIDs {
		for i := range fs {
			if fs[i].ID == id {
				in = &fs[i]
			}
		}
	}
	s.render(w, r, "message", http.StatusOK, data{"Folders": fs, "Folder": in, "M": m, "HasHTML": m.HTML() != "", "Title": m.Subject})
}

// messageBody serves the HTML body alone, for a sandboxed frame: no scripts, no remote
// content (tracking pixels stay blocked), links open in a new tab.
func (s *Server) messageBody(w http.ResponseWriter, r *http.Request) {
	se := sessionFrom(r)
	m, err := se.c.get(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src data:; sandbox allow-popups allow-popups-to-escape-sandbox; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><base target="_blank"><style>body{margin:0;padding:16px;font:15px/1.5 -apple-system,system-ui,sans-serif;color:#111;background:#fff;word-wrap:break-word}img{max-width:100%;height:auto}</style></head><body>`)
	_, _ = io.WriteString(w, m.HTML())
	_, _ = io.WriteString(w, `</body></html>`)
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	se := sessionFrom(r)
	m, err := se.c.get(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var part *bodyPart
	for i := range m.Attachments {
		if m.Attachments[i].BlobID == r.PathValue("blob") {
			part = &m.Attachments[i]
		}
	}
	if part == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	resp, err := se.c.download(r.Context(), part.BlobID, part.Name, "application/octet-stream")
	if err != nil {
		http.Error(w, "the mail server is not answering", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	name := part.Name
	if name == "" {
		name = "attachment"
	}
	w.Header().Set("Content-Type", "application/octet-stream") // never rendered in the page
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, _ = io.Copy(w, resp.Body)
}

// moveTo moves messages to a role's folder.
func moveTo(fs []folder, role string) map[string]any {
	id := roleID(fs, role)
	if id == "" {
		return nil
	}
	return map[string]any{"mailboxIds": map[string]bool{id: true}}
}

func (s *Server) apply(r *http.Request, ids []string, action string) (string, error) {
	se := sessionFrom(r)
	fs, err := se.c.folders(r.Context())
	if err != nil {
		return "", err
	}
	switch action {
	case "read":
		return "Marked as read.", se.c.update(r.Context(), ids, map[string]any{"keywords/$seen": true})
	case "unread":
		return "Marked as unread.", se.c.update(r.Context(), ids, map[string]any{"keywords/$seen": nil})
	case "flag":
		return "Starred.", se.c.update(r.Context(), ids, map[string]any{"keywords/$flagged": true})
	case "unflag":
		return "Star removed.", se.c.update(r.Context(), ids, map[string]any{"keywords/$flagged": nil})
	case "trash", "junk", "inbox", "archive":
		p := moveTo(fs, action)
		if p == nil {
			return "", fmt.Errorf("no %s folder", action)
		}
		label := map[string]string{"trash": "Moved to the trash.", "junk": "Moved to spam.", "inbox": "Moved to the inbox.", "archive": "Archived."}[action]
		return label, se.c.update(r.Context(), ids, p)
	case "delete":
		return "Deleted for good.", se.c.destroy(r.Context(), ids)
	}
	return "", errors.New("unknown action")
}

func (s *Server) messageAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.FormValue("action")
	msg, err := s.apply(r, []string{id}, action)
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	back := safeNext(r.FormValue("back"))
	if back == "" || action == "trash" || action == "delete" || action == "junk" || action == "archive" {
		if back == "" || strings.HasPrefix(back, "/m/") {
			back = "/f/inbox"
		}
	}
	http.Redirect(w, r, withMsg(back, msg), http.StatusSeeOther)
}

func (s *Server) bulkAction(w http.ResponseWriter, r *http.Request) {
	ids := r.Form["sel"]
	back := safeNext(r.FormValue("back"))
	if back == "" {
		back = "/f/inbox"
	}
	if len(ids) == 0 {
		http.Redirect(w, r, withMsg(back, "Select messages first."), http.StatusSeeOther)
		return
	}
	if len(ids) > pageSize {
		ids = ids[:pageSize]
	}
	msg, err := s.apply(r, ids, r.FormValue("action"))
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	http.Redirect(w, r, withMsg(back, msg), http.StatusSeeOther)
}

func withMsg(path, msg string) string {
	u, err := url.Parse(path)
	if err != nil {
		return "/f/inbox"
	}
	q := u.Query()
	q.Set("msg", msg)
	u.RawQuery = q.Encode()
	return u.String()
}

// ---------- compose ----------

func (s *Server) composePage(w http.ResponseWriter, r *http.Request) {
	se := sessionFrom(r)
	fs, err := se.c.folders(r.Context())
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	d := data{"Folders": fs, "Title": "New message", "MaxMB": maxUploadMB}
	q := r.URL.Query()
	if id := q.Get("reply"); id != "" || q.Get("forward") != "" {
		if id == "" {
			id = q.Get("forward")
		}
		m, err := se.c.get(r.Context(), id)
		if err != nil {
			s.mailErr(w, r, err)
			return
		}
		quote := "\n\n" + "On " + m.Received.Local().Format("2 Jan 2006 15:04") + ", " + m.Sender().String() + " wrote:\n> " + strings.ReplaceAll(strings.TrimSpace(m.Text()), "\n", "\n> ")
		if q.Get("forward") != "" {
			d["Subject"] = prefixed("Fwd: ", m.Subject)
			d["Body"] = "\n\n---------- Forwarded message ----------\nFrom: " + m.Sender().String() + "\nDate: " + m.Received.Local().Format("2 Jan 2006 15:04") + "\nSubject: " + m.Subject + "\n\n" + m.Text()
		} else {
			to := m.From
			if len(m.ReplyTo) > 0 {
				to = m.ReplyTo
			}
			var tos []string
			for _, a := range to {
				tos = append(tos, a.String())
			}
			if q.Get("all") == "1" {
				for _, a := range append(append([]address{}, m.To...), m.Cc...) {
					if !strings.EqualFold(a.Email, se.Email) {
						tos = append(tos, a.String())
					}
				}
			}
			d["To"] = strings.Join(tos, ", ")
			d["Subject"] = prefixed("Re: ", m.Subject)
			d["Body"] = quote
			if len(m.MessageID) > 0 {
				d["InReplyTo"] = m.MessageID[0]
				d["References"] = strings.Join(append(append([]string{}, m.References...), m.MessageID[0]), " ")
			}
		}
	} else if to := q.Get("to"); to != "" {
		d["To"] = to
	}
	s.render(w, r, "compose", http.StatusOK, d)
}

func prefixed(p, subject string) string {
	if strings.HasPrefix(strings.ToLower(subject), strings.ToLower(p)) {
		return subject
	}
	return p + subject
}

// parseAddrs reads a comma-separated list of addresses.
func parseAddrs(v string) ([]address, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	list, err := mail.ParseAddressList(v)
	if err != nil {
		return nil, err
	}
	out := make([]address, 0, len(list))
	for _, a := range list {
		out = append(out, address{Name: a.Name, Email: strings.ToLower(a.Address)})
	}
	return out, nil
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	se := sessionFrom(r)
	fs, err := se.c.folders(r.Context())
	if err != nil {
		s.mailErr(w, r, err)
		return
	}
	again := func(msg string) {
		s.render(w, r, "compose", http.StatusUnprocessableEntity, data{"Folders": fs, "Title": "New message", "Err": msg, "MaxMB": maxUploadMB,
			"To": r.FormValue("to"), "Cc": r.FormValue("cc"), "Bcc": r.FormValue("bcc"), "Subject": r.FormValue("subject"), "Body": r.FormValue("body"),
			"InReplyTo": r.FormValue("in_reply_to"), "References": r.FormValue("references")})
	}
	to, err1 := parseAddrs(r.FormValue("to"))
	cc, err2 := parseAddrs(r.FormValue("cc"))
	bcc, err3 := parseAddrs(r.FormValue("bcc"))
	switch {
	case err1 != nil || err2 != nil || err3 != nil:
		again("An address is not valid: separate addresses with commas, such as anna@example.com, Bob <bob@example.org>.")
		return
	case len(to)+len(cc)+len(bcc) == 0:
		again("Add at least one recipient.")
		return
	case len(to)+len(cc)+len(bcc) > 100:
		again("At most 100 recipients.")
		return
	}
	subject := strings.TrimSpace(r.FormValue("subject"))
	if len(subject) > 500 {
		again("The subject is too long.")
		return
	}
	ids, err := se.c.identities(r.Context())
	if err != nil || len(ids) == 0 {
		again("This mailbox cannot send mail (no sending identity).")
		return
	}
	idt := ids[0]
	out := outgoing{From: address{Name: idt.Name, Email: idt.Email}, To: to, Cc: cc, Bcc: bcc, Subject: subject, Text: r.FormValue("body")}
	if irt := strings.TrimSpace(r.FormValue("in_reply_to")); irt != "" {
		out.InReplyTo = []string{strings.Trim(irt, "<>")}
		for _, ref := range strings.Fields(r.FormValue("references")) {
			out.References = append(out.References, strings.Trim(ref, "<>"))
		}
	}
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File["files"] {
			if fh.Size == 0 {
				continue
			}
			f, err := fh.Open()
			if err != nil {
				again("An attachment could not be read.")
				return
			}
			b, err := io.ReadAll(io.LimitReader(f, maxUploadMB<<20))
			f.Close()
			if err != nil {
				again("An attachment could not be read.")
				return
			}
			u, err := se.c.upload(r.Context(), fh.Filename, fh.Header.Get("Content-Type"), b)
			if err != nil {
				again(err.Error())
				return
			}
			out.Attachments = append(out.Attachments, u)
		}
	}
	if err := se.c.send(r.Context(), idt, out, roleID(fs, "drafts"), roleID(fs, "sent")); err != nil {
		if errors.Is(err, errAuth) {
			s.mailErr(w, r, err)
			return
		}
		again(err.Error())
		return
	}
	http.Redirect(w, r, withMsg("/f/inbox", "Message sent."), http.StatusSeeOther)
}

func urlPath(s string) string  { return url.PathEscape(s) }
func urlQuery(s string) string { return url.QueryEscape(s) }

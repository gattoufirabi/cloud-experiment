package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"platform/internal/config"
	"platform/internal/db"
	"platform/internal/secure"
)

// Passkeys (WebAuthn) with the go-webauthn library.
//
//   - Registration happens on the Account page, in a fully authenticated session.
//   - Sign-in uses discoverable credentials ("Sign in with a passkey", no email typed)
//     and requires user verification, so it stands in for password + two-step code.
//   - Ceremony state (the random challenge) is kept server side for 5 minutes and used
//     once; the browser only holds an opaque ID in a SameSite=Strict cookie.
//   - Only public keys are stored. A sign counter that goes backwards (cloned
//     authenticator) refuses the sign-in.

func newWebAuthn(cfg config.Config) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPDisplayName: "Platform",
		RPID:          cfg.ConsoleHost,
		RPOrigins:     []string{cfg.BaseURL},
	})
}

type ceremony struct {
	data    webauthn.SessionData
	userID  string
	expires time.Time
}

type ceremonyStore struct {
	mu sync.Mutex
	m  map[string]ceremony
}

func (c *ceremonyStore) put(key string, cer ceremony) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.m { // drop expired ones
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	cer.expires = now.Add(5 * time.Minute)
	c.m[key] = cer
}

// take returns and removes a ceremony: each challenge can be answered once.
func (c *ceremonyStore) take(key string) (ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[key]
	delete(c.m, key)
	if !ok || time.Now().After(cer.expires) {
		return ceremony{}, false
	}
	return cer, true
}

// ---------- the user as go-webauthn sees it ----------

type waUser struct {
	id          []byte
	name, label string
	creds       []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.id }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.label }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// User handles are the 16 raw bytes of the user's UUID.
func userHandle(id string) []byte {
	b, _ := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return b
}

func userIDFromHandle(h []byte) (string, error) {
	if len(h) != 16 {
		return "", errors.New("unknown user handle")
	}
	x := hex.EncodeToString(h)
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:], nil
}

func (s *Server) loadWAUser(ctx context.Context, userID string) (*waUser, error) {
	u := &waUser{id: userHandle(userID)}
	if err := s.pool.QueryRow(ctx, `SELECT email, name FROM users WHERE id = $1`, userID).Scan(&u.name, &u.label); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, public_key, attestation_type, transports, coalesce(aaguid, '\x'::bytea), sign_count, backup_eligible, backup_state
		FROM passkeys WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c webauthn.Credential
		var transports []string
		var count int64
		if err := rows.Scan(&c.ID, &c.PublicKey, &c.AttestationType, &transports, &c.Authenticator.AAGUID, &count,
			&c.Flags.BackupEligible, &c.Flags.BackupState); err != nil {
			return nil, err
		}
		c.Authenticator.SignCount = uint32(count)
		for _, t := range transports {
			c.Transport = append(c.Transport, protocol.AuthenticatorTransport(t))
		}
		u.creds = append(u.creds, c)
	}
	return u, rows.Err()
}

// ---------- account page ----------

type passkeyView struct {
	ID       string
	Name     string
	Created  time.Time
	LastUsed *time.Time
	Synced   bool
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	rows, err := s.pool.Query(r.Context(), `
		SELECT id, name, created_at, last_used_at, backup_state FROM passkeys WHERE user_id = $1 ORDER BY created_at`, sess.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var keys []passkeyView
	for rows.Next() {
		var id []byte
		var k passkeyView
		if err := rows.Scan(&id, &k.Name, &k.Created, &k.LastUsed, &k.Synced); err != nil {
			rows.Close()
			s.serverError(w, r, err)
			return
		}
		k.ID = base64.RawURLEncoding.EncodeToString(id)
		keys = append(keys, k)
	}
	rows.Close()
	var codesLeft int
	_ = s.pool.QueryRow(r.Context(), `SELECT count(*) FROM recovery_codes WHERE user_id = $1 AND used_at IS NULL`, sess.UserID).Scan(&codesLeft)
	s.render(w, r, "account", http.StatusOK, data{"Passkeys": keys, "CodesLeft": codesLeft})
}

func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	u, err := s.loadWAUser(r.Context(), sess.UserID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not load the account")
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range u.creds {
		exclude = append(exclude, c.Descriptor())
	}
	options, sd, err := s.wa.BeginRegistration(u,
		webauthn.WithExclusions(exclude),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		}))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not start registration")
		return
	}
	s.ceremonies.put("reg:"+hex.EncodeToString(sess.TokenHash), ceremony{data: *sd, userID: sess.UserID})
	writeJSON(w, options)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	cer, ok := s.ceremonies.take("reg:" + hex.EncodeToString(sess.TokenHash))
	if !ok || cer.userID != sess.UserID {
		apiError(w, http.StatusBadRequest, "the registration expired; try again")
		return
	}
	u, err := s.loadWAUser(r.Context(), sess.UserID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not load the account")
		return
	}
	cred, err := s.wa.FinishRegistration(u, cer.data, r)
	if err != nil {
		s.log.Warn("passkey registration rejected", "user", sess.UserID, "err", err)
		apiError(w, http.StatusBadRequest, "the passkey could not be verified")
		return
	}
	var transports []string
	for _, t := range cred.Transport {
		transports = append(transports, string(t))
	}
	if transports == nil {
		transports = []string{}
	}
	if _, err := s.pool.Exec(r.Context(), `
		INSERT INTO passkeys (id, user_id, name, public_key, attestation_type, transports, aaguid, sign_count, backup_eligible, backup_state)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		cred.ID, sess.UserID, name, cred.PublicKey, cred.AttestationType, transports, cred.Authenticator.AAGUID,
		int64(cred.Authenticator.SignCount), cred.Flags.BackupEligible, cred.Flags.BackupState); err != nil {
		if isUniqueViolation(err) {
			apiError(w, http.StatusConflict, "this passkey is already registered")
			return
		}
		apiError(w, http.StatusInternalServerError, "could not save the passkey")
		return
	}
	_ = db.Audit(r.Context(), s.pool, "", sess.UserID, "passkey.added", "user:"+sess.UserID, map[string]string{"name": name}, clientIP(r))
	writeJSON(w, map[string]string{"status": "added"})
}

func (s *Server) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	sess := sessionFrom(r)
	id, err := base64.RawURLEncoding.DecodeString(r.PathValue("id"))
	if err != nil {
		back(w, r, "/account", "", "Unknown passkey.")
		return
	}
	tag, err := s.pool.Exec(r.Context(), `DELETE FROM passkeys WHERE id = $1 AND user_id = $2`, id, sess.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if tag.RowsAffected() == 1 {
		_ = db.Audit(r.Context(), s.pool, "", sess.UserID, "passkey.removed", "user:"+sess.UserID, nil, clientIP(r))
	}
	back(w, r, "/account", "Passkey removed.", "")
}

// ---------- sign-in ----------

const ceremonyCookie = "pkc"

// sameOrigin refuses cross-site calls to the sign-in endpoints (there is no session
// yet, so no CSRF token; the ceremony cookie is also SameSite=Strict).
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o == "" || o == s.cfg.BaseURL
}

func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		apiError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if !s.limiter.allow("pk-login:"+clientIP(r), 30, 15*time.Minute) {
		apiError(w, http.StatusTooManyRequests, "too many attempts; wait a few minutes")
		return
	}
	options, sd, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	id := secure.NewToken()
	s.ceremonies.put("login:"+id, ceremony{data: *sd})
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: id, Path: "/login/passkey", MaxAge: 300,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
	writeJSON(w, options)
}

func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.sameOrigin(r) {
		apiError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if !s.limiter.allow("pk-login:"+ip, 30, 15*time.Minute) {
		apiError(w, http.StatusTooManyRequests, "too many attempts; wait a few minutes")
		return
	}
	c, err := r.Cookie(ceremonyCookie)
	if err != nil {
		apiError(w, http.StatusBadRequest, "the sign-in expired; try again")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: "", Path: "/login/passkey", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure})
	cer, ok := s.ceremonies.take("login:" + c.Value)
	if !ok {
		apiError(w, http.StatusBadRequest, "the sign-in expired; try again")
		return
	}
	var userID string
	cred, err := s.wa.FinishDiscoverableLogin(func(rawID, handle []byte) (webauthn.User, error) {
		id, err := userIDFromHandle(handle)
		if err != nil {
			return nil, err
		}
		userID = id
		return s.loadWAUser(r.Context(), id)
	}, cer.data, r)
	if err != nil || userID == "" {
		s.log.Warn("passkey sign-in rejected", "err", err)
		apiError(w, http.StatusUnauthorized, "this passkey could not be verified")
		return
	}
	if cred.Authenticator.CloneWarning {
		_ = db.Audit(r.Context(), s.pool, "", userID, "passkey.clone_warning", "user:"+userID, nil, ip)
		apiError(w, http.StatusUnauthorized, "this passkey looks cloned; sign in with your password and replace it")
		return
	}
	var locked *time.Time
	if err := s.pool.QueryRow(r.Context(), `SELECT locked_until FROM users WHERE id = $1`, userID).Scan(&locked); err != nil || (locked != nil && locked.After(time.Now())) {
		apiError(w, http.StatusUnauthorized, "the account is temporarily locked")
		return
	}
	if _, err := s.pool.Exec(r.Context(), `
		UPDATE passkeys SET sign_count = $3, backup_state = $4, last_used_at = now() WHERE id = $1 AND user_id = $2`,
		cred.ID, userID, int64(cred.Authenticator.SignCount), cred.Flags.BackupState); err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.startSession(w, r, userID, true); err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	_ = db.Audit(r.Context(), s.pool, "", userID, "login.passkey", "user:"+userID, nil, ip)
	writeJSON(w, map[string]string{"redirect": "/"})
}

// ---------- routes ----------

func (s *Server) passkeyRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /account", s.auth(authFull, s.accountPage))
	m.HandleFunc("POST /account/recovery-codes", s.auth(authFull, s.regenerateRecoveryCodes))
	m.HandleFunc("POST /account/passkeys/begin", s.auth(authFull, s.passkeyRegisterBegin))
	m.HandleFunc("POST /account/passkeys/finish", s.auth(authFull, s.passkeyRegisterFinish))
	m.HandleFunc("POST /account/passkeys/{id}/delete", s.auth(authFull, s.passkeyDelete))
	m.HandleFunc("POST /login/passkey/begin", s.passkeyLoginBegin)
	m.HandleFunc("POST /login/passkey/finish", s.passkeyLoginFinish)
}

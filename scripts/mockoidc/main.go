// mockoidc is a minimal OpenID Connect provider for the end-to-end test: discovery,
// JWKS, an /authorize endpoint that signs in whoever the test names (mock_email,
// mock_groups, mock_verified query parameters) and a /token endpoint with client
// authentication and PKCE. It also has /sink, an endpoint that receives log exports
// (gzipped NDJSON, signed), and /sink/count?q=text to check what arrived. Never use it
// for anything else. With -dns it also answers TXT queries (see dns.go).
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type grant struct {
	nonce, challenge, redirect, email, name string
	groups                                  []string
	verified                                bool
}

func main() {
	addr := flag.String("addr", ":8097", "listen address")
	issuer := flag.String("issuer", "http://host.docker.internal:8097", "issuer URL")
	clientID := flag.String("client-id", "platform-e2e", "client ID")
	secret := flag.String("client-secret", "e2e-client-secret", "client secret")
	sinkSecret := flag.String("sink-secret", "e2e-sink-secret", "secret that signs log exports")
	dnsAddr := flag.String("dns", "", "UDP address of the test DNS server (off when empty)")
	smtpAddr := flag.String("smtp", "", "address of the test mail server (off when empty)")
	flag.Parse()
	store := &txtStore{recs: map[string][]rec{}}
	store.handlers()
	(&hookLog{}).handlers()
	if *dnsAddr != "" {
		go serveDNS(*dnsAddr, store)
	}
	if *smtpAddr != "" {
		go serveSMTP(*smtpAddr)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	var mu sync.Mutex
	codes := map[string]grant{}
	b64 := base64.RawURLEncoding.EncodeToString
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	http.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"issuer": *issuer, "authorization_endpoint": *issuer + "/authorize",
			"token_endpoint": *issuer + "/token", "jwks_uri": *issuer + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	http.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "e2e", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
	})
	http.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != *clientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "bad authorization request", http.StatusBadRequest)
			return
		}
		email := q.Get("mock_email")
		if email == "" {
			email = q.Get("login_hint")
		}
		g := grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), email: email,
			name: q.Get("mock_name"), verified: q.Get("mock_verified") != "false"}
		if gs := q.Get("mock_groups"); gs != "" {
			g.groups = strings.Split(gs, ",")
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		code := b64(b)
		mu.Lock()
		codes[code] = g
		mu.Unlock()
		u, _ := url.Parse(g.redirect)
		rq := u.Query()
		rq.Set("code", code)
		rq.Set("state", q.Get("state"))
		u.RawQuery = rq.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	http.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		id, sec, ok := r.BasicAuth()
		if ok {
			id, _ = url.QueryUnescape(id)
			sec, _ = url.QueryUnescape(sec)
		}
		if !ok || id != *clientID || sec != *secret {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]string{"error": "invalid_client"})
			return
		}
		mu.Lock()
		g, found := codes[r.PostFormValue("code")]
		delete(codes, r.PostFormValue("code"))
		mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !found || b64(sum[:]) != g.challenge || r.PostFormValue("redirect_uri") != g.redirect {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant"})
			return
		}
		now := time.Now()
		claims := jwt.MapClaims{"iss": *issuer, "aud": *clientID, "sub": "sub-" + g.email, "email": g.email, "email_verified": g.verified,
			"name": g.name, "nonce": g.nonce, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
		if g.groups != nil {
			claims["groups"] = g.groups
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = "e2e"
		signed, err := tok.SignedString(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"access_token": "unused", "token_type": "Bearer", "id_token": signed, "expires_in": 300})
	})
	var sinkMu sync.Mutex
	var sinkLines []string
	badSignatures := 0
	http.HandleFunc("/sink", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		mac := hmac.New(sha256.New, []byte(*sinkSecret))
		mac.Write(body)
		ok := r.Header.Get("X-Platform-Signature") == "sha256="+hex.EncodeToString(mac.Sum(nil))
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			http.Error(w, "not gzip", http.StatusBadRequest)
			return
		}
		plain, _ := io.ReadAll(zr)
		sinkMu.Lock()
		if !ok {
			badSignatures++
		}
		for _, l := range strings.Split(strings.TrimSpace(string(plain)), "\n") {
			if l != "" {
				sinkLines = append(sinkLines, l)
			}
		}
		sinkMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	http.HandleFunc("/sink/count", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		sinkMu.Lock()
		n := 0
		for _, l := range sinkLines {
			if strings.Contains(l, q) {
				n++
			}
		}
		bad := badSignatures
		sinkMu.Unlock()
		fmt.Fprintf(w, "%d %d", n, bad)
	})
	log.Printf("mock OIDC provider on %s, issuer %s", *addr, *issuer)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

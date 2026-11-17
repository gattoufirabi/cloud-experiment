package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// A provider signs ID tokens with its RSA key; Verify accepts only a token with the
// right signature, issuer, audience, expiry and nonce.
func TestVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/a", "token_endpoint": srv.URL + "/t", "jwks_uri": srv.URL + "/k"})
		case "/k":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "k1",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
		}
	}))
	defer srv.Close()
	c := NewClient(srv.Client())
	p, err := c.Discover(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(k *rsa.PrivateKey, mut func(jwt.MapClaims)) string {
		cl := jwt.MapClaims{"iss": srv.URL, "aud": "app", "sub": "u1", "email": "a@example.test", "email_verified": true,
			"nonce": "n1", "exp": time.Now().Add(time.Minute).Unix(), "groups": []string{"ops", "dev"}}
		if mut != nil {
			mut(cl)
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, cl)
		tok.Header["kid"] = "k1"
		s, _ := tok.SignedString(k)
		return s
	}
	cl, err := c.Verify(context.Background(), p, "app", sign(key, nil), "n1")
	if err != nil || cl.Email != "a@example.test" || !cl.EmailVerified || len(cl.Groups("groups")) != 2 {
		t.Fatalf("valid token: %v %+v", err, cl)
	}
	bad := map[string]string{
		"wrong key":      sign(other, nil),
		"wrong audience": sign(key, func(m jwt.MapClaims) { m["aud"] = "someone-else" }),
		"wrong issuer":   sign(key, func(m jwt.MapClaims) { m["iss"] = "https://evil.example" }),
		"expired":        sign(key, func(m jwt.MapClaims) { m["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"wrong nonce":    sign(key, func(m jwt.MapClaims) { m["nonce"] = "other" }),
	}
	for name, tok := range bad {
		if _, err := c.Verify(context.Background(), p, "app", tok, "n1"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk") != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Error("PKCE challenge differs from RFC 7636 example")
	}
}

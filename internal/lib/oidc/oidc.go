// Package oidc is a small OpenID Connect relying party: discovery, the authorization
// code flow with PKCE, and ID token verification against the provider's JWKS (RSA and
// EC keys). It is enough for signing in with Google Workspace, Microsoft Entra ID,
// Okta, Keycloak, Authentik, Dex and other standard providers.
package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Provider struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// Client fetches provider metadata and keys (cached) with its HTTP client.
type Client struct {
	HTTP *http.Client
	mu   sync.Mutex
	disc map[string]cached[Provider]
	keys map[string]cached[map[string]any]
}

type cached[T any] struct {
	v  T
	at time.Time
}

const cacheFor = 10 * time.Minute

func NewClient(hc *http.Client) *Client {
	return &Client{HTTP: hc, disc: map[string]cached[Provider]{}, keys: map[string]cached[map[string]any]{}}
}

func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Discover reads /.well-known/openid-configuration; the issuer it announces must be
// the one configured.
func (c *Client) Discover(ctx context.Context, issuer string) (Provider, error) {
	issuer = strings.TrimRight(issuer, "/")
	c.mu.Lock()
	if e, ok := c.disc[issuer]; ok && time.Since(e.at) < cacheFor {
		c.mu.Unlock()
		return e.v, nil
	}
	c.mu.Unlock()
	var p Provider
	if err := c.getJSON(ctx, issuer+"/.well-known/openid-configuration", &p); err != nil {
		return p, fmt.Errorf("discovery: %w", err)
	}
	if strings.TrimRight(p.Issuer, "/") != issuer {
		return p, fmt.Errorf("discovery: the provider announces issuer %q, not %q", p.Issuer, issuer)
	}
	if p.AuthorizationEndpoint == "" || p.TokenEndpoint == "" || p.JWKSURI == "" {
		return p, errors.New("discovery: incomplete provider metadata")
	}
	c.mu.Lock()
	c.disc[issuer] = cached[Provider]{p, time.Now()}
	c.mu.Unlock()
	return p, nil
}

// Random returns a URL-safe random string (state, nonce, PKCE verifier).
func Random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Challenge is the PKCE S256 challenge of a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL is where the user signs in.
func AuthURL(p Provider, clientID, redirectURI, state, nonce, verifier, loginHint string) string {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"scope": {"openid email profile"}, "state": {state}, "nonce": {nonce},
		"code_challenge": {Challenge(verifier)}, "code_challenge_method": {"S256"},
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	sep := "?"
	if strings.Contains(p.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return p.AuthorizationEndpoint + sep + q.Encode()
}

// Exchange trades the authorization code for tokens and returns the raw ID token.
func (c *Client) Exchange(ctx context.Context, p Provider, clientID, secret, redirectURI, code, verifier string) (string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {clientID}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if secret != "" {
		req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(secret))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("token endpoint answered %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return "", fmt.Errorf("token endpoint: %s %s", out.Error, out.Desc)
	}
	return out.IDToken, nil
}

// Claims are the ID token claims the platform uses; Raw holds them all.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Raw           map[string]any
}

// Groups returns the string values of a claim (a list or a single string).
func (c Claims) Groups(claim string) []string {
	var out []string
	switch v := c.Raw[claim].(type) {
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok {
				out = append(out, s)
			}
		}
	case string:
		out = append(out, v)
	}
	return out
}

// Verify checks the ID token's signature (keys from the provider's JWKS), issuer,
// audience, expiry and nonce.
func (c *Client) Verify(ctx context.Context, p Provider, clientID, raw, nonce string) (Claims, error) {
	var cl Claims
	keyfunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		k, err := c.key(ctx, p.JWKSURI, kid, false)
		if err != nil { // the provider may have rotated its keys
			k, err = c.key(ctx, p.JWKSURI, kid, true)
		}
		return k, err
	}
	tok, err := jwt.Parse(raw, keyfunc,
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256"}),
		jwt.WithIssuer(p.Issuer), jwt.WithAudience(clientID), jwt.WithExpirationRequired(), jwt.WithLeeway(time.Minute))
	if err != nil {
		return cl, fmt.Errorf("id token: %w", err)
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return cl, errors.New("id token: unexpected claims")
	}
	if n, _ := mc["nonce"].(string); nonce == "" || n != nonce {
		return cl, errors.New("id token: nonce mismatch")
	}
	cl.Raw = mc
	cl.Subject, _ = mc["sub"].(string)
	cl.Email, _ = mc["email"].(string)
	cl.Name, _ = mc["name"].(string)
	switch v := mc["email_verified"].(type) {
	case bool:
		cl.EmailVerified = v
	case string:
		cl.EmailVerified = v == "true"
	}
	if cl.Subject == "" {
		return cl, errors.New("id token: no subject")
	}
	return cl, nil
}

func (c *Client) key(ctx context.Context, jwksURI, kid string, refresh bool) (any, error) {
	c.mu.Lock()
	e, ok := c.keys[jwksURI]
	c.mu.Unlock()
	if !ok || refresh || time.Since(e.at) > cacheFor {
		var set struct {
			Keys []json.RawMessage `json:"keys"`
		}
		if err := c.getJSON(ctx, jwksURI, &set); err != nil {
			return nil, fmt.Errorf("jwks: %w", err)
		}
		m := map[string]any{}
		for _, raw := range set.Keys {
			id, k, err := parseJWK(raw)
			if err == nil {
				m[id] = k
			}
		}
		e = cached[map[string]any]{m, time.Now()}
		c.mu.Lock()
		c.keys[jwksURI] = e
		c.mu.Unlock()
	}
	if k, ok := e.v[kid]; ok {
		return k, nil
	}
	if kid == "" && len(e.v) == 1 {
		for _, k := range e.v {
			return k, nil
		}
	}
	return nil, fmt.Errorf("jwks: no key %q", kid)
}

func parseJWK(raw json.RawMessage) (string, any, error) {
	var k struct {
		Kty, Kid, Use, N, E, Crv, X, Y string
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return "", nil, err
	}
	if k.Use != "" && k.Use != "sig" {
		return "", nil, errors.New("not a signing key")
	}
	dec := base64.RawURLEncoding.DecodeString
	switch k.Kty {
	case "RSA":
		n, err1 := dec(k.N)
		e, err2 := dec(k.E)
		if err1 != nil || err2 != nil {
			return "", nil, errors.New("bad RSA key")
		}
		return k.Kid, &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return "", nil, errors.New("unsupported curve")
		}
		x, err1 := dec(k.X)
		y, err2 := dec(k.Y)
		if err1 != nil || err2 != nil {
			return "", nil, errors.New("bad EC key")
		}
		return k.Kid, &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
	}
	return "", nil, errors.New("unsupported key type")
}

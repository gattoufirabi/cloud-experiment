// Package registry issues the bearer tokens that the CNCF Distribution registry verifies
// (Docker registry token authentication), and holds the image-namespace rules.
//
// Every organization owns exactly one namespace: <registry-host>/<org-slug>/...
// A token never grants access outside the namespace of the credential that asked for it.
package registry

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"
)

const (
	Issuer  = "platform"
	Service = "platform-registry"
	// AgentUserPrefix: the agent and the scanner log in as "agent/<org-slug>" with a
	// password derived from a shared secret, so even platform-side pulls are org-scoped.
	AgentUserPrefix = "agent/"
	// BuilderUserPrefix: the agent pushes images it built as "builder/<org-slug>",
	// allowed to push (and pull) only inside that organization's namespace.
	BuilderUserPrefix = "builder/"
)

// ---------- signing key ----------

// GenerateKeyPair writes an ECDSA P-256 signing key (API side) and a self-signed
// certificate holding its public key (registry side, "rootcertbundle").
func GenerateKeyPair(keyPath, certPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "platform registry token signer"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := writeNew(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o400); err != nil {
		return err
	}
	return writeNew(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0o444)
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func LoadKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("registry signing key: no PEM block")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// KeyID is the libtrust fingerprint the registry uses to match the token's "kid"
// header with a key from its certificate bundle.
func KeyID(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	s := strings.TrimRight(base32.StdEncoding.EncodeToString(sum[:30]), "=")
	var b bytes.Buffer
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+4])
	}
	return b.String(), nil
}

// ---------- tokens ----------

type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Sign returns a compact JWS (ES256) the registry accepts for ttl.
func Sign(key *ecdsa.PrivateKey, subject string, access []Access, ttl time.Duration) (string, error) {
	kid, err := KeyID(&key.PublicKey)
	if err != nil {
		return "", err
	}
	if access == nil {
		access = []Access{}
	}
	now := time.Now()
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	header, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256", "kid": kid})
	claims, _ := json.Marshal(map[string]any{
		"iss": Issuer, "sub": subject, "aud": Service,
		"exp": now.Add(ttl).Unix(), "nbf": now.Add(-10 * time.Second).Unix(), "iat": now.Unix(),
		"jti": hex.EncodeToString(jti), "access": access,
	})
	enc := base64.RawURLEncoding
	input := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS ES256: r || s, each left-padded to 32 bytes
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + enc.EncodeToString(sig), nil
}

// ParseScope splits "repository:acme/web:pull,push".
func ParseScope(scope string) (typ, name string, actions []string, ok bool) {
	first := strings.Index(scope, ":")
	last := strings.LastIndex(scope, ":")
	if first <= 0 || last <= first {
		return "", "", nil, false
	}
	typ, name = scope[:first], scope[first+1:last]
	for _, a := range strings.Split(scope[last+1:], ",") {
		if a = strings.TrimSpace(a); a != "" {
			actions = append(actions, a)
		}
	}
	return typ, name, actions, name != "" && len(actions) > 0
}

// InternalPassword is the agent/scanner password for one organization's namespace.
func InternalPassword(secret, orgSlug string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("registry-pull:" + orgSlug))
	return hex.EncodeToString(m.Sum(nil))
}

// BuilderPassword is the agent's push password for one organization's namespace.
func BuilderPassword(secret, orgSlug string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("registry-push:" + orgSlug))
	return hex.EncodeToString(m.Sum(nil))
}

// ---------- namespace rules ----------

// IsPlatformImage reports whether image is hosted on the platform registry.
// Hostnames are compared case-insensitively, as DNS does.
func IsPlatformImage(image, registryHost string) bool {
	return registryHost != "" && len(image) > len(registryHost) &&
		strings.EqualFold(image[:len(registryHost)], registryHost) && image[len(registryHost)] == '/'
}

// CheckNamespace refuses platform-registry images outside the deploying organization.
func CheckNamespace(image, registryHost, orgSlug string) error {
	if !IsPlatformImage(image, registryHost) {
		return nil
	}
	if !strings.HasPrefix(image[len(registryHost)+1:], orgSlug+"/") {
		return fmt.Errorf("images from the platform registry must be under %s/%s/", registryHost, orgSlug)
	}
	return nil
}

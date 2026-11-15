// softauthn is a software passkey (WebAuthn authenticator) used only by the end-to-end
// test, so the real registration and sign-in ceremonies run against the API without a
// browser or hardware key. It holds a P-256 key in a local JSON file.
//
//	softauthn register -url URL -sid SID -csrf TOKEN -key FILE [-name NAME]
//	softauthn login    -url URL -key FILE [-tamper]     # prints the new session cookie
//
// Not for real use: the key file is unprotected.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

type keyFile struct {
	Key    []byte `json:"key"` // PKCS#8
	CredID []byte `json:"cred_id"`
	User   []byte `json:"user"`
	RPID   string `json:"rp_id"`
	Count  uint32 `json:"count"`
}

var b64 = base64.RawURLEncoding

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, "softauthn: "+f+"\n", a...); os.Exit(1) }

func client() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h, p, err := net.SplitHostPort(addr); err == nil && strings.HasSuffix(h, ".localhost") {
			addr = net.JoinHostPort("127.0.0.1", p)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: tr, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func post(c *http.Client, base, path string, body any, hdr map[string]string) []byte {
	var rd io.Reader = strings.NewReader("{}")
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		die("%s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		die("%s: HTTP %d %s", path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out
}

type options struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		RPID string `json:"rpId"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"publicKey"`
}

func clientData(typ, challenge, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	return b
}

func main() {
	if len(os.Args) < 2 {
		die("usage: softauthn register|login ...")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	base := fs.String("url", "", "console URL")
	sid := fs.String("sid", "", "session cookie (register)")
	csrf := fs.String("csrf", "", "CSRF token (register)")
	kf := fs.String("key", "", "key file")
	name := fs.String("name", "Test key", "passkey name")
	tamper := fs.Bool("tamper", false, "send a broken signature (login)")
	_ = fs.Parse(os.Args[2:])
	c := client()
	u, err := url.Parse(*base)
	if err != nil || *kf == "" {
		die("need -url and -key")
	}

	switch os.Args[1] {
	case "register":
		c.Jar.SetCookies(u, []*http.Cookie{{Name: "sid", Value: *sid}})
		hdr := map[string]string{"X-CSRF-Token": *csrf}
		var opt options
		if err := json.Unmarshal(post(c, *base, "/account/passkeys/begin", nil, hdr), &opt); err != nil {
			die("begin: %v", err)
		}
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		credID := make([]byte, 32)
		_, _ = rand.Read(credID)
		user, _ := b64.DecodeString(opt.PublicKey.User.ID)
		rpID := opt.PublicKey.RP.ID

		x, y := make([]byte, 32), make([]byte, 32)
		key.PublicKey.X.FillBytes(x)
		key.PublicKey.Y.FillBytes(y)
		cose, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
		rpHash := sha256.Sum256([]byte(rpID))
		auth := append([]byte{}, rpHash[:]...)
		auth = append(auth, 0x45)                     // UP | UV | AT
		auth = binary.BigEndian.AppendUint32(auth, 0) // sign count
		auth = append(auth, make([]byte, 16)...)      // AAGUID
		auth = binary.BigEndian.AppendUint16(auth, uint16(len(credID)))
		auth = append(auth, credID...)
		auth = append(auth, cose...)
		att, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": auth})

		post(c, *base, "/account/passkeys/finish?name="+url.QueryEscape(*name), map[string]any{
			"id": b64.EncodeToString(credID), "rawId": b64.EncodeToString(credID), "type": "public-key",
			"response": map[string]any{
				"clientDataJSON":    b64.EncodeToString(clientData("webauthn.create", opt.PublicKey.Challenge, *base)),
				"attestationObject": b64.EncodeToString(att),
				"transports":        []string{"internal"},
			},
		}, hdr)
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		out, _ := json.Marshal(keyFile{Key: der, CredID: credID, User: user, RPID: rpID})
		if err := os.WriteFile(*kf, out, 0o600); err != nil {
			die("%v", err)
		}
		fmt.Println("registered")

	case "login":
		raw, err := os.ReadFile(*kf)
		if err != nil {
			die("%v", err)
		}
		var k keyFile
		if err := json.Unmarshal(raw, &k); err != nil {
			die("%v", err)
		}
		pk, err := x509.ParsePKCS8PrivateKey(k.Key)
		if err != nil {
			die("%v", err)
		}
		var opt options
		if err := json.Unmarshal(post(c, *base, "/login/passkey/begin", nil, nil), &opt); err != nil {
			die("begin: %v", err)
		}
		k.Count++
		rpHash := sha256.Sum256([]byte(k.RPID))
		auth := append(append([]byte{}, rpHash[:]...), 0x05) // UP | UV
		auth = binary.BigEndian.AppendUint32(auth, k.Count)
		cd := clientData("webauthn.get", opt.PublicKey.Challenge, *base)
		cdHash := sha256.Sum256(cd)
		digest := sha256.Sum256(append(append([]byte{}, auth...), cdHash[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, pk.(*ecdsa.PrivateKey), digest[:])
		if *tamper {
			sig[len(sig)-1] ^= 0xff
		}
		post(c, *base, "/login/passkey/finish", map[string]any{
			"id": b64.EncodeToString(k.CredID), "rawId": b64.EncodeToString(k.CredID), "type": "public-key",
			"response": map[string]any{
				"clientDataJSON":    b64.EncodeToString(cd),
				"authenticatorData": b64.EncodeToString(auth),
				"signature":         b64.EncodeToString(sig),
				"userHandle":        b64.EncodeToString(k.User),
			},
		}, nil)
		out, _ := json.Marshal(k)
		_ = os.WriteFile(*kf, out, 0o600)
		for _, ck := range c.Jar.Cookies(u) {
			if ck.Name == "sid" {
				fmt.Println(ck.Value)
				return
			}
		}
		die("no session cookie returned")
	default:
		die("unknown command %q", os.Args[1])
	}
}

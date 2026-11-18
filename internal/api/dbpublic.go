package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Public access to a managed database: a TLS endpoint on a public port of the platform,
// open only to the listed client addresses (the agent runs the proxy, see dbproxy).
// PostgreSQL, Redis and MongoDB; MySQL, MariaDB and Kafka negotiate TLS inside their own
// protocols and are not offered yet.

type dbPublic struct {
	Enabled bool
	Host    string // name in the certificate (resolves to the platform)
	IP      string
	Port    int
	Allow   []string
	Cert    string
}

func publicEngine(engine string) bool {
	return engine == "postgres" || engine == "redis" || engine == "mongodb"
}

func (s *Server) dbPublicHost(o *orgInfo, p *projectInfo, d *databaseInfo) string {
	return d.Slug + "-" + p.Slug + "-" + o.Slug + "-db." + s.cfg.AppsDomain
}

func (s *Server) loadDBPublic(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) (dbPublic, error) {
	v := dbPublic{Host: s.dbPublicHost(o, p, d), IP: s.cfg.PublicIP}
	err := tx.QueryRow(r.Context(), `
		SELECT d.public, d.allow_cidrs, d.proxy_cert, coalesce((SELECT public_port FROM public_ports WHERE database_id = d.id), 0)
		FROM databases d WHERE d.id = $1`, d.ID).Scan(&v.Enabled, &v.Allow, &v.Cert, &v.Port)
	return v, err
}

// parseAllowList checks the client addresses or ranges allowed to connect.
func parseAllowList(in string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(in, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == ';' || r == '\r' }) {
		if !strings.Contains(f, "/") {
			if a, err := netip.ParseAddr(f); err == nil {
				f = netip.PrefixFrom(a, a.BitLen()).String()
			}
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or a range such as 203.0.113.7 or 203.0.113.0/24", f)
		}
		out = append(out, p.Masked().String())
	}
	if len(out) == 0 {
		return nil, errors.New("give at least one address or range allowed to connect")
	}
	if len(out) > 20 {
		return nil, errors.New("at most 20 addresses or ranges")
	}
	return out, nil
}

// selfSigned makes the endpoint's certificate (10 years; the host name and the platform's
// address), returning the certificate and the key in PEM.
func selfSigned(host, ip string) (certPEM, keyPEM []byte, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, // self-signed: clients pin it as their root (sslrootcert)
	}
	if a := net.ParseIP(ip); a != nil {
		tmpl.IPAddresses = []net.IP{a}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// setDBPublicTx opens the database to the listed addresses (or closes it when allow is nil).
func (s *Server) setDBPublicTx(r *http.Request, tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo, allow []string) error {
	ctx := r.Context()
	target := "database:" + p.Slug + "/" + d.Slug
	if allow == nil {
		if _, err := tx.Exec(ctx, `UPDATE databases SET public = false WHERE id = $1`, d.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM public_ports WHERE database_id = $1`, d.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "database.public_closed", target, nil)
		_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', '')`)
		return err
	}
	if !publicEngine(d.Engine) {
		return apiErr{http.StatusUnprocessableEntity, "public access is available for PostgreSQL, Redis and MongoDB (MySQL, MariaDB and Kafka negotiate TLS in their own protocol)"}
	}
	// a public port from the platform's pool, counted with the services' TCP/UDP ports
	var have int
	_ = tx.QueryRow(ctx, `SELECT public_port FROM public_ports WHERE database_id = $1`, d.ID).Scan(&have)
	if have == 0 {
		var used, max int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM public_ports), p.max_public_ports FROM plans p WHERE p.id = $1`, o.PlanID).Scan(&used, &max); err != nil {
			return err
		}
		if used >= max {
			return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d public ports (services and databases together)", max)}
		}
		for cand := s.cfg.PublicPortMin; cand <= s.cfg.PublicPortMax && have == 0; cand++ {
			var taken bool
			if err := tx.QueryRow(ctx, `SELECT public_port_taken($1, 'tcp')`, cand).Scan(&taken); err != nil {
				return err
			}
			if taken {
				continue
			}
			tag, err := tx.Exec(ctx, `INSERT INTO public_ports (public_port, protocol, org_id, database_id, container_port) VALUES ($1, 'tcp', $2, $3, $4) ON CONFLICT DO NOTHING`,
				cand, o.ID, d.ID, d.Port())
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				have = cand
			}
		}
		if have == 0 {
			return apiErr{http.StatusServiceUnavailable, "no public port is free on this platform; ask the operator"}
		}
	}
	var cert string
	_ = tx.QueryRow(ctx, `SELECT proxy_cert FROM databases WHERE id = $1`, d.ID).Scan(&cert)
	if cert == "" {
		c, k, err := selfSigned(s.dbPublicHost(o, p, d), s.cfg.PublicIP)
		if err != nil {
			return err
		}
		enc, err := s.box.Seal(k, "dbproxy:"+d.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE databases SET proxy_cert = $2, proxy_key_enc = $3 WHERE id = $1`, d.ID, string(c), enc); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE databases SET public = true, allow_cidrs = $2 WHERE id = $1`, d.ID, allow); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "database.public_opened", target, map[string]any{"allow": allow, "port": have})
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', '')`)
	return err
}

func (s *Server) databasePublic(w http.ResponseWriter, r *http.Request) {
	var formErr, msg string
	o, p, d, err, ok := s.withDatabase(w, r, "admin", func(tx pgx.Tx, o *orgInfo, p *projectInfo, d *databaseInfo) error {
		if r.PostFormValue("close") == "1" {
			msg = "Public access closed: the database is reachable from its project only."
			return s.setDBPublicTx(r, tx, o, p, d, nil)
		}
		allow, perr := parseAllowList(r.PostFormValue("allow"))
		if perr != nil {
			formErr = perr.Error()
			return nil
		}
		msg = "Public access open to " + strings.Join(allow, ", ") + ". The endpoint starts within a minute."
		err := s.setDBPublicTx(r, tx, o, p, d, allow)
		var ae apiErr
		if errors.As(err, &ae) {
			formErr = ae.msg
			return nil
		}
		return err
	})
	if !ok {
		return
	}
	switch {
	case err != nil:
		s.serverError(w, r, err)
	case formErr != "":
		back(w, r, dbPath(o, p, d)+"#connect", "", formErr+".")
	default:
		back(w, r, dbPath(o, p, d)+"#connect", msg, "")
	}
}

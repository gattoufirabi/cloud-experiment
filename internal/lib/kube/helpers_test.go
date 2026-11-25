package kube

import (
	"encoding/pem"
	"net/http/httptest"
	"testing"
)

// pemCert returns the PEM certificate of an httptest TLS server.
func pemCert(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

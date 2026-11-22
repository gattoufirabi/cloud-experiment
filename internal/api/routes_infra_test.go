package api

import (
	"net/http"
	"testing"
)

// Registering every API route must not panic (two handlers for one pattern would).
func TestAPIRoutesRegister(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("API routes conflict: %v", p)
		}
	}()
	(&Server{}).apiRoutes(http.NewServeMux())
}

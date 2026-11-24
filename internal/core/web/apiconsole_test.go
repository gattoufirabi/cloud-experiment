package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFormValues(t *testing.T) {
	v, err := formValues(strings.NewReader(`{"slug": "web", "port": 8080, "private": true, "edge_cache": false, "events": ["created", "removed"], "env": "A=1\nB=2"}`))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"slug": "web", "port": "8080", "private": "1", "env": "A=1\nB=2"} {
		if got := v.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := v["edge_cache"]; ok {
		t.Error("false must be left out, like an unticked checkbox")
	}
	if len(v["events"]) != 2 {
		t.Errorf("events = %v", v["events"])
	}
}

func TestAPIOrgRoles(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/shop/services", nil)
	if _, st := apiOrgFrom(r, "viewer"); st != 0 {
		t.Fatal("a console request must use the session, not the API organization")
	}
	dev := &OrgInfo{ID: "o1", Slug: "acme", Role: "developer"}
	r = r.WithContext(context.WithValue(r.Context(), apiOrgKey, dev))
	if o, st := apiOrgFrom(r, "developer"); o == nil || st != http.StatusOK {
		t.Fatal("a developer token may do developer actions")
	}
	if o, st := apiOrgFrom(r, "admin"); o != nil || st != http.StatusForbidden {
		t.Fatal("a developer token must not do admin actions")
	}
	scoped := &OrgInfo{ID: "o1", Slug: "acme", Role: "developer", Scoped: true, Projects: map[string]string{"blog": "developer"}}
	r = httptest.NewRequest(http.MethodPost, "/api/v1/projects/shop/services", nil)
	r.SetPathValue("project", "shop")
	r = r.WithContext(context.WithValue(r.Context(), apiOrgKey, scoped))
	if o, _ := apiOrgFrom(r, "viewer"); o != nil {
		t.Fatal("a project token must not reach another project")
	}
}

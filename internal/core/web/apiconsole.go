package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"

	"platform/internal/lib/model"
)

// REST endpoints for console actions (creating projects, services and databases, saving
// settings, connecting resources...): the JSON body becomes the console form and the
// console's own handler runs, with the token's organization and role, so the API checks,
// limits and audit trail are exactly the console's. The handler's redirect becomes the
// answer: 200 {"message": ...} on success, 422 {"error": ...} for a refused form.

type apiOrgCtx struct{}

var apiOrgKey apiOrgCtx

// apiOrgFrom returns the organization of an API call run through a console handler.
func apiOrgFrom(r *http.Request, minRole string) (*OrgInfo, int) {
	o, ok := r.Context().Value(apiOrgKey).(*OrgInfo)
	if !ok {
		return nil, 0
	}
	c := *o
	if c.Scoped {
		proj := r.PathValue("project")
		role, ok := c.Projects[proj]
		if proj == "" || !ok {
			return nil, http.StatusForbidden
		}
		c.Role = role
	}
	if !model.RoleAtLeast(c.Role, minRole) {
		return nil, http.StatusForbidden
	}
	return &c, http.StatusOK
}

// formValues turns a JSON object into form values (true -> "1", lists -> repeated keys).
func formValues(body io.Reader) (url.Values, error) {
	var in map[string]any
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&in); err != nil && err != io.EOF {
		return nil, err
	}
	v := url.Values{}
	var add func(k string, x any)
	add = func(k string, x any) {
		switch t := x.(type) {
		case nil:
		case bool:
			if t {
				v.Add(k, "1")
			}
		case float64:
			v.Add(k, fmt.Sprint(t))
		case string:
			v.Add(k, t)
		case []any:
			for _, e := range t {
				add(k, e)
			}
		default:
			b, _ := json.Marshal(t)
			v.Add(k, string(b))
		}
	}
	for k, x := range in {
		add(k, x)
	}
	return v, nil
}

// apiConsole wraps a console handler as an API endpoint. extract, when set, reads
// values from a page the handler renders (a secret shown once, a webhook URL).
func (s *Server) APIConsole(h http.HandlerFunc, extract func(page string) map[string]string) func(http.ResponseWriter, *http.Request, *OrgInfo) {
	return func(w http.ResponseWriter, r *http.Request, o *OrgInfo) {
		form, err := formValues(r.Body)
		if err != nil {
			APIError(w, http.StatusBadRequest, "body must be a JSON object")
			return
		}
		r2 := r.WithContext(context.WithValue(r.Context(), apiOrgKey, o))
		r2.Body = io.NopCloser(strings.NewReader(""))
		r2.Header = r.Header.Clone()
		r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r2.PostForm, r2.Form = form, form
		r2.SetPathValue("org", o.Slug)
		rec := httptest.NewRecorder()
		h(rec, r2)
		res := rec.Result()
		loc := res.Header.Get("Location")
		switch {
		case res.StatusCode == http.StatusSeeOther || res.StatusCode == http.StatusFound:
			u, _ := url.Parse(loc)
			q := u.Query()
			if e := q.Get("err"); e != "" {
				APIError(w, http.StatusUnprocessableEntity, e)
				return
			}
			out := map[string]string{"message": q.Get("msg")}
			if out["message"] == "" {
				out["message"] = "done"
			}
			WriteJSON(w, out)
		case res.StatusCode == http.StatusOK && extract != nil:
			out := extract(rec.Body.String())
			if out == nil {
				APIError(w, http.StatusUnprocessableEntity, "the action did not complete")
				return
			}
			WriteJSON(w, out)
		case res.StatusCode < 300:
			WriteJSON(w, map[string]string{"message": "done"})
		case res.StatusCode == http.StatusNotFound:
			APIError(w, http.StatusNotFound, "not found")
		case res.StatusCode == http.StatusForbidden:
			APIError(w, http.StatusForbidden, "this token's role cannot do this")
		case res.StatusCode == http.StatusBadRequest:
			APIError(w, http.StatusUnprocessableEntity, "the request was refused")
		default:
			APIError(w, http.StatusInternalServerError, "internal error")
		}
	}
}

var WebhookURLRe = regexp.MustCompile(`https?://[^"<\s]+/hooks/fn/whk_[A-Za-z0-9_-]+`)

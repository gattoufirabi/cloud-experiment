// Package stalwart drives the platform's mail server (Stalwart v1) through its JMAP
// management API: domains, accounts (mailboxes) with aliases and quotas, DKIM keys,
// listeners, the server certificate. It holds no state; the platform's database is the
// source of truth.
package stalwart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const hour = int64(time.Hour / time.Millisecond) // durations are in milliseconds

// Client talks to one Stalwart server as its administrator.
type Client struct {
	URL, User, Password string
	HC                  *http.Client
}

// New returns a client for url ("http://stalwart:8080") with "user:password" credentials.
func New(url, admin string) *Client {
	user, pass, _ := strings.Cut(admin, ":")
	return &Client{URL: strings.TrimRight(url, "/"), User: user, Password: pass, HC: &http.Client{Timeout: 20 * time.Second}}
}

// Error is a method-level or set-level error reported by the server.
type Error struct {
	Type, Detail string
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return "mail server: " + e.Type + ": " + e.Detail
	}
	return "mail server: " + e.Type
}

// IsType reports whether err is a server error of type t (such as "primaryKeyViolation").
func IsType(err error, t string) bool {
	var e *Error
	return errors.As(err, &e) && e.Type == t
}

type call struct {
	method string
	args   any
}

// do runs method calls in one request and returns each response's arguments.
func (c *Client) do(ctx context.Context, calls ...call) ([]json.RawMessage, error) {
	mc := make([]any, len(calls))
	for i, cl := range calls {
		mc[i] = []any{cl.method, cl.args, fmt.Sprint(i)}
	}
	body, _ := json.Marshal(map[string]any{"using": []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"}, "methodCalls": mc})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/jmap/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mail server unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Type: fmt.Sprintf("HTTP %d", resp.StatusCode), Detail: strings.TrimSpace(string(raw[:min(len(raw), 300)]))}
	}
	var out struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	res := make([]json.RawMessage, 0, len(out.MethodResponses))
	for _, mr := range out.MethodResponses {
		var triple []json.RawMessage
		if err := json.Unmarshal(mr, &triple); err != nil || len(triple) < 2 {
			return nil, fmt.Errorf("mail server: malformed response")
		}
		var name string
		_ = json.Unmarshal(triple[0], &name)
		if name == "error" {
			var e struct{ Type, Description string }
			_ = json.Unmarshal(triple[1], &e)
			return nil, &Error{Type: e.Type, Detail: e.Description}
		}
		res = append(res, triple[1])
	}
	return res, nil
}

type setResult struct {
	Created      map[string]struct{ ID string } `json:"created"`
	NotCreated   map[string]setError            `json:"notCreated"`
	NotUpdated   map[string]setError            `json:"notUpdated"`
	NotDestroyed map[string]setError            `json:"notDestroyed"`
}

type setError struct {
	Type             string `json:"type"`
	Description      string `json:"description"`
	ValidationErrors []struct {
		Type, Property string
	} `json:"validationErrors"`
}

func (e setError) err() error {
	d := e.Description
	for _, v := range e.ValidationErrors {
		d = strings.TrimSpace(d + " " + v.Property + " " + v.Type)
	}
	return &Error{Type: e.Type, Detail: d}
}

// set runs one /set call and returns the server id of the object created as "new".
func (c *Client) set(ctx context.Context, object string, args map[string]any) (string, error) {
	res, err := c.do(ctx, call{"x:" + object + "/set", args})
	if err != nil {
		return "", err
	}
	var sr setResult
	if err := json.Unmarshal(res[0], &sr); err != nil {
		return "", err
	}
	for _, m := range []map[string]setError{sr.NotCreated, sr.NotUpdated, sr.NotDestroyed} {
		for _, e := range m {
			return "", e.err()
		}
	}
	return sr.Created["new"].ID, nil
}

func (c *Client) query(ctx context.Context, object string, filter map[string]any) ([]string, error) {
	args := map[string]any{}
	if filter != nil {
		args["filter"] = filter
	}
	res, err := c.do(ctx, call{"x:" + object + "/query", args})
	if err != nil {
		return nil, err
	}
	var q struct{ IDs []string }
	err = json.Unmarshal(res[0], &q)
	return q.IDs, err
}

// get reads objects (ids nil: all) into out, a pointer to a slice.
func (c *Client) get(ctx context.Context, object string, ids []string, props []string, out any) error {
	args := map[string]any{"ids": ids}
	if props != nil {
		args["properties"] = props
	}
	res, err := c.do(ctx, call{"x:" + object + "/get", args})
	if err != nil {
		return err
	}
	var g struct {
		List json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(res[0], &g); err != nil {
		return err
	}
	return json.Unmarshal(g.List, out)
}

// ---------- domains ----------

// Domain as the platform sees it.
type Domain struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enable      bool      `json:"enable"`
	CatchAll    string    `json:"catchAllAddress"`
	ZoneFile    string    `json:"dnsZoneFile"`
	CreatedAt   time.Time `json:"createdAt"`
}

// CreateDomain adds a domain with automatic DKIM keys; description tags it as the platform's.
func (c *Client) CreateDomain(ctx context.Context, name, description string) (string, error) {
	return c.set(ctx, "Domain", map[string]any{"create": map[string]any{"new": map[string]any{
		"name": name, "description": description, "enable": true,
		"certificateManagement": map[string]any{"@type": "Manual"},
		"dnsManagement":         map[string]any{"@type": "Manual"},
		"dkimManagement":        map[string]any{"@type": "Automatic"},
	}}})
}

// Domains lists every domain on the server.
func (c *Client) Domains(ctx context.Context) ([]Domain, error) {
	var ds []Domain
	err := c.get(ctx, "Domain", nil, []string{"name", "description", "enable", "catchAllAddress", "createdAt"}, &ds)
	return ds, err
}

// ZoneFile returns the DNS records the server expects for a domain (zone file text).
func (c *Client) ZoneFile(ctx context.Context, id string) (string, error) {
	var ds []Domain
	if err := c.get(ctx, "Domain", []string{id}, []string{"dnsZoneFile"}, &ds); err != nil {
		return "", err
	}
	if len(ds) == 0 {
		return "", &Error{Type: "notFound", Detail: "domain " + id}
	}
	return ds[0].ZoneFile, nil
}

// WaitZoneFile returns the zone file once the domain's DKIM keys exist (an Ed25519 and
// an RSA key, made in the background one after the other just after the domain is
// created; after 10 seconds it returns what there is).
func (c *Client) WaitZoneFile(ctx context.Context, id string) (string, error) {
	for i := 0; ; i++ {
		z, err := c.ZoneFile(ctx, id)
		if err != nil || strings.Count(z, "._domainkey.") >= 2 || i >= 20 {
			return z, err
		}
		select {
		case <-ctx.Done():
			return z, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// UpdateDomain changes a domain's fields (such as catchAllAddress or enable).
func (c *Client) UpdateDomain(ctx context.Context, id string, fields map[string]any) error {
	_, err := c.set(ctx, "Domain", map[string]any{"update": map[string]any{id: fields}})
	return err
}

// DeleteDomain removes a domain with its accounts and DKIM keys.
func (c *Client) DeleteDomain(ctx context.Context, id string) error {
	accts, err := c.query(ctx, "Account", map[string]any{"domainId": id})
	if err != nil {
		return err
	}
	if len(accts) > 0 {
		if _, err := c.set(ctx, "Account", map[string]any{"destroy": accts}); err != nil {
			return err
		}
	}
	keys, err := c.query(ctx, "DkimSignature", map[string]any{"domainId": id})
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		if _, err := c.set(ctx, "DkimSignature", map[string]any{"destroy": keys}); err != nil {
			return err
		}
	}
	_, err = c.set(ctx, "Domain", map[string]any{"destroy": []string{id}})
	if IsType(err, "notFound") {
		return nil
	}
	return err
}

// ---------- accounts ----------

// Account is a mailbox.
type Account struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Email  string           `json:"emailAddress"`
	Used   int64            `json:"usedDiskQuota"`
	Quotas map[string]int64 `json:"quotas"`
}

// Alias is an extra address of an account.
type Alias struct{ Name, DomainID string }

func aliasMap(as []Alias) map[string]any {
	m := map[string]any{}
	for i, a := range as {
		m[fmt.Sprint(i)] = map[string]any{"name": a.Name, "domainId": a.DomainID, "enable": true}
	}
	return m
}

// CreateAccount adds a mailbox name@domain with a password and a disk quota.
func (c *Client) CreateAccount(ctx context.Context, domainID, name, description, password string, quotaBytes int64) (string, error) {
	return c.set(ctx, "Account", map[string]any{"create": map[string]any{"new": map[string]any{
		"@type": "User", "name": name, "domainId": domainID, "description": nullIfEmpty(description),
		"credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": password}},
		"quotas":      map[string]any{"maxDiskQuota": quotaBytes},
	}}})
}

// SetPassword replaces the account's password (and signs out other credentials).
func (c *Client) SetPassword(ctx context.Context, id, password string) error {
	return c.updateAccount(ctx, id, map[string]any{"credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": password}}})
}

// SetQuota changes the account's disk quota.
func (c *Client) SetQuota(ctx context.Context, id string, quotaBytes int64) error {
	return c.updateAccount(ctx, id, map[string]any{"quotas/maxDiskQuota": quotaBytes})
}

// SetDescription changes the account's display name.
func (c *Client) SetDescription(ctx context.Context, id, description string) error {
	return c.updateAccount(ctx, id, map[string]any{"description": nullIfEmpty(description)})
}

// SetAliases replaces the account's extra addresses.
func (c *Client) SetAliases(ctx context.Context, id string, as []Alias) error {
	return c.updateAccount(ctx, id, map[string]any{"aliases": aliasMap(as)})
}

// nullIfEmpty: the server refuses empty strings where it accepts null.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (c *Client) updateAccount(ctx context.Context, id string, fields map[string]any) error {
	_, err := c.set(ctx, "Account", map[string]any{"update": map[string]any{id: fields}})
	return err
}

// DeleteAccount removes a mailbox and its mail.
func (c *Client) DeleteAccount(ctx context.Context, id string) error {
	_, err := c.set(ctx, "Account", map[string]any{"destroy": []string{id}})
	if IsType(err, "notFound") {
		return nil
	}
	return err
}

// Accounts reads mailboxes (with their disk use) by id.
func (c *Client) Accounts(ctx context.Context, ids []string) ([]Account, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var as []Account
	err := c.get(ctx, "Account", ids, []string{"name", "emailAddress", "usedDiskQuota", "quotas"}, &as)
	return as, err
}

// ---------- server setup ----------

// Setup makes sure the server greets as hostname, has the system domain hostname (the
// default domain the server requires) and listens for mail submission on port 587. It
// reports whether a listener was added (listeners start after a restart).
func (c *Client) Setup(ctx context.Context, hostname string) (restart bool, err error) {
	ds, err := c.Domains(ctx)
	if err != nil {
		return false, err
	}
	sysID := ""
	for _, d := range ds {
		if d.Name == hostname {
			sysID = d.ID
		}
	}
	if sysID == "" {
		if sysID, err = c.set(ctx, "Domain", map[string]any{"create": map[string]any{"new": map[string]any{
			"name": hostname, "description": "platform-system", "dkimManagement": map[string]any{"@type": "Automatic"},
		}}}); err != nil {
			return false, err
		}
	}
	if _, err := c.set(ctx, "SystemSettings", map[string]any{"update": map[string]any{"singleton": map[string]any{
		"defaultHostname": hostname, "defaultDomainId": sysID,
	}}}); err != nil {
		return false, err
	}
	// automatic bans expire (by default they are forever: a person who mistyped a
	// password too often would never get back in)
	if _, err := c.set(ctx, "Security", map[string]any{"update": map[string]any{"singleton": map[string]any{
		"authBanPeriod": hour, "abuseBanPeriod": hour, "loiterBanPeriod": hour, "scanBanPeriod": 24 * hour,
	}}}); err != nil {
		return false, err
	}
	var ls []struct {
		Name string `json:"name"`
	}
	if err := c.get(ctx, "NetworkListener", nil, []string{"name"}, &ls); err != nil {
		return false, err
	}
	if err := c.action(ctx, "ReloadSettings"); err != nil {
		return false, err
	}
	for _, l := range ls {
		if l.Name == "submission" {
			return false, nil
		}
	}
	_, err = c.set(ctx, "NetworkListener", map[string]any{"create": map[string]any{"new": map[string]any{
		"name": "submission", "bind": map[string]bool{"[::]:587": true}, "protocol": "smtp", "implicitTls": false, "useTls": true,
	}}})
	return err == nil, err
}

// SetCertificate installs a PEM certificate and key as the server's default (replacing
// the previous one) and reloads TLS.
func (c *Client) SetCertificate(ctx context.Context, certPEM, keyPEM string) error {
	var old []struct {
		ID string `json:"id"`
	}
	if err := c.get(ctx, "Certificate", nil, []string{"id"}, &old); err != nil {
		return err
	}
	id, err := c.set(ctx, "Certificate", map[string]any{"create": map[string]any{"new": map[string]any{
		"certificate": map[string]any{"@type": "Text", "value": certPEM},
		"privateKey":  map[string]any{"@type": "Text", "secret": keyPEM},
	}}})
	if err != nil {
		return err
	}
	if _, err := c.set(ctx, "SystemSettings", map[string]any{"update": map[string]any{"singleton": map[string]any{"defaultCertificateId": id}}}); err != nil {
		return err
	}
	if len(old) > 0 {
		ids := make([]string, len(old))
		for i, o := range old {
			ids[i] = o.ID
		}
		_, _ = c.set(ctx, "Certificate", map[string]any{"destroy": ids})
	}
	return c.action(ctx, "ReloadTlsCertificates")
}

func (c *Client) action(ctx context.Context, typ string) error {
	_, err := c.set(ctx, "Action", map[string]any{"create": map[string]any{"new": map[string]any{"@type": typ}}})
	return err
}

// Ping checks that the server answers with these credentials.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.query(ctx, "Domain", nil)
	return err
}

// ---------- operator views ----------

// QueueSize is the number of messages waiting in the outgoing queue.
func (c *Client) QueueSize(ctx context.Context) (int, error) {
	ids, err := c.query(ctx, "QueuedMessage", nil)
	return len(ids), err
}

// BlockedIP is an address the server banned (failed sign-ins, abuse, scans).
type BlockedIP struct {
	ID      string     `json:"id"`
	Address string     `json:"address"`
	Reason  string     `json:"reason"`
	Created time.Time  `json:"createdAt"`
	Expires *time.Time `json:"expiresAt"`
}

// BlockedIPs lists the banned addresses.
func (c *Client) BlockedIPs(ctx context.Context) ([]BlockedIP, error) {
	var out []BlockedIP
	err := c.get(ctx, "BlockedIp", nil, nil, &out)
	return out, err
}

// Unban lifts a ban and makes the server forget it at once.
func (c *Client) Unban(ctx context.Context, id string) error {
	if _, err := c.set(ctx, "BlockedIp", map[string]any{"destroy": []string{id}}); err != nil {
		return err
	}
	return c.action(ctx, "ReloadBlockedIps")
}

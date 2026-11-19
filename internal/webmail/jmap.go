package webmail

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

// A JMAP client for one signed-in person: every call carries their own address and
// password (the mail server checks them), so the webmail holds no privilege of its own.

var errAuth = errors.New("wrong address or password")

type jmapClient struct {
	base, user, pass string
	hc               *http.Client
	accountID        string
}

const usingMail = `["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"]`

// connect checks the credentials and finds the person's mail account.
func connect(ctx context.Context, base, user, pass string) (*jmapClient, error) {
	c := &jmapClient{base: strings.TrimRight(base, "/"), user: user, pass: pass, hc: &http.Client{Timeout: 30 * time.Second}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/jmap/session", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(user, pass)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the mail server is not answering")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errAuth
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the mail server answered HTTP %d", resp.StatusCode)
	}
	var s struct {
		PrimaryAccounts map[string]string `json:"primaryAccounts"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
		return nil, err
	}
	c.accountID = s.PrimaryAccounts["urn:ietf:params:jmap:mail"]
	if c.accountID == "" {
		return nil, errors.New("this account has no mailbox")
	}
	return c, nil
}

type invocation struct {
	Method string
	Args   map[string]any
}

// call runs method calls and returns each response's arguments by call id ("0", "1"...).
func (c *jmapClient) call(ctx context.Context, calls ...invocation) ([]json.RawMessage, error) {
	mc := make([]any, len(calls))
	for i, cl := range calls {
		if cl.Args == nil {
			cl.Args = map[string]any{}
		}
		cl.Args["accountId"] = c.accountID
		mc[i] = []any{cl.Method, cl.Args, fmt.Sprint(i)}
	}
	body, _ := json.Marshal(map[string]any{"using": json.RawMessage(usingMail), "methodCalls": mc})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/jmap/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the mail server is not answering")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errAuth
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the mail server answered HTTP %d", resp.StatusCode)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	res := make([]json.RawMessage, len(calls))
	for _, mr := range out.MethodResponses {
		if len(mr) < 3 {
			continue
		}
		var name, id string
		_ = json.Unmarshal(mr[0], &name)
		_ = json.Unmarshal(mr[2], &id)
		if name == "error" {
			var e struct{ Type, Description string }
			_ = json.Unmarshal(mr[1], &e)
			return nil, fmt.Errorf("the mail server refused: %s %s", e.Type, e.Description)
		}
		var i int
		if _, err := fmt.Sscan(id, &i); err == nil && i >= 0 && i < len(res) && res[i] == nil {
			res[i] = mr[1]
		}
	}
	return res, nil
}

// ---------- folders ----------

type folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Total     int    `json:"totalEmails"`
	Unread    int    `json:"unreadEmails"`
	SortOrder int    `json:"sortOrder"`
}

var roleOrder = map[string]int{"inbox": 0, "drafts": 1, "sent": 2, "archive": 3, "junk": 4, "trash": 5}

// Label is the folder's name for the menu.
func (f folder) Label() string {
	switch f.Role {
	case "inbox":
		return "Inbox"
	case "sent":
		return "Sent"
	case "drafts":
		return "Drafts"
	case "trash":
		return "Trash"
	case "junk":
		return "Spam"
	case "archive":
		return "Archive"
	}
	return f.Name
}

// Key is how the folder appears in URLs (its role, or its id).
func (f folder) Key() string {
	if f.Role != "" {
		return f.Role
	}
	return f.ID
}

func (c *jmapClient) folders(ctx context.Context) ([]folder, error) {
	res, err := c.call(ctx, invocation{"Mailbox/get", map[string]any{"properties": []string{"name", "role", "totalEmails", "unreadEmails", "sortOrder"}}})
	if err != nil {
		return nil, err
	}
	var g struct{ List []folder }
	if err := json.Unmarshal(res[0], &g); err != nil {
		return nil, err
	}
	fs := g.List
	order := func(f folder) int {
		if o, ok := roleOrder[f.Role]; ok {
			return o
		}
		return 10
	}
	for i := 1; i < len(fs); i++ { // small list: insertion sort by role, then name
		for j := i; j > 0 && (order(fs[j]) < order(fs[j-1]) || order(fs[j]) == order(fs[j-1]) && strings.ToLower(fs[j].Name) < strings.ToLower(fs[j-1].Name)); j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}
	return fs, nil
}

// ---------- messages ----------

type address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// String is "Name <email>" or the email alone.
func (a address) String() string {
	if a.Name != "" {
		return a.Name + " <" + a.Email + ">"
	}
	return a.Email
}

// Short is the name, or the email.
func (a address) Short() string {
	if a.Name != "" {
		return a.Name
	}
	return a.Email
}

type bodyPart struct {
	PartID      string `json:"partId"`
	BlobID      string `json:"blobId"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Disposition string `json:"disposition"`
	CID         string `json:"cid"`
}

type message struct {
	ID            string                            `json:"id"`
	ThreadID      string                            `json:"threadId"`
	MailboxIDs    map[string]bool                   `json:"mailboxIds"`
	Keywords      map[string]bool                   `json:"keywords"`
	From          []address                         `json:"from"`
	To            []address                         `json:"to"`
	Cc            []address                         `json:"cc"`
	ReplyTo       []address                         `json:"replyTo"`
	Subject       string                            `json:"subject"`
	Received      time.Time                         `json:"receivedAt"`
	Preview       string                            `json:"preview"`
	HasAttachment bool                              `json:"hasAttachment"`
	Size          int64                             `json:"size"`
	MessageID     []string                          `json:"messageId"`
	References    []string                          `json:"references"`
	TextBody      []bodyPart                        `json:"textBody"`
	HTMLBody      []bodyPart                        `json:"htmlBody"`
	Attachments   []bodyPart                        `json:"attachments"`
	BodyValues    map[string]struct{ Value string } `json:"bodyValues"`
}

func (m *message) Seen() bool    { return m.Keywords["$seen"] }
func (m *message) Flagged() bool { return m.Keywords["$flagged"] }

// Sender is the first From address.
func (m *message) Sender() address {
	if len(m.From) > 0 {
		return m.From[0]
	}
	return address{Email: "(no sender)"}
}

// Text is the plain-text body.
func (m *message) Text() string {
	var b strings.Builder
	for _, p := range m.TextBody {
		b.WriteString(m.BodyValues[p.PartID].Value)
	}
	return b.String()
}

// HTML is the HTML body ("" when the message is plain text only).
func (m *message) HTML() string {
	var b strings.Builder
	for _, p := range m.HTMLBody {
		if p.Type == "text/html" {
			b.WriteString(m.BodyValues[p.PartID].Value)
		}
	}
	return b.String()
}

// Files are the attachments shown to the person (not inline images).
func (m *message) Files() []bodyPart {
	var out []bodyPart
	for _, a := range m.Attachments {
		if a.Disposition == "inline" && a.CID != "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

var listProps = []string{"threadId", "mailboxIds", "keywords", "from", "to", "subject", "receivedAt", "preview", "hasAttachment"}

// list returns a page of a folder (or of a search over every folder).
func (c *jmapClient) list(ctx context.Context, folderID, text string, position, limit int) ([]message, int, error) {
	filter := map[string]any{}
	if folderID != "" {
		filter["inMailbox"] = folderID
	}
	if text != "" {
		filter["text"] = text
	}
	res, err := c.call(ctx,
		invocation{"Email/query", map[string]any{"filter": filter, "sort": []map[string]any{{"property": "receivedAt", "isAscending": false}},
			"position": position, "limit": limit, "calculateTotal": true}},
		invocation{"Email/get", map[string]any{"#ids": map[string]any{"resultOf": "0", "name": "Email/query", "path": "/ids"}, "properties": listProps}},
	)
	if err != nil {
		return nil, 0, err
	}
	var q struct{ Total int }
	_ = json.Unmarshal(res[0], &q)
	var g struct{ List []message }
	if err := json.Unmarshal(res[1], &g); err != nil {
		return nil, 0, err
	}
	return g.List, q.Total, nil
}

// get reads one message with its bodies.
func (c *jmapClient) get(ctx context.Context, id string) (*message, error) {
	props := append([]string{"cc", "replyTo", "size", "messageId", "references", "textBody", "htmlBody", "attachments", "bodyValues"}, listProps...)
	res, err := c.call(ctx, invocation{"Email/get", map[string]any{"ids": []string{id}, "properties": props,
		"fetchTextBodyValues": true, "fetchHTMLBodyValues": true, "maxBodyValueBytes": 2 << 20,
		"bodyProperties": []string{"partId", "blobId", "type", "name", "size", "disposition", "cid"}}})
	if err != nil {
		return nil, err
	}
	var g struct{ List []message }
	if err := json.Unmarshal(res[0], &g); err != nil {
		return nil, err
	}
	if len(g.List) == 0 {
		return nil, errNotFound
	}
	return &g.List[0], nil
}

var errNotFound = errors.New("this message does not exist")

// update changes messages (keywords, folders) with JMAP patches.
func (c *jmapClient) update(ctx context.Context, ids []string, patch map[string]any) error {
	up := map[string]any{}
	for _, id := range ids {
		up[id] = patch
	}
	_, err := c.call(ctx, invocation{"Email/set", map[string]any{"update": up}})
	return err
}

func (c *jmapClient) destroy(ctx context.Context, ids []string) error {
	_, err := c.call(ctx, invocation{"Email/set", map[string]any{"destroy": ids}})
	return err
}

type identity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (c *jmapClient) identities(ctx context.Context) ([]identity, error) {
	res, err := c.call(ctx, invocation{"Identity/get", nil})
	if err != nil {
		return nil, err
	}
	var g struct{ List []identity }
	err = json.Unmarshal(res[0], &g)
	return g.List, err
}

type outgoing struct {
	From                  address
	To, Cc, Bcc           []address
	Subject, Text         string
	InReplyTo, References []string
	Attachments           []uploaded
}

type uploaded struct {
	BlobID, Type, Name string
	Size               int64
}

// upload stores an attachment on the mail server and returns its blob.
func (c *jmapClient) upload(ctx context.Context, name, typ string, data []byte) (uploaded, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/jmap/upload/"+c.accountID+"/", bytes.NewReader(data))
	if err != nil {
		return uploaded{}, err
	}
	req.SetBasicAuth(c.user, c.pass)
	if typ == "" {
		typ = "application/octet-stream"
	}
	req.Header.Set("Content-Type", typ)
	resp, err := c.hc.Do(req)
	if err != nil {
		return uploaded{}, fmt.Errorf("the mail server is not answering")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return uploaded{}, fmt.Errorf("the attachment was refused (HTTP %d)", resp.StatusCode)
	}
	var u struct {
		BlobID string `json:"blobId"`
		Type   string `json:"type"`
		Size   int64  `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return uploaded{}, err
	}
	return uploaded{BlobID: u.BlobID, Type: typ, Name: name, Size: u.Size}, nil
}

// send creates the message in Sent and submits it.
func (c *jmapClient) send(ctx context.Context, idt identity, m outgoing, drafts, sent string) error {
	addrs := func(as []address) []map[string]string {
		out := []map[string]string{}
		for _, a := range as {
			out = append(out, map[string]string{"name": a.Name, "email": a.Email})
		}
		return out
	}
	email := map[string]any{
		"mailboxIds": map[string]bool{drafts: true},
		"keywords":   map[string]bool{"$draft": true, "$seen": true},
		"from":       []map[string]string{{"name": m.From.Name, "email": m.From.Email}},
		"to":         addrs(m.To), "subject": m.Subject,
		"textBody":   []map[string]string{{"partId": "t", "type": "text/plain"}},
		"bodyValues": map[string]any{"t": map[string]string{"value": m.Text}},
	}
	if len(m.Cc) > 0 {
		email["cc"] = addrs(m.Cc)
	}
	if len(m.Bcc) > 0 {
		email["bcc"] = addrs(m.Bcc)
	}
	if len(m.InReplyTo) > 0 {
		email["inReplyTo"] = m.InReplyTo
		email["references"] = m.References
	}
	if len(m.Attachments) > 0 {
		var atts []map[string]any
		for _, a := range m.Attachments {
			atts = append(atts, map[string]any{"blobId": a.BlobID, "type": a.Type, "name": a.Name, "disposition": "attachment"})
		}
		email["attachments"] = atts
	}
	update := map[string]any{"keywords/$draft": nil, "mailboxIds/" + drafts: nil}
	if sent != "" {
		update["mailboxIds/"+sent] = true
	}
	res, err := c.call(ctx,
		invocation{"Email/set", map[string]any{"create": map[string]any{"m": email}}},
		invocation{"EmailSubmission/set", map[string]any{"create": map[string]any{"s": map[string]any{"emailId": "#m", "identityId": idt.ID}},
			"onSuccessUpdateEmail": map[string]any{"#s": update}}},
	)
	if err != nil {
		return err
	}
	for _, r := range res[:2] {
		var sr struct {
			NotCreated map[string]struct{ Type, Description string } `json:"notCreated"`
		}
		if json.Unmarshal(r, &sr) == nil {
			for _, e := range sr.NotCreated {
				return fmt.Errorf("not sent: %s %s", e.Type, e.Description)
			}
		}
	}
	return nil
}

// download streams an attachment.
func (c *jmapClient) download(ctx context.Context, blobID, name, typ string) (*http.Response, error) {
	u := fmt.Sprintf("%s/jmap/download/%s/%s/%s?accept=%s", c.base, c.accountID, blobID, urlPath(name), urlQuery(typ))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.pass)
	return c.hc.Do(req)
}

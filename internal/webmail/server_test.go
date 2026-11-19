package webmail

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every page renders with realistic data (template errors otherwise show at run time).
func TestPagesRender(t *testing.T) {
	s, err := New(Config{MailURL: "http://mail.test"}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	me := &session{Email: "zoe@shop.test", Name: "Zoe", csrf: "tok"}
	fs := []folder{{ID: "a", Name: "Inbox", Role: "inbox", Total: 2, Unread: 1}, {ID: "e", Name: "Sent Items", Role: "sent"}, {ID: "b", Name: "Deleted Items", Role: "trash"}}
	m := message{ID: "m1", Subject: "Hello", From: []address{{Name: "Anna", Email: "anna@example.com"}}, To: []address{{Email: "zoe@shop.test"}},
		Received: time.Now(), Preview: "Hi Zoe", Keywords: map[string]bool{}, MailboxIDs: map[string]bool{"a": true},
		TextBody: []bodyPart{{PartID: "1", Type: "text/plain"}}, BodyValues: map[string]struct{ Value string }{"1": {Value: "Hi <Zoe>"}},
		Attachments: []bodyPart{{BlobID: "b1", Name: "invoice.pdf", Size: 2048}}}
	cases := map[string]data{
		"login":   {"Err": "Wrong email address or password.", "Email": "zoe@shop.test"},
		"list":    {"Me": me, "CSRF": "tok", "Folders": fs, "Folder": &fs[0], "Messages": []message{m}, "Total": 1, "Title": "Inbox", "HasNext": true, "Page": 1},
		"message": {"Me": me, "CSRF": "tok", "Folders": fs, "Folder": &fs[0], "M": &m, "Title": "Hello"},
		"compose": {"Me": me, "CSRF": "tok", "Folders": fs, "Title": "New message", "To": "anna@example.com", "MaxMB": 20},
		"error":   {"Me": me, "Folders": fs, "Text": "Nope"},
	}
	for page, d := range cases {
		rec := httptest.NewRecorder()
		s.render(rec, httptest.NewRequest("GET", "/x", nil), page, 200, d)
		out := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(out, "</html>") {
			t.Fatalf("%s: %d %s", page, rec.Code, out)
		}
		if page == "message" && (!strings.Contains(out, "Hi &lt;Zoe&gt;") || !strings.Contains(out, "invoice.pdf")) {
			t.Errorf("message page: body not escaped or attachment missing")
		}
		if page == "list" && (!strings.Contains(out, `href="/m/m1"`) || !strings.Contains(out, "?page=2")) {
			t.Errorf("list page: link or pager missing")
		}
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/f/inbox": "/f/inbox", "//evil.example": "", "https://evil.example": "", `/\evil`: ""} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

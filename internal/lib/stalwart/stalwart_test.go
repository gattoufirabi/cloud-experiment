package stalwart

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestAgainstServer runs against a real server: STALWART_TEST_URL=http://localhost:18080
// STALWART_TEST_ADMIN=admin:password go test ./internal/stalwart
func TestAgainstServer(t *testing.T) {
	url := os.Getenv("STALWART_TEST_URL")
	if url == "" {
		t.Skip("STALWART_TEST_URL not set")
	}
	c := New(url, os.Getenv("STALWART_TEST_ADMIN"))
	ctx := context.Background()
	if _, err := c.Setup(ctx, "mail.platform-test.test"); err != nil {
		t.Fatal("setup:", err)
	}
	if _, err := c.Setup(ctx, "mail.platform-test.test"); err != nil {
		t.Fatal("setup twice:", err)
	}
	id, err := c.CreateDomain(ctx, "unit-shop.test", "platform:test")
	if err != nil {
		t.Fatal("create domain:", err)
	}
	defer func() {
		if err := c.DeleteDomain(ctx, id); err != nil {
			t.Error("delete domain:", err)
		}
	}()
	zone, err := c.WaitZoneFile(ctx, id)
	if err != nil || !strings.Contains(zone, "_domainkey.unit-shop.test.") || !strings.Contains(zone, "MX 10 mail.platform-test.test.") {
		t.Fatalf("zone file: %v %q", err, zone)
	}
	acct, err := c.CreateAccount(ctx, id, "bob", "Bob", "Unit-test-pass-1", 50<<20)
	if err != nil {
		t.Fatal("create account:", err)
	}
	if _, err := c.CreateAccount(ctx, id, "nameless", "", "Unit-test-pass-1", 50<<20); err != nil {
		t.Error("account without a description:", err)
	}
	if _, err := c.CreateAccount(ctx, id, "bob", "Bob", "Unit-test-pass-1", 50<<20); err == nil {
		t.Error("duplicate account accepted")
	}
	if err := c.SetAliases(ctx, acct, []Alias{{"sales", id}}); err != nil {
		t.Fatal("aliases:", err)
	}
	if err := c.SetQuota(ctx, acct, 100<<20); err != nil {
		t.Fatal("quota:", err)
	}
	if err := c.SetDescription(ctx, acct, ""); err != nil {
		t.Fatal("clear description:", err)
	}
	if err := c.SetPassword(ctx, acct, "Unit-test-pass-2"); err != nil {
		t.Fatal("password:", err)
	}
	if err := c.UpdateDomain(ctx, id, map[string]any{"catchAllAddress": "bob@unit-shop.test"}); err != nil {
		t.Fatal("catch-all:", err)
	}
	as, err := c.Accounts(ctx, []string{acct})
	if err != nil || len(as) != 1 || as[0].Email != "bob@unit-shop.test" || as[0].Quotas["maxDiskQuota"] != 100<<20 {
		t.Fatalf("accounts: %v %+v", err, as)
	}
}

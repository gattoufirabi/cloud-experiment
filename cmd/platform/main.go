// Command platform runs every part of the platform from one binary:
//
//	platform api                 console + API (unprivileged, internet-facing via Caddy)
//	platform agent               node agent (privileged, never internet-facing)
//	platform migrate             apply database migrations (owner role)
//	platform healthcheck         container health probe for the API
//	platform admin <command>     operator commands (owner role), see "platform admin help"
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/agent"
	"platform/internal/api"
	"platform/internal/billing"
	"platform/internal/config"
	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/registry"
	"platform/internal/secure"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := config.Load()

	var err error
	switch os.Args[1] {
	case "api":
		err = api.Run(ctx, cfg)
	case "agent":
		err = agent.Run(ctx, cfg)
	case "migrate":
		err = migrate(ctx, cfg)
	case "healthcheck":
		err = healthcheck(cfg)
	case "admin":
		err = admin(ctx, cfg, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: platform api | agent | migrate | healthcheck | admin <command>")
	os.Exit(2)
}

func migrate(ctx context.Context, cfg config.Config) error {
	pool, err := db.Connect(ctx, cfg.AdminDatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return db.Migrate(ctx, pool)
}

func healthcheck(cfg config.Config) error {
	addr := cfg.InternalAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}

func admin(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println(`platform admin commands:
  init-key --file PATH                       create the master encryption key (never overwrites)
  create-org --slug S --name N --owner-email E [--plan starter|pro]
                                             create an organization and print the owner's invite link
  invite --org S --email E [--role owner|admin|developer|viewer]
                                             print a new invite link for an existing organization
  suspend-org --slug S [--undo]              stop (or restore) all applications of an organization
  set-plan --slug S --plan P                 change an organization's plan (leaving "trial" lifts a trial-ended suspension)
  trials                                     list trial organizations, their owner and status
  set-scan-policy --slug S --policy warn|block-critical
                                             refuse (or only report) images with critical vulnerabilities
  init-registry-key --file KEY --cert CERT   create the registry token signing key and certificate
  backup-key --slug S                        print an organization's backup repository and password (audited)
  set-prices --plan P [--monthly CENTS] [--vcpu-hour CENTS] [--gb-hour CENTS] [--gb-out CENTS]
                                             set a plan's prices (cents of BILLING_CURRENCY; decimals allowed)
  generate-invoices [--month YYYY-MM]        issue invoices for a finished month (default: last month; safe to rerun)
  invoice-paid --number N | invoice-void --number N
                                             record a payment, or cancel an invoice
  totp-code --secret BASE32                  print the current two-step code (testing only)`)
		return nil
	}
	fs := flag.NewFlagSet(args[0], flag.ExitOnError)
	file := fs.String("file", "", "")
	slug := fs.String("slug", "", "")
	orgSlug := fs.String("org", "", "")
	name := fs.String("name", "", "")
	email := fs.String("owner-email", "", "")
	inviteEmail := fs.String("email", "", "")
	role := fs.String("role", "developer", "")
	plan := fs.String("plan", "starter", "")
	undo := fs.Bool("undo", false, "")
	secret := fs.String("secret", "", "")
	certFile := fs.String("cert", "", "")
	policy := fs.String("policy", "", "")
	monthFlag := fs.String("month", "", "")
	number := fs.String("number", "", "")
	monthly := fs.Float64("monthly", -1, "")
	vcpuHour := fs.Float64("vcpu-hour", -1, "")
	gbHour := fs.Float64("gb-hour", -1, "")
	gbOut := fs.Float64("gb-out", -1, "")
	_ = fs.Parse(args[1:])

	switch args[0] {
	case "init-key":
		if *file == "" {
			return fmt.Errorf("--file is required")
		}
		if err := secure.GenerateKeyFile(*file); err != nil {
			return err
		}
		fmt.Println("master key written to", *file, "- back it up offline: without it, secrets cannot be decrypted")
		return nil
	case "init-registry-key":
		if *file == "" || *certFile == "" {
			return fmt.Errorf("--file (private key, API) and --cert (certificate, registry) are required")
		}
		if err := registry.GenerateKeyPair(*file, *certFile); err != nil {
			return err
		}
		fmt.Println("registry signing key written to", *file, "and certificate to", *certFile)
		return nil
	case "totp-code":
		code, err := secure.TOTPCode(*secret, time.Now())
		if err != nil {
			return err
		}
		fmt.Println(code)
		return nil
	}

	pool, err := db.Connect(ctx, cfg.AdminDatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch args[0] {
	case "create-org":
		if !model.ValidSlug(*slug) || *name == "" || !strings.Contains(*email, "@") {
			return fmt.Errorf("--slug (2-20 chars a-z0-9-), --name and --owner-email are required")
		}
		var orgID string
		token := secure.NewToken()
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `INSERT INTO orgs (slug, name, plan_id) VALUES ($1, $2, $3) RETURNING id`, *slug, *name, *plan).Scan(&orgID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO invites (org_id, email, role, token_hash, expires_at) VALUES ($1, $2, 'owner', $3, now() + interval '7 days')`,
				orgID, strings.ToLower(*email), secure.HashToken(token)); err != nil {
				return err
			}
			return db.Audit(ctx, tx, orgID, "", "org.created", "org:"+*slug, map[string]string{"plan": *plan, "owner": strings.ToLower(*email)}, "")
		})
		if err != nil {
			return err
		}
		fmt.Printf("organization %s created (plan %s)\nowner invite (valid 7 days, send it privately):\n%s/invite/%s\n", *slug, *plan, cfg.BaseURL, token)
	case "invite":
		if !model.ValidRole(*role) || !strings.Contains(*inviteEmail, "@") {
			return fmt.Errorf("--org, --email and a valid --role are required")
		}
		token := secure.NewToken()
		tag, err := pool.Exec(ctx, `INSERT INTO invites (org_id, email, role, token_hash, expires_at)
			SELECT id, $2, $3, $4, now() + interval '7 days' FROM orgs WHERE slug = $1`,
			*orgSlug, strings.ToLower(*inviteEmail), *role, secure.HashToken(token))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("organization %q not found", *orgSlug)
		}
		fmt.Printf("%s/invite/%s\n", cfg.BaseURL, token)
	case "suspend-org":
		tag, err := pool.Exec(ctx, `UPDATE orgs SET suspended = $2, suspend_reason = CASE WHEN $2 THEN 'operator' ELSE '' END WHERE slug = $1`, *slug, !*undo)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("organization %q not found", *slug)
		}
		_, _ = pool.Exec(ctx, `SELECT pg_notify('deployments', '')`)
		action := "org.suspended"
		if *undo {
			action = "org.restored"
		}
		_ = db.Audit(ctx, pool, "", "", action, "org:"+*slug, nil, "")
		fmt.Println(action, *slug)
	case "set-scan-policy":
		if *policy != "warn" && *policy != "block-critical" {
			return fmt.Errorf("--policy must be warn or block-critical")
		}
		tag, err := pool.Exec(ctx, `UPDATE orgs SET scan_policy = $2 WHERE slug = $1`, *slug, *policy)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("organization %q not found", *slug)
		}
		_ = db.Audit(ctx, pool, "", "", "org.scan_policy", "org:"+*slug, map[string]string{"policy": *policy}, "")
		fmt.Println("scan policy of", *slug, "set to", *policy)
	case "backup-key":
		// Disaster recovery: lets the operator open an organization's repository with
		// plain restic, without the platform. Printed once and audited.
		box, err := secure.LoadBox(cfg.MasterKeyFile)
		if err != nil {
			return err
		}
		var orgID string
		var enc []byte
		if err := pool.QueryRow(ctx, `SELECT id, backup_key_enc FROM orgs WHERE slug = $1`, *slug).Scan(&orgID, &enc); err != nil {
			return fmt.Errorf("organization %q not found", *slug)
		}
		if enc == nil {
			return fmt.Errorf("organization %q has no backups yet", *slug)
		}
		key, err := box.Open(enc, "backup:"+orgID)
		if err != nil {
			return err
		}
		_ = db.Audit(ctx, pool, orgID, "", "backup.key_exported", "org:"+*slug, nil, "")
		repo := "/repos/" + orgID + "   (inside the backup volume)"
		if cfg.BackupRestURL != "" {
			repo = cfg.BackupRestURL + "/" + orgID
		}
		fmt.Printf("repository: %s\npassword:   %s\n", repo, key)
	case "set-plan":
		// Leaving the trial ends its clock and lifts a "trial ended" suspension.
		tag, err := pool.Exec(ctx, `
			UPDATE orgs SET plan_id = $2,
			       trial_ends_at = CASE WHEN $2 = 'trial' THEN coalesce(trial_ends_at, now() + interval '14 days') END,
			       suspended = CASE WHEN suspend_reason = 'trial_ended' AND $2 <> 'trial' THEN false ELSE suspended END,
			       suspend_reason = CASE WHEN suspend_reason = 'trial_ended' AND $2 <> 'trial' THEN '' ELSE suspend_reason END
			WHERE slug = $1`, *slug, *plan)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("organization %q not found", *slug)
		}
		_, _ = pool.Exec(ctx, `SELECT pg_notify('deployments', '')`)
		_ = db.Audit(ctx, pool, "", "", "org.plan_changed", "org:"+*slug, map[string]string{"plan": *plan}, "")
		fmt.Println("plan of", *slug, "set to", *plan)
	case "trials":
		rows, err := pool.Query(ctx, `
			SELECT o.slug, coalesce((SELECT u.email FROM memberships m JOIN users u ON u.id = m.user_id WHERE m.org_id = o.id AND m.role = 'owner' LIMIT 1), ''),
			       o.created_at, o.trial_ends_at, o.suspended, o.suspend_reason
			FROM orgs o WHERE o.plan_id = 'trial' ORDER BY o.created_at DESC LIMIT 200`)
		if err != nil {
			return err
		}
		defer rows.Close()
		fmt.Printf("%-20s %-32s %-12s %-12s %s\n", "ORG", "OWNER", "CREATED", "TRIAL ENDS", "STATUS")
		for rows.Next() {
			var slugV, owner, reason string
			var created time.Time
			var ends *time.Time
			var susp bool
			if err := rows.Scan(&slugV, &owner, &created, &ends, &susp, &reason); err != nil {
				return err
			}
			st, end := "active", "-"
			if susp {
				st = "suspended (" + reason + ")"
			}
			if ends != nil {
				end = ends.Format("2006-01-02")
			}
			fmt.Printf("%-20s %-32s %-12s %-12s %s\n", slugV, owner, created.Format("2006-01-02"), end, st)
		}
		return rows.Err()
	case "set-prices":
		set := func(col string, v float64) error {
			if v < 0 {
				return nil
			}
			tag, err := pool.Exec(ctx, `UPDATE plans SET `+col+` = $2 WHERE id = $1`, *plan, v)
			if err == nil && tag.RowsAffected() == 0 {
				err = fmt.Errorf("plan %q not found", *plan)
			}
			return err
		}
		for col, v := range map[string]float64{"price_monthly_cents": math.Round(*monthly), "price_vcpu_hour_cents": *vcpuHour,
			"price_gb_hour_cents": *gbHour, "price_gb_out_cents": *gbOut} {
			if err := set(col, v); err != nil {
				return err
			}
		}
		var m int64
		var c, g, o float64
		if err := pool.QueryRow(ctx, `SELECT price_monthly_cents, price_vcpu_hour_cents::float8, price_gb_hour_cents::float8, price_gb_out_cents::float8 FROM plans WHERE id = $1`, *plan).
			Scan(&m, &c, &g, &o); err != nil {
			return err
		}
		fmt.Printf("%s: %s/month, %.4f c/vCPU-hour, %.4f c/GB-hour, %.4f c/GB out\n", *plan, billing.Money(m, cfg.BillingCurrency), c, g, o)
	case "generate-invoices":
		month := time.Now().UTC().AddDate(0, -1, 0)
		if *monthFlag != "" {
			t, err := time.Parse("2006-01", *monthFlag)
			if err != nil {
				return fmt.Errorf("--month must look like 2026-10")
			}
			month = t
		}
		n, err := billing.Generate(ctx, pool, cfg, month)
		if err != nil {
			return err
		}
		fmt.Printf("%d invoice(s) issued for %s\n", n, month.Format("2006-01"))
	case "invoice-paid", "invoice-void":
		status := strings.TrimPrefix(args[0], "invoice-")
		var orgID string
		err := pool.QueryRow(ctx, `
			UPDATE invoices SET status = $2, paid_at = CASE WHEN $2 = 'paid' THEN now() END
			WHERE number = $1 AND status = 'open' RETURNING org_id`, *number, status).Scan(&orgID)
		if err == pgx.ErrNoRows {
			return fmt.Errorf("no open invoice %q", *number)
		}
		if err != nil {
			return err
		}
		if err := db.Audit(ctx, pool, orgID, "", "invoice."+status, "invoice:"+*number, nil, ""); err != nil {
			return err
		}
		fmt.Println("invoice", *number, "marked", status)
	default:
		return fmt.Errorf("unknown admin command %q (try: platform admin help)", args[0])
	}
	return nil
}

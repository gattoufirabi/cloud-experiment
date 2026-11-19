package agent

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"platform/internal/stalwart"
)

// The mail server of hosted mailboxes (Stalwart, container platform-stalwart on the
// primary). The agent keeps it set up: its name (the mail host), the submission listener
// on port 587, the mail host's certificate copied from the proxy, and it removes domains
// the platform no longer has (an organization deleted) and turns off those of suspended
// organizations. Domains and mailboxes themselves are created by the API.

const stalwartContainer = "platform-stalwart"

func (a *Agent) mailHostLoop(ctx context.Context) {
	if !a.isPrimary() || !a.cfg.MailHostingEnabled() {
		return
	}
	ms := stalwart.New(a.cfg.StalwartURL, a.cfg.StalwartAdmin)
	var certMod time.Time
	nameMods := map[string]time.Time{} // own server names of mail domains -> installed certificate's date
	setup := false
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		if !setup {
			restart, err := ms.Setup(ctx, a.cfg.MailHost())
			switch {
			case err != nil:
				a.log.Warn("mail server setup", "err", err)
			case restart:
				a.log.Info("mail server: submission listener added, restarting it")
				if err := a.dk.ContainerStop(ctx, stalwartContainer, 20); err == nil {
					_ = a.dk.ContainerStart(ctx, stalwartContainer)
				}
				certMod = time.Time{} // and install the certificates again once it is back
				clear(nameMods)
			default:
				setup = true
			}
		}
		if setup {
			certMod = a.mailCertificate(ctx, ms, certMod)
			a.serverNameCertificates(ctx, ms, nameMods)
			a.reconcileMailDomains(ctx, ms)
			a.allowWebmail(ctx, ms)
		}
		wait := tick.C
		if !setup {
			wait = time.After(20 * time.Second) // the server is starting
		}
		select {
		case <-ctx.Done():
			return
		case <-wait:
		}
	}
}

// mailCertificate installs the proxy's certificate for the mail host when it changed.
func (a *Agent) mailCertificate(ctx context.Context, ms *stalwart.Client, last time.Time) time.Time {
	if a.cfg.MailCertDir == "" {
		return last
	}
	host := a.cfg.MailHost()
	var crt string
	_ = filepath.WalkDir(a.cfg.MailCertDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == host+".crt" {
			crt = p
			return fs.SkipAll
		}
		return nil
	})
	if crt == "" {
		return last
	}
	st, err := os.Stat(crt)
	if err != nil || !st.ModTime().After(last) {
		return last
	}
	cert, err1 := os.ReadFile(crt)
	key, err2 := os.ReadFile(strings.TrimSuffix(crt, ".crt") + ".key")
	if err1 != nil || err2 != nil {
		a.log.Warn("mail certificate: cannot read it", "path", crt)
		return last
	}
	if err := ms.SetCertificate(ctx, host, string(cert), string(key), true); err != nil {
		a.log.Warn("mail certificate: install", "err", err)
		return last
	}
	a.log.Info("mail certificate installed", "host", host, "valid_from", st.ModTime().Format(time.DateOnly))
	return st.ModTime()
}

// reconcileMailDomains deletes the platform's domains that no longer exist and turns
// off (or back on) those of suspended organizations.
func (a *Agent) reconcileMailDomains(ctx context.Context, ms *stalwart.Client) {
	rows, err := a.pool.Query(ctx, `SELECT md.id::text, o.suspended FROM mail_domains md JOIN orgs o ON o.id = md.org_id WHERE md.stalwart_id IS NOT NULL`)
	if err != nil {
		return
	}
	suspended := map[string]bool{}
	for rows.Next() {
		var id string
		var s bool
		if rows.Scan(&id, &s) == nil {
			suspended[id] = s
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	ds, err := ms.Domains(ctx)
	if err != nil {
		a.log.Warn("mail server: list domains", "err", err)
		return
	}
	for _, d := range ds {
		id, ok := strings.CutPrefix(d.Description, "platform:")
		if !ok {
			continue // the system domain, or one an operator made by hand
		}
		s, known := suspended[id]
		switch {
		case !known:
			if time.Since(d.CreatedAt) < 10*time.Minute {
				continue // created moments ago by an activation still in its transaction
			}
			var exists bool
			if a.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mail_domains WHERE id::text = $1)`, id).Scan(&exists) != nil || exists {
				continue
			}
			if err := ms.DeleteDomain(ctx, d.ID); err != nil {
				a.log.Warn("mail server: delete leftover domain", "domain", d.Name, "err", err)
			} else {
				a.log.Info("mail server: leftover domain deleted", "domain", d.Name)
			}
		case s == d.Enable:
			if err := ms.UpdateDomain(ctx, d.ID, map[string]any{"enable": !s}); err == nil {
				a.log.Info("mail server: domain switched", "domain", d.Name, "enabled", !s)
			}
		}
	}
}

// allowWebmail keeps the webmail's address on the mail server's allow list: every webmail
// sign-in reaches the mail server from that one address, so its failed sign-ins (which
// the webmail limits per visitor itself) must not ban everybody's webmail.
func (a *Agent) allowWebmail(ctx context.Context, ms *stalwart.Client) {
	cs, err := a.dk.ContainerList(ctx, "com.docker.compose.service=webmail")
	if err != nil {
		return
	}
	var ips []string
	for _, c := range cs {
		info, err := a.dk.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}
		for _, n := range info.NetworkSettings.Networks {
			if n.IPAddress != "" {
				ips = append(ips, n.IPAddress)
			}
		}
	}
	if changed, err := ms.SetAllowed(ctx, "platform webmail", ips); err != nil {
		a.log.Warn("mail server: allow the webmail", "err", err)
	} else if changed {
		a.log.Info("mail server: webmail address allowed", "addresses", ips)
	}
}

// certFile finds the proxy's certificate for host (its .crt; the .key is beside it).
func (a *Agent) certFile(host string) (string, time.Time) {
	var crt string
	_ = filepath.WalkDir(a.cfg.MailCertDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == host+".crt" {
			crt = p
			return fs.SkipAll
		}
		return nil
	})
	if crt == "" {
		return "", time.Time{}
	}
	st, err := os.Stat(crt)
	if err != nil {
		return "", time.Time{}
	}
	return crt, st.ModTime()
}

// serverNameCertificates installs on the mail server the certificate the proxy obtained
// for each mail domain's own server name (IMAP, POP3 and SMTP answer under that name),
// again when it is renewed, and removes those of names no longer used.
func (a *Agent) serverNameCertificates(ctx context.Context, ms *stalwart.Client, mods map[string]time.Time) {
	if a.cfg.MailCertDir == "" {
		return
	}
	names := a.loadMailServerNames(ctx)
	keep := map[string]bool{}
	for _, n := range names {
		keep[n] = true
		crt, mod := a.certFile(n)
		if crt == "" || !mod.After(mods[n]) {
			continue // not issued yet (the DNS may not point here yet), or unchanged
		}
		cert, err1 := os.ReadFile(crt)
		key, err2 := os.ReadFile(strings.TrimSuffix(crt, ".crt") + ".key")
		if err1 != nil || err2 != nil {
			continue
		}
		if err := ms.SetCertificate(ctx, n, string(cert), string(key), false); err != nil {
			a.log.Warn("mail certificate: install", "host", n, "err", err)
			continue
		}
		mods[n] = mod
		a.log.Info("mail certificate installed", "host", n)
	}
	for n := range mods {
		if !keep[n] {
			delete(mods, n)
		}
	}
	if err := ms.DropCertificates(ctx, keep, a.cfg.MailHost()); err != nil {
		a.log.Warn("mail certificates: clean up", "err", err)
	}
}

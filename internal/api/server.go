// Package api is the internet-facing console and API. It has no access to Docker or
// the host: it only reads and writes desired state in PostgreSQL, scoped by RLS.
package api

import (
	"github.com/go-webauthn/webauthn/webauthn"

	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"platform/internal/billing"
	"platform/internal/config"
	"platform/internal/db"
	"platform/internal/oidc"
	"platform/internal/registry"
	"platform/internal/secure"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg           config.Config
	pool          *pgxpool.Pool
	box           *secure.Box
	pages         map[string]*template.Template
	limiter       *rateLimiter
	log           *slog.Logger
	dummyHash     string
	hc            *http.Client
	regKey        *ecdsa.PrivateKey // signs registry tokens; nil when the registry is disabled
	assetV        string            // content hash of static files, for cache busting
	docs          map[string]docPage
	wa            *webauthn.WebAuthn
	settingsCache settingsCache
	ceremonies    *ceremonyStore
	pow           *powStore
	fnWait        fnWaiters // requests waiting for a function's result
	oidc          *oidc.Client
	oidcOnce      sync.Once
}

func Run(ctx context.Context, cfg config.Config) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	box, err := secure.LoadBox(cfg.MasterKeyFile)
	if err != nil {
		return err
	}
	pages, err := parseTemplates()
	if err != nil {
		return err
	}
	var regKey *ecdsa.PrivateKey
	if cfg.RegistryHost != "" {
		if regKey, err = registry.LoadKey(cfg.RegistryTokenKeyFile); err != nil {
			return fmt.Errorf("registry token key: %w", err)
		}
	}
	docs, err := loadDocs(cfg)
	if err != nil {
		return fmt.Errorf("load docs: %w", err)
	}
	wa, err := newWebAuthn(cfg)
	if err != nil {
		return fmt.Errorf("passkeys: %w", err)
	}
	dummy, _ := secure.HashPassword(secure.NewToken())
	s := &Server{
		cfg: cfg, pool: pool, box: box, pages: pages,
		limiter: newRateLimiter(), log: slog.Default().With("component", "api"),
		dummyHash: dummy, hc: &http.Client{Timeout: 10 * time.Second}, regKey: regKey,
		assetV: assetVersion(), docs: docs,
		wa: wa, ceremonies: &ceremonyStore{m: map[string]ceremony{}}, pow: newPowStore(),
	}

	public := &http.Server{
		Addr: cfg.ListenAddr, Handler: s.routes(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
	}
	internal := &http.Server{Addr: cfg.InternalAddr, Handler: s.internalRoutes(), ReadHeaderTimeout: 5 * time.Second}

	go s.housekeeping(ctx)
	go s.listenFnDone(ctx)
	go s.serveSMTP(ctx)
	errc := make(chan error, 2)
	go func() { errc <- public.ListenAndServe() }()
	go func() { errc <- internal.ListenAndServe() }()
	s.log.Info("listening", "public", cfg.ListenAddr, "internal", cfg.InternalAddr)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = public.Shutdown(sctx)
	_ = internal.Shutdown(sctx)
	return nil
}

func (s *Server) housekeeping(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() OR last_seen < now() - interval '2 hours'`)
			s.limiter.sweep()
		}
	}
}

func (s *Server) routes() http.Handler {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	m.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/docs/user", http.StatusSeeOther) })
	m.HandleFunc("GET /docs/{guide}", s.docsPage)
	m.HandleFunc("GET /status/{org}/{project}", s.statusPage)
	m.HandleFunc("POST /hooks/build/{token}", s.buildHook)
	m.HandleFunc("GET /signup", s.signupPage)
	m.HandleFunc("POST /signup", s.signupSubmit)
	m.HandleFunc("GET /signup/verify/{token}", s.signupVerifyPage)
	m.HandleFunc("POST /signup/verify/{token}", s.signupVerifySubmit)
	m.HandleFunc("GET /password/forgot", s.forgotPage)
	m.HandleFunc("POST /password/forgot", s.forgotSubmit)
	m.HandleFunc("GET /password/reset/{token}", s.resetPage)
	m.HandleFunc("POST /password/reset/{token}", s.resetSubmit)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.loginSubmit)
	m.HandleFunc("POST /sso/start", s.ssoStart)
	m.HandleFunc("GET /sso/callback", s.ssoCallback)
	m.HandleFunc("POST /logout", s.auth(authAny, s.logout))
	m.HandleFunc("GET /totp/setup", s.auth(authPassword, s.totpSetupPage))
	m.HandleFunc("POST /totp/setup", s.auth(authPassword, s.totpSetupSubmit))
	m.HandleFunc("GET /totp/verify", s.auth(authPassword, s.totpVerifyPage))
	m.HandleFunc("POST /totp/verify", s.auth(authPassword, s.totpVerifySubmit))
	m.HandleFunc("GET /invite/{token}", s.invitePage)
	m.HandleFunc("POST /invite/{token}", s.inviteSubmit)

	m.HandleFunc("GET /{$}", s.auth(authFull, s.home))
	m.HandleFunc("GET /o/{org}", s.auth(authFull, s.orgPage))
	m.HandleFunc("GET /o/{org}/services", s.auth(authFull, s.servicesPage))
	m.HandleFunc("POST /o/{org}/pins", s.auth(authFull, s.togglePin))
	m.HandleFunc("POST /o/{org}/sso", s.auth(authFull, s.saveSSO))
	m.HandleFunc("GET /o/{org}/scope", s.auth(authFull, s.setScope))
	m.HandleFunc("GET /o/{org}/search", s.auth(authFull, s.searchPage))
	m.HandleFunc("GET /o/{org}/projects", s.auth(authFull, s.orgArea("projects", "viewer")))
	m.HandleFunc("GET /o/{org}/iam", s.auth(authFull, s.orgArea("iam", "viewer")))
	m.HandleFunc("GET /o/{org}/registry", s.auth(authFull, s.orgArea("registry", "viewer")))
	m.HandleFunc("GET /o/{org}/monitoring", s.auth(authFull, s.orgArea("monitoring", "viewer")))
	m.HandleFunc("GET /o/{org}/compute", s.auth(authFull, s.computePage))
	m.HandleFunc("POST /o/{org}/compute", s.auth(authFull, s.createServiceFull))
	m.HandleFunc("POST /o/{org}/compute/actions", s.auth(authFull, s.computeActions))
	m.HandleFunc("GET /o/{org}/compute/jobs", s.auth(authFull, s.computeJobsPage))
	m.HandleFunc("GET /o/{org}/compute/previews", s.auth(authFull, s.computePreviewsPage))
	m.HandleFunc("GET /o/{org}/databases", s.auth(authFull, s.databasesPage))
	m.HandleFunc("POST /o/{org}/databases", s.auth(authFull, s.createDatabaseIn))
	m.HandleFunc("POST /o/{org}/email/suppressions", s.auth(authFull, s.emailSuppressionAction))
	m.HandleFunc("POST /o/{org}/email/webhook", s.auth(authFull, s.emailWebhookAction))
	m.HandleFunc("GET /o/{org}/domains", s.auth(authFull, s.domainsPage))
	m.HandleFunc("POST /o/{org}/domains", s.auth(authFull, s.addDomainFromList))
	m.HandleFunc("POST /o/{org}/domains/actions", s.auth(authFull, s.bulkActions("domains")))
	m.HandleFunc("POST /o/{org}/databases/actions", s.auth(authFull, s.bulkActions("databases")))
	m.HandleFunc("POST /o/{org}/functions/actions", s.auth(authFull, s.bulkActions("functions")))
	m.HandleFunc("POST /o/{org}/storage/actions", s.auth(authFull, s.bulkActions("storage")))
	m.HandleFunc("GET /o/{org}/monitoring/logs", s.auth(authFull, s.orgLogsPage))
	m.HandleFunc("GET /o/{org}/monitoring/logs.txt", s.auth(authFull, s.downloadLogs))
	m.HandleFunc("POST /o/{org}/monitoring/logs/exports", s.auth(authFull, s.createLogExport))
	m.HandleFunc("POST /o/{org}/monitoring/logs/exports/{id}", s.auth(authFull, s.updateLogExport))
	m.HandleFunc("GET /a/s.js", s.analyticsScriptHandler)
	m.HandleFunc("POST /a/e", s.analyticsCollect)
	m.HandleFunc("OPTIONS /a/e", s.analyticsCollect)
	m.HandleFunc("GET /o/{org}/activity.json", s.auth(authFull, s.activityJSON))
	m.HandleFunc("GET /o/{org}/dns", s.auth(authFull, s.dnsPage))
	m.HandleFunc("POST /o/{org}/dns", s.auth(authFull, s.addZone))
	m.HandleFunc("GET /o/{org}/dns/{zone}", s.auth(authFull, s.zonePage))
	m.HandleFunc("POST /o/{org}/dns/{zone}", s.auth(authFull, s.zoneAction))
	m.HandleFunc("GET /o/{org}/analytics", s.auth(authFull, s.analyticsPage))
	m.HandleFunc("POST /o/{org}/analytics/script", s.auth(authFull, s.analyticsScriptToggle))
	m.HandleFunc("POST /o/{org}/monitoring/grafana", s.auth(authFull, s.createGrafana))
	m.HandleFunc("POST /o/{org}/monitoring/alarms", s.auth(authFull, s.createAlarm))
	m.HandleFunc("POST /o/{org}/monitoring/alarms/{id}", s.auth(authFull, s.alarmAction))
	m.HandleFunc("GET /o/{org}/email", s.auth(authFull, s.emailPage))
	m.HandleFunc("POST /o/{org}/email/domains", s.auth(authFull, s.addEmailDomain))
	m.HandleFunc("POST /o/{org}/email/send", s.auth(authFull, s.sendEmailConsole))
	m.HandleFunc("POST /o/{org}/email/attach", s.auth(authFull, s.emailAttach))
	m.HandleFunc("GET /o/{org}/email/d/{domain}", s.auth(authFull, s.emailDomainPage))
	m.HandleFunc("POST /o/{org}/email/d/{domain}/check", s.auth(authFull, s.checkEmailDomain))
	m.HandleFunc("POST /o/{org}/email/d/{domain}/delete", s.auth(authFull, s.deleteEmailDomain))
	m.HandleFunc("GET /o/{org}/secrets", s.auth(authFull, s.orgSecretsPage))
	m.HandleFunc("POST /o/{org}/secrets", s.auth(authFull, s.createOrgSecret))
	m.HandleFunc("GET /o/{org}/secrets/{secret}", s.auth(authFull, s.orgSecretPage))
	m.HandleFunc("POST /o/{org}/secrets/{secret}/reveal", s.auth(authFull, s.revealOrgSecret))
	m.HandleFunc("POST /o/{org}/secrets/{secret}/version", s.auth(authFull, s.rotateOrgSecret))
	m.HandleFunc("POST /o/{org}/secrets/{secret}/delete", s.auth(authFull, s.deleteOrgSecret))
	m.HandleFunc("GET /o/{org}/queues", s.auth(authFull, s.queuesPage))
	m.HandleFunc("POST /o/{org}/queues", s.auth(authFull, s.createQueue))
	m.HandleFunc("GET /o/{org}/queues/{queue}", s.auth(authFull, s.queuePage))
	m.HandleFunc("POST /o/{org}/queues/{queue}/send", s.auth(authFull, s.queueSendConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/settings", s.auth(authFull, s.queueSettingsConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/purge", s.auth(authFull, s.queuePurgeConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/delete", s.auth(authFull, s.queueDeleteConsole))
	m.HandleFunc("POST /o/{org}/queues/{queue}/attach", s.auth(authFull, s.queueAttach))
	m.HandleFunc("GET /o/{org}/functions", s.auth(authFull, s.functionsPage))
	m.HandleFunc("POST /o/{org}/functions", s.auth(authFull, s.createFunction))
	const fnp = "/o/{org}/p/{project}/f/{fn}"
	m.HandleFunc("GET "+fnp, s.auth(authFull, s.functionPage))
	m.HandleFunc("POST "+fnp+"/code", s.auth(authFull, s.saveFunctionCode))
	m.HandleFunc("POST "+fnp+"/test", s.auth(authFull, s.testFunction))
	m.HandleFunc("POST "+fnp+"/settings", s.auth(authFull, s.functionSettings))
	m.HandleFunc("POST "+fnp+"/token", s.auth(authFull, s.newFunctionToken))
	m.HandleFunc("POST "+fnp+"/delete", s.auth(authFull, s.deleteFunction))
	m.HandleFunc("POST "+fnp+"/triggers", s.auth(authFull, s.addBucketTrigger))
	m.HandleFunc("POST "+fnp+"/triggers/{id}/delete", s.auth(authFull, s.deleteBucketTrigger))
	m.HandleFunc("POST "+fnp+"/webhook", s.auth(authFull, s.functionWebhook))
	m.HandleFunc("POST /hooks/fn/{token}", s.functionWebhookCall)
	m.HandleFunc("GET /o/{org}/audit", s.auth(authFull, s.auditPage))
	m.HandleFunc("GET /operator", s.operatorOnly(s.operatorPage))
	m.HandleFunc("POST /operator/orgs/{slug}/plan", s.operatorOnly(s.operatorSetPlan))
	m.HandleFunc("POST /operator/orgs/{slug}/suspend", s.operatorOnly(s.operatorSuspend))
	m.HandleFunc("POST /operator/invoices/{number}", s.operatorOnly(s.operatorInvoice))
	m.HandleFunc("POST /operator/nodes/{id}", s.operatorOnly(s.operatorSetNode))
	m.HandleFunc("POST /operator/nodes/{id}/drain", s.operatorOnly(s.operatorDrainNode))
	m.HandleFunc("POST /operator/orgs/{slug}/cluster", s.operatorOnly(s.operatorOrgCluster))
	m.HandleFunc("GET /operator/orgs/{slug}", s.operatorOnly(s.operatorOrgPage))
	m.HandleFunc("GET /operator/settings", s.operatorOnly(s.operatorSettingsPage))
	m.HandleFunc("POST /operator/settings", s.operatorOnly(s.operatorSaveSettings))
	m.HandleFunc("GET /operator/plans", s.operatorOnly(s.operatorPlansPage))
	m.HandleFunc("POST /operator/plans", s.operatorOnly(s.operatorCreatePlan))
	m.HandleFunc("POST /operator/plans/{id}", s.operatorOnly(s.operatorUpdatePlan))
	m.HandleFunc("POST /operator/plans/{id}/delete", s.operatorOnly(s.operatorDeletePlan))
	m.HandleFunc("GET /operator/resources", s.operatorOnly(s.operatorResourcesPage))
	m.HandleFunc("GET /operator/health", s.operatorOnly(s.operatorHealthPage))
	m.HandleFunc("POST /operator/invoices/generate", s.operatorOnly(s.operatorGenerateInvoices))
	m.HandleFunc("GET /operator/users", s.operatorOnly(s.operatorUsersPage))
	m.HandleFunc("POST /operator/users", s.operatorOnly(s.operatorCreateUser))
	m.HandleFunc("POST /operator/invites/{id}/revoke", s.operatorOnly(s.operatorRevokeInvite))
	m.HandleFunc("GET /operator/users/{id}", s.operatorOnly(s.operatorUserPage))
	m.HandleFunc("POST /operator/users/{id}/disable", s.operatorOnly(s.operatorUserDisable))
	m.HandleFunc("POST /operator/users/{id}/signout", s.operatorOnly(s.operatorUserSignout))
	m.HandleFunc("POST /operator/users/{id}/unlock", s.operatorOnly(s.operatorUserUnlock))
	m.HandleFunc("POST /operator/users/{id}/reset-password", s.operatorOnly(s.operatorUserResetPassword))
	m.HandleFunc("POST /operator/users/{id}/reset-2fa", s.operatorOnly(s.operatorUserReset2FA))
	m.HandleFunc("POST /operator/users/{id}/profile", s.operatorOnly(s.operatorUserProfile))
	m.HandleFunc("POST /operator/users/{id}/operator", s.operatorOnly(s.operatorUserOperator))
	m.HandleFunc("POST /operator/users/{id}/membership", s.operatorOnly(s.operatorUserMembership))
	m.HandleFunc("POST /operator/users/{id}/delete", s.operatorOnly(s.operatorUserDelete))
	m.HandleFunc("GET /o/{org}/billing", s.auth(authFull, s.billingPage))
	m.HandleFunc("POST /o/{org}/billing/details", s.auth(authFull, s.billingDetails))
	m.HandleFunc("POST /o/{org}/billing/budgets", s.auth(authFull, s.saveBudget))
	m.HandleFunc("POST /o/{org}/billing/budgets/{id}/delete", s.auth(authFull, s.deleteBudget))
	m.HandleFunc("GET /o/{org}/invoices/{number}", s.auth(authFull, s.invoicePage))
	m.HandleFunc("POST /o/{org}/projects", s.auth(authFull, s.createProject))
	m.HandleFunc("POST /o/{org}/invites", s.auth(authFull, s.createInvite))
	m.HandleFunc("POST /o/{org}/members/{user}/remove", s.auth(authFull, s.removeMember))
	m.HandleFunc("POST /o/{org}/members/{user}/access", s.auth(authFull, s.setMemberAccess))
	m.HandleFunc("POST /o/{org}/registry-tokens", s.auth(authFull, s.createRegistryToken))
	m.HandleFunc("POST /o/{org}/api-tokens", s.auth(authFull, s.createAPIToken))
	m.HandleFunc("POST /o/{org}/api-tokens/{id}/delete", s.auth(authFull, s.deleteAPIToken))
	m.HandleFunc("POST /o/{org}/alerts/channels", s.auth(authFull, s.createChannel))
	m.HandleFunc("POST /o/{org}/alerts/channels/{id}/delete", s.auth(authFull, s.deleteChannel))
	m.HandleFunc("POST /o/{org}/alerts/channels/{id}/test", s.auth(authFull, s.testChannel))
	m.HandleFunc("POST /o/{org}/registry-tokens/{id}/delete", s.auth(authFull, s.deleteRegistryToken))

	// Registry token endpoint (Docker registry auth). Caddy exposes it on the registry host.
	m.HandleFunc("GET /token", s.registryToken)

	m.HandleFunc("GET /o/{org}/p/{project}", s.auth(authFull, s.projectPage))
	m.HandleFunc("POST /o/{org}/p/{project}/services", s.auth(authFull, s.createService))
	m.HandleFunc("POST /o/{org}/p/{project}/delete", s.auth(authFull, s.deleteProject))
	m.HandleFunc("POST /o/{org}/p/{project}/status-page", s.auth(authFull, s.toggleStatusPage))
	m.HandleFunc("POST /o/{org}/p/{project}/variables", s.auth(authFull, s.setProjectVar))
	m.HandleFunc("POST /o/{org}/p/{project}/variables/{name}/delete", s.auth(authFull, s.deleteProjectVar))
	m.HandleFunc("POST /o/{org}/p/{project}/redeploy", s.auth(authFull, s.redeployProject))
	m.HandleFunc("POST /o/{org}/p/{project}/connections", s.auth(authFull, s.projectConnection))
	m.HandleFunc("GET /o/{org}/p/{project}/map", s.auth(authFull, s.projectMapFragment))

	m.HandleFunc("POST /o/{org}/p/{project}/databases", s.auth(authFull, s.createDatabase))
	const dbp = "/o/{org}/p/{project}/d/{db}"
	m.HandleFunc("GET "+dbp, s.auth(authFull, s.databasePage))
	m.HandleFunc("POST "+dbp+"/settings", s.auth(authFull, s.databaseSettings))
	m.HandleFunc("POST "+dbp+"/attach", s.auth(authFull, s.attachDatabase))
	m.HandleFunc("POST "+dbp+"/public", s.auth(authFull, s.databasePublic))
	m.HandleFunc("POST "+dbp+"/backups", s.auth(authFull, s.databaseBackupNow))
	m.HandleFunc("POST "+dbp+"/backups/restore/{snap}", s.auth(authFull, s.databaseRestore))
	m.HandleFunc("POST "+dbp+"/delete", s.auth(authFull, s.deleteDatabase))

	const st = "/o/{org}/storage"
	m.HandleFunc("GET "+st, s.auth(authFull, s.storagePage))
	m.HandleFunc("POST "+st+"/buckets", s.auth(authFull, s.createBucket))
	m.HandleFunc("POST "+st+"/keys", s.auth(authFull, s.createStorageKey))
	m.HandleFunc("POST "+st+"/keys/{id}/delete", s.auth(authFull, s.deleteStorageKey))
	m.HandleFunc("GET "+st+"/b/{bucket}", s.auth(authFull, s.bucketPage))
	m.HandleFunc("POST "+st+"/b/{bucket}/settings", s.auth(authFull, s.bucketSettings))
	m.HandleFunc("POST "+st+"/b/{bucket}/attach", s.auth(authFull, s.attachBucket))
	m.HandleFunc("POST "+st+"/b/{bucket}/delete", s.auth(authFull, s.deleteBucket))
	m.HandleFunc("POST "+st+"/b/{bucket}/objects/upload", s.auth(authFull, s.uploadObject))
	m.HandleFunc("POST "+st+"/b/{bucket}/objects/delete", s.auth(authFull, s.deleteObject))
	m.HandleFunc("GET "+st+"/b/{bucket}/objects/download", s.auth(authFull, s.downloadObject))

	const svc = "/o/{org}/p/{project}/s/{service}"
	m.HandleFunc("GET "+svc, s.auth(authFull, s.servicePage))
	m.HandleFunc("POST "+svc+"/settings", s.auth(authFull, s.saveSettings))
	m.HandleFunc("POST "+svc+"/deploy", s.auth(authFull, s.deployService))
	m.HandleFunc("POST "+svc+"/rollback/{rev}", s.auth(authFull, s.rollback))
	m.HandleFunc("POST "+svc+"/secrets", s.auth(authFull, s.setSecret))
	m.HandleFunc("POST "+svc+"/jobs", s.auth(authFull, s.saveJobForm))
	m.HandleFunc("POST "+svc+"/schedules", s.auth(authFull, s.addScheduleForm))
	m.HandleFunc("POST "+svc+"/schedules/{id}/delete", s.auth(authFull, s.deleteScheduleForm))
	m.HandleFunc("POST "+svc+"/jobs/{job}/{action}", s.auth(authFull, s.jobAction))
	m.HandleFunc("GET "+svc+"/job-runs/{run}", s.auth(authFull, s.jobRunLog))
	m.HandleFunc("POST "+svc+"/secrets/{name}/delete", s.auth(authFull, s.deleteSecret))
	m.HandleFunc("POST "+svc+"/domains", s.auth(authFull, s.addDomain))
	m.HandleFunc("POST "+svc+"/default-address", s.auth(authFull, s.setDefaultAddress))
	m.HandleFunc("POST "+svc+"/domains/{id}/verify", s.auth(authFull, s.verifyDomain))
	m.HandleFunc("POST "+svc+"/domains/{id}/delete", s.auth(authFull, s.deleteDomain))
	m.HandleFunc("POST "+svc+"/domains/{id}/paths", s.auth(authFull, s.addDomainPathForm))
	m.HandleFunc("POST "+svc+"/domains/paths/{path}/delete", s.auth(authFull, s.deleteDomainPathForm))
	m.HandleFunc("POST "+svc+"/ports", s.auth(authFull, s.addServicePortForm))
	m.HandleFunc("POST "+svc+"/ports/{label}/delete", s.auth(authFull, s.deleteServicePortForm))
	m.HandleFunc("POST "+svc+"/delete", s.auth(authFull, s.deleteService))
	m.HandleFunc("POST "+svc+"/backups", s.auth(authFull, s.backupNow))
	m.HandleFunc("POST "+svc+"/alerts", s.auth(authFull, s.alertThresholds))
	m.HandleFunc("POST "+svc+"/protection", s.auth(authFull, s.protectionSettings))
	m.HandleFunc("POST "+svc+"/previews", s.auth(authFull, s.createPreviewForm))
	m.HandleFunc("POST "+svc+"/source", s.auth(authFull, s.saveSource))
	m.HandleFunc("POST "+svc+"/builds", s.auth(authFull, s.startBuildForm))
	m.HandleFunc("POST "+svc+"/zip", s.auth(authFull, s.zipDeployForm))
	m.HandleFunc("GET "+svc+"/builds/{n}", s.auth(authFull, s.buildPage))
	m.HandleFunc("POST "+svc+"/build-hook", s.auth(authFull, s.rotateBuildHook))
	m.HandleFunc("POST "+svc+"/previews/{name}/delete", s.auth(authFull, s.deletePreviewForm))
	m.HandleFunc("POST "+svc+"/backups/settings", s.auth(authFull, s.backupSettings))
	m.HandleFunc("POST "+svc+"/backups/restore/{snap}", s.auth(authFull, s.restoreSnapshot))
	m.HandleFunc("GET "+svc+"/status.json", s.auth(authFull, s.statusJSON))
	m.HandleFunc("GET "+svc+"/logs.json", s.auth(authFull, s.logsJSON))
	m.HandleFunc("GET "+svc+"/metrics.json", s.auth(authFull, s.metricsJSON))

	s.apiRoutes(m)
	s.passkeyRoutes(m)
	return s.functionHosts(s.secureHeaders(m))
}

func assetVersion() string {
	h := sha256.New()
	for _, f := range []string{"static/app.css", "static/app.js"} {
		b, _ := staticFS.ReadFile(f)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if r.Method == http.MethodPost {
			limit := int64(1 << 20)
			switch {
			case strings.HasSuffix(r.URL.Path, "/objects/upload"): // console uploads to a bucket
				limit = maxUploadBytes + 1<<20
			case strings.HasSuffix(r.URL.Path, "/code"): // function code archive
				limit = 11 << 20
			case strings.HasSuffix(r.URL.Path, "/invoke"): // function event
				limit = 7 << 20
			case strings.HasSuffix(r.URL.Path, "/zip"): // zip deployment
				limit = maxZipDeployBytes + 1<<20
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- sessions ----------

type session struct {
	TokenHash   []byte
	UserID      string
	Email       string
	Name        string
	CSRF        string
	TOTPOK      bool
	TOTPEnabled bool
	IsOperator  bool
}

type ctxKey int

const sessKey ctxKey = 1

const (
	authAny = iota
	authPassword
	authFull
)

func (s *Server) cookieName() string {
	if s.cfg.CookieSecure {
		return "__Host-sid"
	}
	return "sid"
}

func (s *Server) loadSession(r *http.Request) *session {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil
	}
	sess := &session{TokenHash: secure.HashToken(c.Value)}
	err = s.pool.QueryRow(r.Context(), `
		UPDATE sessions se SET last_seen = now()
		FROM users u
		WHERE se.token_hash = $1 AND u.id = se.user_id AND u.disabled_at IS NULL
		  AND se.expires_at > now() AND se.last_seen > now() - interval '2 hours'
		RETURNING se.user_id, u.email, u.name, se.csrf, se.totp_ok, u.totp_enabled, u.is_operator`, sess.TokenHash).
		Scan(&sess.UserID, &sess.Email, &sess.Name, &sess.CSRF, &sess.TOTPOK, &sess.TOTPEnabled, &sess.IsOperator)
	if err != nil {
		return nil
	}
	return sess
}

func sessionFrom(r *http.Request) *session {
	s, _ := r.Context().Value(sessKey).(*session)
	return s
}

func (s *Server) auth(level int, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.loadSession(r)
		if sess == nil {
			if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, ".json") {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				http.Error(w, "session expired, please log in again", http.StatusUnauthorized)
			}
			return
		}
		if level == authFull && !sess.TOTPOK {
			if sess.TOTPEnabled {
				http.Redirect(w, r, "/totp/verify", http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/totp/setup", http.StatusSeeOther)
			}
			return
		}
		if r.Method == http.MethodPost && !s.validCSRF(r, sess) {
			http.Error(w, "invalid or missing CSRF token: reload the page and try again", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), sessKey, sess)))
	}
}

func (s *Server) validCSRF(r *http.Request, sess *session) bool {
	if o := r.Header.Get("Origin"); o != "" && o != "null" && o != s.cfg.BaseURL {
		return false
	}
	tok := r.Header.Get("X-CSRF-Token") // JSON requests (passkeys)
	if tok == "" {
		tok = r.PostFormValue("csrf")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRF)) == 1
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string, totpOK bool) error {
	token := secure.NewToken()
	// A disabled account gets no session, whatever the way it signed in.
	tag, err := s.pool.Exec(r.Context(), `
		INSERT INTO sessions (token_hash, user_id, csrf, totp_ok, ip, expires_at)
		SELECT $1, id, $3, $4, $5, now() + interval '12 hours' FROM users WHERE id = $2 AND disabled_at IS NULL`,
		secure.HashToken(token), userID, secure.NewToken(), totpOK, clientIP(r))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errAccountDisabled
	}
	_, _ = s.pool.Exec(r.Context(), `UPDATE users SET last_login_at = now() WHERE id = $1`, userID)
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", MaxAge: 12 * 3600,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

var errAccountDisabled = errors.New("account disabled")

// sessionError answers a failed startSession: a clear message for a disabled account.
func (s *Server) sessionError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errAccountDisabled) {
		s.render(w, r, "login", http.StatusForbidden, data{"Err": "This account is disabled. Contact the platform's support."})
		return
	}
	s.serverError(w, r, err)
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
}

// clientIP: the API is only reachable through Caddy, which sets X-Forwarded-For to the
// real client address (it does not trust incoming values), so the last entry is used.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- rendering ----------

func parseTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"ago": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return pluralize(int(d.Minutes()), "minute") + " ago"
			case d < 48*time.Hour:
				return pluralize(int(d.Hours()), "hour") + " ago"
			default:
				return t.Format("2 Jan 2006")
			}
		},
		"datetime":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
		"roleAtLeast": func(role, min string) bool { return roleAtLeast(role, min) },
		"money":       func(cents int64, cur string) string { return billing.Money(cents, cur) },
		"qty":         func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) },
		"unitprice":   func(c float64) string { return strconv.FormatFloat(c/100, 'f', -1, 64) },
		"lines":       func(s string) []string { return strings.Split(s, "\n") },
		"month":       func(t time.Time) string { return t.Format("January 2006") },
		"day":         func(t time.Time) string { return t.Format("2 Jan 2006") },
		"buildStatus": func(s string) string {
			switch s {
			case "done":
				return "live"
			case "running":
				return "deploying"
			case "queued":
				return "pending"
			}
			return s
		},
		"jobStatus":  func(s string) string { return s }, // succeeded, failed, timed_out, skipped, running
		"engineName": engineName,
		"bytes":      humanBytes,
		"icon":       icon,
		"runtime":    runtimeLabel,
		"list":       func(xs ...int) []int { return xs },
		"contains":   strings.Contains,
		"join":       strings.Join,
		"strs":       func(xs ...string) []string { return xs },
		"pct":        percentOf,
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"hoursOf": func(sec int) int { return max(1, sec/3600) },
		"int64":   func(i int) int64 { return int64(i) },
		"ms":      itoaPtr,
		"dbStatus": func(s string) string {
			if s == "running" {
				return "live"
			}
			return s
		},
		"hours": func() []int {
			h := make([]int, 24)
			for i := range h {
				h[i] = i
			}
			return h
		},
	}
	pages := map[string]*template.Template{}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		page := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if page == "layout" {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n)
		if err != nil {
			return nil, err
		}
		pages[page] = t
	}
	return pages, nil
}

func pluralize(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strings.TrimSpace(strings.Join([]string{itoa(n), word + "s"}, " "))
}

type data map[string]any

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, d data) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "page not found", http.StatusInternalServerError)
		return
	}
	if d == nil {
		d = data{}
	}
	d["V"] = s.assetV
	ps := s.settings(r.Context())
	d["SignupEnabled"] = ps.SignupEnabled
	if ps.BannerMessage != "" {
		d["Banner"], d["BannerLevel"] = ps.BannerMessage, ps.BannerLevel
	}
	s.addNav(r, d)
	if sess := sessionFrom(r); sess != nil {
		d["User"] = sess
		d["CSRF"] = sess.CSRF
	}
	if _, ok := d["Msg"]; !ok {
		d["Msg"] = r.URL.Query().Get("msg")
	}
	if _, ok := d["Err"]; !ok {
		d["Err"] = r.URL.Query().Get("err")
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", d); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// renderPartial renders one shared template (defined in layout.html) without the page
// around it: the HTML fragments htmx swaps in place.
func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, name string, d any) {
	t, ok := s.pages["project"]
	if !ok {
		http.Error(w, "page not found", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, d); err != nil {
		s.log.Error("render partial", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// back redirects with a flash message (msg) or error (errMsg).
func back(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	frag := ""
	if i := strings.Index(path, "#"); i >= 0 {
		path, frag = path[:i], path[i:]
	}
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	http.Redirect(w, r, path+frag, http.StatusSeeOther)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "error", http.StatusNotFound, data{"Title": "Not found", "Text": "This page does not exist, or you do not have access to it."})
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "error", http.StatusForbidden, data{"Title": "Not allowed", "Text": "Your role in this organization does not allow this action."})
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	s.render(w, r, "error", http.StatusInternalServerError, data{"Title": "Something went wrong", "Text": "The error was logged. Please try again."})
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ---------- rate limiting (fixed window, in memory) ----------

type window struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	mu sync.Mutex
	m  map[string]*window
}

func newRateLimiter() *rateLimiter { return &rateLimiter{m: map[string]*window{}} }

func (l *rateLimiter) allow(key string, limit int, per time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.m[key]
	if !ok || now.After(w.reset) {
		w = &window{reset: now.Add(per)}
		l.m[key] = w
	}
	w.count++
	return w.count <= limit
}

func (l *rateLimiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, w := range l.m {
		if now.After(w.reset) {
			delete(l.m, k)
		}
	}
}

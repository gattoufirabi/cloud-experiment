// Package config reads all settings from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL      string // role-specific: platform_api for the API, platform_agent for the agent
	AdminDatabaseURL string // owner role: migrations and admin commands only

	ListenAddr   string // public HTTP (behind Caddy)
	InternalAddr string // internal HTTP: TLS ask endpoint, health; never proxied
	TunnelAddr   string // internal HTTP: the proxy reaches apps on customer servers here; never proxied from outside
	BaseURL      string // public console URL, e.g. https://console.example.com
	ConsoleHost  string
	AppsDomain   string // default app hosts: <service>-<project>-<org>.<AppsDomain>
	CookieSecure bool

	MasterKeyFile      string
	VictoriaLogsURL    string
	VictoriaMetricsURL string

	// agent
	DockerSocket         string
	Runtime              string // runsc (gVisor) in production
	AllowInsecureRuntime bool   // development only: allows runc
	// TenantResolvConf: host path of a resolv.conf bind-mounted (read-only) into
	// sandboxed containers. gVisor cannot reach Docker's embedded DNS (127.0.0.11) on
	// user-defined networks, so production points tenants at real resolvers instead.
	TenantResolvConf string
	CaddyAdminSocket string
	CaddyContainer   string
	AgentContainer   string // set when the agent itself runs in a container (dev)
	TLSMode          string // off | acme
	APIUpstream      string
	TunnelUpstream   string // the API's tunnel listener, as the proxy dials it
	APIInternalURL   string
	ACMECA           string
	MaxCPUMillis     int
	MaxMemoryMB      int

	// registry and scanning
	RegistryHost           string // public host[:port] clients push to, e.g. registry.example.com
	RegistryUpstream       string // registry container address for Caddy
	RegistryInternalSecret string // derives the agent's org-scoped pull passwords
	RegistryEventsSecret   string // bearer secret of the registry push webhook
	RegistryTokenKeyFile   string // ECDSA key signing registry tokens (API only)
	ScannerEnabled         bool
	TrivyImage             string
	TrivyServerURL         string
	TrivyToken             string
	ScannerNetwork         string // network of the short-lived scan containers
	ScannerInsecure        bool   // dev only: registry served over plain HTTP

	// per-client backups (restic)
	BackupsEnabled bool
	ResticImage    string
	BackupVolume   string // local mode: Docker volume holding one repository per org
	BackupRestURL  string // off-site mode: rest:http(s)://user:pass@host:8000 (org ID is appended)
	BackupNetwork  string // network for off-site mode; local mode runs with no network
	BackupKeep     string // restic forget policy

	// alerts
	AlertsEnabled bool

	// This server, as shown to the operator (and, in stage 2, used for placement).
	NodeID   string // default: the Docker host name
	NodeRole string // primary (runs the control plane) | worker
	// NodeFailoverMinutes: a node silent this long is cordoned and its stateless projects
	// are redeployed elsewhere (0 = never)
	NodeFailoverMinutes int
	NodeCluster         string
	NodeAddress         string // private address other nodes reach it on (WireGuard)
	NodeBind            string // address worker apps are published on (default: NodeAddress)

	// Builds from git (sandboxed BuildKit); needs the registry.
	BuildsEnabled    bool
	BuildkitImage    string
	GitImage         string // git + Railpack helper image (deploy/builder)
	RailpackFrontend string // BuildKit frontend for repositories without a Dockerfile
	BuildRuntime     string // container runtime for build containers ("" = Docker default)
	BuildTimeout     time.Duration
	BuildMemoryMB    int
	BuildCPUMillis   int
	BuildSubnet      string
	BuildMaxImageMB  int
	// BuildAllowInsecure (development only): allow http:// and private-address repositories.
	BuildAllowInsecure bool
	// RegistryKeepDays: unused tags younger than this are kept by the daily cleanup.
	RegistryKeepDays int

	// Outgoing email (any SMTP server). Used for signup verification and trial notices.
	SMTPHost, SMTPUser, SMTPPassword, SMTPFrom, SMTPTLS string
	SMTPPort                                            int

	// Self-signup and abuse controls.
	SignupEnabled        bool
	SignupPowBits        int // proof-of-work difficulty (leading zero bits)
	SignupDailyLimit     int // signups accepted per day, all IPs together
	SignupTrialDays      int
	SignupBlockedDomains []string // added to the built-in disposable-email list
	SignupAllowedDomains []string // if set, only these email domains may sign up
	AnomalyCPUPercent    int      // trial containers above this share of their CPU limit...
	AnomalyMinutes       int      // ...for this many minutes in a row get the org suspended
	OperatorNtfyURL      string   // ntfy topic URL for operator notices (optional)

	// Billing: usage is always metered; invoices are issued monthly when enabled.
	BillingEnabled    bool
	BillingCurrency   string        // ISO 4217 code shown on invoices
	BillingTaxPercent float64       // VAT/sales tax applied to every invoice
	BillingSeller     string        // seller block printed on invoices; "|" separates lines
	AlertInterval     time.Duration // health probe / evaluation period
	AlertAllowPrivate bool          // dev only: let notifications reach private addresses

	// Object storage (S3-compatible, served by Garage). On when S3Host is set.
	S3Host        string // public host[:port] of the S3 endpoint, e.g. s3.example.com
	S3Upstream    string // Garage S3 API address for Caddy and the API
	S3WebUpstream string // Garage website endpoint address for Caddy
	S3Region      string // region clients sign requests for
	S3ServiceURL  string // endpoint given to services (default: the public URL)
	// Hosted mailboxes: STALWART_URL (the mail server's management HTTP, e.g.
	// http://stalwart:8080) and STALWART_ADMIN ("admin:password"); MAIL_CERT_DIR (agent):
	// Caddy's certificates folder, from which the mail host's certificate is installed on
	// the mail server.
	StalwartURL, StalwartAdmin, MailCertDir string
	// The webmail of hosted mailboxes: WEBMAIL_HOST (its public name, such as
	// webmail.example.com) and WEBMAIL_UPSTREAM (where the proxy reaches it).
	WebmailHost, WebmailUpstream string
	// ProductName is the name people see (console, webmail, emails): PRODUCT_NAME.
	ProductName string
	// OrphanVolumeDays: how long the volumes of a deleted service or database are kept
	// (ORPHAN_VOLUME_DAYS, default 7) before the agent removes them.
	OrphanVolumeDays int
	GarageAdminURL   string
	GarageAdminToken string

	// OIDCAllowInsecure lets single sign-on use http:// and private addresses (development
	// only, for a local test provider).
	OIDCAllowInsecure bool

	// Outgoing email for tenants: EMAIL_DNS_RESOLVER (host:port) replaces the system
	// resolver for domain verification (tests); EMAIL_SPF_INCLUDE is the domain tenants
	// add to their SPF record (shown in the console when set).
	EmailDNSResolver, EmailSPFInclude string
	// EMAIL_DELIVERY: "direct" (to each recipient's mail servers, port 25), "relay"
	// (through SMTP_HOST) or "off". Default: relay when SMTP_HOST is set, direct otherwise.
	EmailDelivery string
	// MAIL_HOSTNAME: the name this server greets other mail servers with; its reverse
	// DNS (PTR) should point to it. Default: the console host.
	MailHostname string
	// EMAIL_DIRECT_PORT (25) and EMAIL_DIRECT_ALLOW_PRIVATE: test settings only.
	EmailDirectPort         int
	EmailDirectAllowPrivate bool
	// SMTP submission for tenants' applications: SMTP_SUBMISSION_LISTEN (":2587" inside
	// the API container; empty = off), the folder of the proxy's certificates
	// (SMTP_SUBMISSION_CERT_DIR, Caddy's data directory) for STARTTLS on MAIL_HOSTNAME,
	// and SMTP_SUBMISSION_INSECURE (development: sign-in allowed without TLS).
	SMTPSubmissionListen, SMTPSubmissionCertDir string
	SMTPSubmissionInsecure                      bool
	// SMTP_SUBMISSION_UPSTREAM (agent): where the proxy forwards port 465 (implicit TLS,
	// terminated by the proxy with the console's certificate), such as "api:2587".
	SMTPSubmissionUpstream string
	// DBPROXY_IMAGE: the platform's own image, run as the public endpoint of databases
	// opened to the internet (mode "dbproxy").
	DBProxyImage string

	// SignupsPerHour limits self-signups from one address (SIGNUPS_PER_HOUR, default 5).
	SignupsPerHour int

	// DNS hosting: the agent answers on DNSListen (DNS_LISTEN, e.g. ":5353", published as
	// port 53 of the server) for zones delegated to DNSNameservers (DNS_NAMESERVERS,
	// comma-separated host names that resolve to the server). PublicIP (PUBLIC_IP) is
	// the address records point to for an application at a zone's apex.
	DNSListen      string
	DNSNameservers []string
	PublicIP       string

	// MovesSharedLocalRepo: the nodes share the local backup volume (one Docker host,
	// development), so projects with data can move without BACKUP_REST_URL.
	MovesSharedLocalRepo bool

	// Public TCP/UDP ports for non-HTTP services are taken from this range.
	PublicPortMin, PublicPortMax int
}

// MovesEnabled: projects with data can move between nodes (their volumes travel through
// the backup repository, which must be shared: off-site, or one Docker host in dev).
func (c Config) MovesEnabled() bool {
	return c.BackupsEnabled && (c.BackupRestURL != "" || c.MovesSharedLocalRepo)
}

// StorageEnabled reports whether object storage is configured.
// MailHostingEnabled: the platform runs a mail server for organizations' mailboxes.
func (c Config) MailHostingEnabled() bool { return c.StalwartURL != "" && c.StalwartAdmin != "" }

func (c Config) StorageEnabled() bool { return c.S3Host != "" && c.GarageAdminToken != "" }

// S3PublicURL is the S3 endpoint as clients outside the platform use it.
func (c Config) S3PublicURL() string {
	if c.TLSMode == "acme" {
		return "https://" + c.S3Host
	}
	return "http://" + c.S3Host
}

// S3Endpoint is the S3 endpoint given to services (they reach it through the proxy on
// their project network).
func (c Config) S3Endpoint() string {
	if c.S3ServiceURL != "" {
		return c.S3ServiceURL
	}
	return c.S3PublicURL()
}

// WebDomain hosts public website buckets: <bucket>.<WebDomain>.
func (c Config) WebDomain() string { return "web." + c.AppsDomain }

// EmailMode is how tenants' email leaves: "direct", "relay" or "off".
func (c Config) EmailMode() string {
	switch c.EmailDelivery {
	case "direct", "relay", "off":
		return c.EmailDelivery
	}
	if c.SMTPHost != "" {
		return "relay"
	}
	return "direct"
}

// MailHost is the name this server uses towards other mail servers (and for SMTP
// submission): MAIL_HOSTNAME, or the console host without its port.
func (c Config) MailHost() string {
	if c.MailHostname != "" {
		return c.MailHostname
	}
	h := c.ConsoleHost
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

func Load() Config {
	return Config{
		DatabaseURL:          env("DATABASE_URL", ""),
		AdminDatabaseURL:     env("ADMIN_DATABASE_URL", ""),
		ListenAddr:           env("LISTEN_ADDR", ":8080"),
		InternalAddr:         env("INTERNAL_ADDR", ":8081"),
		TunnelAddr:           env("TUNNEL_ADDR", ":8082"),
		BaseURL:              strings.TrimRight(env("BASE_URL", "http://console.localhost:8088"), "/"),
		ConsoleHost:          env("CONSOLE_HOST", "console.localhost"),
		AppsDomain:           env("APPS_DOMAIN", "apps.localhost"),
		CookieSecure:         envBool("COOKIE_SECURE", false),
		MasterKeyFile:        env("MASTER_KEY_FILE", "/run/secrets/master.key"),
		VictoriaLogsURL:      strings.TrimRight(env("VICTORIALOGS_URL", ""), "/"),
		VictoriaMetricsURL:   strings.TrimRight(env("VICTORIAMETRICS_URL", ""), "/"),
		DockerSocket:         env("DOCKER_SOCKET", "/var/run/docker.sock"),
		Runtime:              env("RUNTIME", "runsc"),
		AllowInsecureRuntime: envBool("ALLOW_INSECURE_RUNTIME", false),
		CaddyAdminSocket:     env("CADDY_ADMIN_SOCKET", "/run/caddy/admin.sock"),
		CaddyContainer:       env("CADDY_CONTAINER", "platform-caddy"),
		AgentContainer:       env("AGENT_CONTAINER", ""),
		TLSMode:              env("TLS_MODE", "off"),
		APIUpstream:          env("API_UPSTREAM", "api:8080"),
		TunnelUpstream:       env("TUNNEL_UPSTREAM", "api:8082"),
		APIInternalURL:       strings.TrimRight(env("API_INTERNAL_URL", "http://api:8081"), "/"),
		ACMECA:               env("ACME_CA", "https://acme-v02.api.letsencrypt.org/directory"),
		MaxCPUMillis:         envInt("MAX_CPU_MILLIS", 4000),
		MaxMemoryMB:          envInt("MAX_MEMORY_MB", 8192),

		RegistryHost:           env("REGISTRY_HOST", ""),
		RegistryUpstream:       env("REGISTRY_UPSTREAM", "registry:5000"),
		RegistryInternalSecret: env("REGISTRY_INTERNAL_SECRET", ""),
		RegistryEventsSecret:   env("REGISTRY_EVENTS_SECRET", ""),
		RegistryTokenKeyFile:   env("REGISTRY_TOKEN_KEY_FILE", "/run/secrets/registry-token.key"),
		ScannerEnabled:         envBool("SCANNER_ENABLED", false),
		TrivyImage:             env("TRIVY_IMAGE", "aquasec/trivy:0.58.1"),
		TrivyServerURL:         env("TRIVY_SERVER_URL", "http://trivy:4954"),
		TrivyToken:             env("TRIVY_TOKEN", ""),
		ScannerNetwork:         env("SCANNER_NETWORK", "platform-core"),
		ScannerInsecure:        envBool("SCANNER_INSECURE", false),

		BackupsEnabled: envBool("BACKUPS_ENABLED", false),
		ResticImage:    env("RESTIC_IMAGE", "restic/restic:0.17.3"),
		BackupVolume:   env("BACKUP_VOLUME", "platform-backups"),
		BackupRestURL:  strings.TrimRight(env("BACKUP_REST_URL", ""), "/"),
		BackupNetwork:  env("BACKUP_NETWORK", "platform-core"),
		BackupKeep:     env("BACKUP_KEEP", "--keep-daily 7 --keep-weekly 4 --keep-monthly 6"),

		AlertsEnabled:     envBool("ALERTS_ENABLED", false),
		AlertInterval:     time.Duration(envInt("ALERT_INTERVAL_SECONDS", 30)) * time.Second,
		AlertAllowPrivate: envBool("ALERT_ALLOW_PRIVATE_TARGETS", false),

		S3Host:           env("S3_HOST", ""),
		S3Upstream:       env("S3_UPSTREAM", "garage:3900"),
		S3WebUpstream:    env("S3_WEB_UPSTREAM", "garage:3902"),
		S3Region:         env("S3_REGION", "us-east-1"),
		S3ServiceURL:     strings.TrimRight(env("S3_SERVICE_URL", ""), "/"),
		StalwartURL:      strings.TrimRight(env("STALWART_URL", ""), "/"),
		StalwartAdmin:    env("STALWART_ADMIN", ""),
		MailCertDir:      env("MAIL_CERT_DIR", ""),
		WebmailHost:      env("WEBMAIL_HOST", ""),
		ProductName:      env("PRODUCT_NAME", "Staysa Cloud"),
		OrphanVolumeDays: envInt("ORPHAN_VOLUME_DAYS", 7),
		WebmailUpstream:  env("WEBMAIL_UPSTREAM", "webmail:8090"),
		GarageAdminURL:   strings.TrimRight(env("GARAGE_ADMIN_URL", "http://garage:3903"), "/"),
		GarageAdminToken: env("GARAGE_ADMIN_TOKEN", ""),

		OIDCAllowInsecure:       envBool("OIDC_ALLOW_INSECURE", false),
		EmailDNSResolver:        env("EMAIL_DNS_RESOLVER", ""),
		EmailSPFInclude:         env("EMAIL_SPF_INCLUDE", ""),
		EmailDelivery:           env("EMAIL_DELIVERY", ""),
		MailHostname:            env("MAIL_HOSTNAME", ""),
		EmailDirectPort:         envInt("EMAIL_DIRECT_PORT", 25),
		EmailDirectAllowPrivate: envBool("EMAIL_DIRECT_ALLOW_PRIVATE", false),
		SMTPSubmissionListen:    env("SMTP_SUBMISSION_LISTEN", ""),
		SMTPSubmissionCertDir:   env("SMTP_SUBMISSION_CERT_DIR", ""),
		SMTPSubmissionInsecure:  envBool("SMTP_SUBMISSION_INSECURE", false),
		SMTPSubmissionUpstream:  env("SMTP_SUBMISSION_UPSTREAM", ""),
		DBProxyImage:            env("DBPROXY_IMAGE", "platform:local"),
		SignupsPerHour:          envInt("SIGNUPS_PER_HOUR", 5),
		DNSListen:               env("DNS_LISTEN", ""),
		DNSNameservers:          envList("DNS_NAMESERVERS"),
		PublicIP:                env("PUBLIC_IP", ""),
		MovesSharedLocalRepo:    envBool("MOVES_SHARED_LOCAL_REPO", false),
		PublicPortMin:           envInt("PUBLIC_PORT_MIN", 20000),
		PublicPortMax:           envInt("PUBLIC_PORT_MAX", 20999),

		TenantResolvConf:    env("TENANT_RESOLV_CONF", ""),
		NodeID:              env("NODE_ID", ""),
		NodeRole:            env("NODE_ROLE", "primary"),
		NodeFailoverMinutes: envInt("NODE_FAILOVER_MINUTES", 3),
		NodeCluster:         env("NODE_CLUSTER", "default"),
		NodeAddress:         env("NODE_ADDRESS", ""),
		NodeBind:            env("NODE_BIND", env("NODE_ADDRESS", "")),
		BuildsEnabled:       envBool("BUILDS_ENABLED", false),
		BuildkitImage:       env("BUILDKIT_IMAGE", "moby/buildkit:v0.34.1-rootless"),
		GitImage:            env("GIT_IMAGE", "platform-builder:0.43.0"),
		RailpackFrontend:    env("RAILPACK_FRONTEND", "ghcr.io/railwayapp/railpack-frontend:v0.43.0"),
		BuildRuntime:        env("BUILD_RUNTIME", ""),
		BuildTimeout:        time.Duration(envInt("BUILD_TIMEOUT_MINUTES", 15)) * time.Minute,
		BuildMemoryMB:       envInt("BUILD_MEMORY_MB", 2048),
		BuildCPUMillis:      envInt("BUILD_CPU_MILLIS", 2000),
		BuildSubnet:         env("BUILD_SUBNET", "10.207.0.0/24"),
		BuildMaxImageMB:     envInt("BUILD_MAX_IMAGE_MB", 8192),
		BuildAllowInsecure:  envBool("BUILD_ALLOW_INSECURE_REPOS", false),
		RegistryKeepDays:    envInt("REGISTRY_KEEP_DAYS", 30),

		SMTPHost:     env("SMTP_HOST", ""),
		SMTPPort:     envInt("SMTP_PORT", 587),
		SMTPUser:     env("SMTP_USERNAME", ""),
		SMTPPassword: env("SMTP_PASSWORD", ""),
		SMTPFrom:     env("SMTP_FROM", ""),
		SMTPTLS:      env("SMTP_TLS", "starttls"),

		SignupEnabled:        envBool("SIGNUP_ENABLED", false),
		SignupPowBits:        envInt("SIGNUP_POW_BITS", 16),
		SignupDailyLimit:     envInt("SIGNUP_DAILY_LIMIT", 50),
		SignupTrialDays:      envInt("SIGNUP_TRIAL_DAYS", 14),
		SignupBlockedDomains: envList("SIGNUP_BLOCKED_DOMAINS"),
		SignupAllowedDomains: envList("SIGNUP_ALLOWED_DOMAINS"),
		AnomalyCPUPercent:    envInt("ANOMALY_CPU_PERCENT", 90),
		AnomalyMinutes:       envInt("ANOMALY_MINUTES", 30),
		OperatorNtfyURL:      env("OPERATOR_NTFY_URL", ""),

		BillingEnabled:    envBool("BILLING_ENABLED", false),
		BillingCurrency:   strings.ToUpper(env("BILLING_CURRENCY", "EUR")),
		BillingTaxPercent: envFloat("BILLING_TAX_PERCENT", 0),
		BillingSeller:     strings.ReplaceAll(env("BILLING_SELLER", "Platform operator"), "|", "\n"),
	}
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v, err := strconv.ParseBool(env(k, strconv.FormatBool(def)))
	if err != nil {
		return def
	}
	return v
}

func envInt(k string, def int) int {
	v, err := strconv.Atoi(env(k, strconv.Itoa(def)))
	if err != nil {
		return def
	}
	return v
}

func envFloat(k string, def float64) float64 {
	v, err := strconv.ParseFloat(env(k, ""), 64)
	if err != nil || v < 0 || v > 100 {
		return def
	}
	return v
}

func envList(k string) []string {
	var out []string
	for _, f := range strings.Split(env(k, ""), ",") {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			out = append(out, f)
		}
	}
	return out
}

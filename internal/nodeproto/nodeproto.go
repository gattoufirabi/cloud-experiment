// Package nodeproto is the protocol between the API and the node agent on a
// customer-owned server. The node agent is not trusted: it only ever talks to the API
// over HTTPS with its node token, and the API decides what it may see and change.
//
//	POST /node/v1/join                       join token -> node ID and node token
//	POST /node/v1/heartbeat                  capacity and health, every 30 s
//	GET  /node/v1/desired                    the deployments this server must run
//	POST /node/v1/deployments/{id}/status    deploying | live | failed (+ message)
//	POST /node/v1/logs                       application log lines
//	POST /node/v1/stats                      measured CPU and memory per deployment
//	GET  /node/v1/tunnel                     HTTP Upgrade: one idle tunnel connection
//
// Tunnel: the node keeps a few upgraded connections open. To forward a visitor's
// request, the API writes one line "OPEN <deployment ID>\n"; the node connects to one
// of that deployment's instances and answers "OK\n" (or "ERR <reason>\n"), then both
// sides copy bytes until either closes. Each connection carries one stream.
package nodeproto

import "time"

const (
	TokenPrefix    = "pln_" // node token
	JoinPrefix     = "plj_" // one-time join token
	TunnelProtocol = "platform-tunnel/1"
	// Labels on the containers the node agent creates (distinct from the platform's own
	// "platform.*" labels, so a platform agent never mistakes them for its own).
	LabelManaged    = "staysa.node.managed"
	LabelDeployment = "staysa.node.deployment"
	LabelService    = "staysa.node.service"
	LabelProject    = "staysa.node.project"
	LabelInstance   = "staysa.node.instance"
)

type JoinRequest struct {
	Token string `json:"token"`
	Facts
}

type JoinResponse struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

// Facts describe the server; sent at join and with every heartbeat.
type Facts struct {
	Hostname      string  `json:"hostname"`
	OS            string  `json:"os"`
	Arch          string  `json:"arch"`
	CPUs          int     `json:"cpus"`
	MemMB         int64   `json:"mem_mb"`
	MemAvailMB    int64   `json:"mem_avail_mb"`
	DiskTotalMB   int64   `json:"disk_total_mb"`
	DiskFreeMB    int64   `json:"disk_free_mb"`
	Load1         float64 `json:"load1"`
	Containers    int     `json:"containers"`
	DockerVersion string  `json:"docker_version"`
	AgentVersion  string  `json:"agent_version"`
}

type Desired struct {
	NodeID      string       `json:"node_id"`
	Registry    RegistryAuth `json:"registry"`
	Deployments []Deployment `json:"deployments"`
}

// RegistryAuth pulls the organization's own images (its namespace only).
type RegistryAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type Deployment struct {
	ID         string   `json:"id"`
	ServiceID  string   `json:"service_id"`
	ProjectID  string   `json:"project_id"`
	Service    string   `json:"service"` // slug (preview: "<parent>-<preview name>")
	Project    string   `json:"project"`
	Revision   int      `json:"revision"`
	Status     string   `json:"status"` // pending | deploying | live
	Image      string   `json:"image"`
	Port       int      `json:"port"`
	HealthPath string   `json:"health_path"`
	CPUMillis  int      `json:"cpu_millis"`
	MemoryMB   int      `json:"memory_mb"`
	Instances  int      `json:"instances"`
	Worker     bool     `json:"worker"`
	VolumePath string   `json:"volume_path,omitempty"`
	Env        []string `json:"env"` // complete environment, secrets included
}

type StatusReport struct {
	Status    string   `json:"status"` // deploying | live | failed ("" = message only)
	Message   string   `json:"message"`
	Instances []string `json:"instances,omitempty"` // container names, on live
}

type LogBatch struct {
	Lines []LogLine `json:"lines"`
}

type LogLine struct {
	Deployment string    `json:"deployment"`
	Time       time.Time `json:"time"`
	Stream     string    `json:"stream"` // stdout | stderr
	Msg        string    `json:"msg"`
}

type StatsReport struct {
	Deployments []DeploymentStats `json:"deployments"`
}

type DeploymentStats struct {
	Deployment string  `json:"deployment"`
	CPUPct     float64 `json:"cpu_pct"`
	MemMB      int     `json:"mem_mb"`
}

// ---------- builds ----------

// BuildJob is a build a server claimed (POST /node/v1/builds/claim; 204 when none).
type BuildJob struct {
	ID         string `json:"id"`
	Number     int    `json:"number"`
	ServiceID  string `json:"service_id"`
	Repo       string `json:"repo,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Dir        string `json:"dir,omitempty"`
	Token      string `json:"token,omitempty"` // read token of a private repository
	Zip        bool   `json:"zip,omitempty"`   // the source is an uploaded archive: GET /node/v1/builds/{id}/source
	SourceName string `json:"source_name,omitempty"`
	SourceSHA  string `json:"source_sha256,omitempty"`
	// ImageRepo is where the image goes (<registry>/<org>/<service>); the server picks
	// the tag (git-<commit12> or zip-<sha12>) and pushes with PushToken (user "build").
	ImageRepo        string `json:"image_repo"`
	PushToken        string `json:"push_token"`
	BuilderImage     string `json:"builder_image"` // git + Railpack helper, built on the server when missing
	BuildkitImage    string `json:"buildkit_image"`
	RailpackFrontend string `json:"railpack_frontend"`
	TimeoutSec       int    `json:"timeout_sec"`
	MaxImageMB       int    `json:"max_image_mb"`
}

// BuildLogReport carries the whole log so far (POST /node/v1/builds/{id}/log). The API
// answers 409 when the build is no longer running on this server: the server stops.
type BuildLogReport struct {
	Log string `json:"log"`
}

// BuildResult ends a build (POST /node/v1/builds/{id}/finish).
type BuildResult struct {
	Status string `json:"status"` // done or failed
	Commit string `json:"commit,omitempty"`
	Image  string `json:"image,omitempty"`
	Error  string `json:"error,omitempty"`
	Log    string `json:"log"`
}

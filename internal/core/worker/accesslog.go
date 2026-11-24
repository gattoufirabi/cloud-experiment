package worker

// Per-request analytics: the proxy writes one JSON line per request to an
// application's host (logger "tenant"); the primary follows the proxy's output, maps
// each host to its organization and service, and stores a compact record in
// VictoriaLogs under its own stream fields (an_org, an_svc, an_kind=access), apart from
// the applications' logs. No IP address is stored: visitors are counted with a hash of
// address and browser salted with a secret that changes every day.

type HostRef struct{ OrgID, ProjectID, ServiceID string }

type CaddyAccess struct {
	Ts      float64 `json:"ts"`
	Request struct {
		ClientIP string              `json:"client_ip"`
		RemoteIP string              `json:"remote_ip"`
		Method   string              `json:"method"`
		Host     string              `json:"host"`
		URI      string              `json:"uri"`
		Proto    string              `json:"proto"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
	BytesRead int64   `json:"bytes_read"`
	Duration  float64 `json:"duration"`
	Size      int64   `json:"size"`
	Status    int     `json:"status"`
}

// Package model holds types and validation shared by the API and the agent.
package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	slugRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,19}$`)
	envKeyRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	SecretNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	imageRe      = regexp.MustCompile(`^[a-z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
	pathRe       = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,199}$`)
	hostnameRe   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

func ValidSlug(s string) bool     { return slugRe.MatchString(s) }
func ValidHostname(s string) bool { return len(s) <= 253 && hostnameRe.MatchString(s) }

// Spec is an immutable deployment description. The agent builds the container from it
// and nothing else: there is deliberately no field for mounts, privileges or networks.
type Spec struct {
	Image      string            `json:"image"`
	Port       int               `json:"port"`
	CPUMillis  int               `json:"cpu_millis"`
	MemoryMB   int               `json:"memory_mb"`
	HealthPath string            `json:"health_path"`
	VolumePath string            `json:"volume_path,omitempty"`
	Env        map[string]string `json:"env"`
	// Worker: no public address (queue consumer, bot). Not routed by the proxy, no
	// port or HTTP health check: healthy while the container keeps running.
	Worker bool `json:"worker,omitempty"`
	// Instances: identical containers behind the proxy (1 when unset).
	Instances int `json:"instances,omitempty"`
}

// Count is the number of instances to run (at least 1).
func (s Spec) Count() int {
	if s.Instances < 1 {
		return 1
	}
	return s.Instances
}

func (s Spec) Validate() error {
	switch {
	case s.Image == "":
		return fmt.Errorf("an image is required (for example nginx:1.27-alpine)")
	case !imageRe.MatchString(s.Image) || strings.Contains(s.Image, "..") || strings.Contains(s.Image, "//"):
		return fmt.Errorf("invalid image reference")
	case !s.Worker && (s.Port < 1 || s.Port > 65535):
		return fmt.Errorf("port must be between 1 and 65535")
	case s.CPUMillis < 50 || s.CPUMillis > 8000:
		return fmt.Errorf("CPU must be between 50 and 8000 millicores")
	case s.MemoryMB < 32 || s.MemoryMB > 16384:
		return fmt.Errorf("memory must be between 32 and 16384 MB")
	case !s.Worker && !pathRe.MatchString(s.HealthPath):
		return fmt.Errorf("health check path must start with / and contain no spaces")
	}
	if s.Instances < 0 || s.Instances > 10 {
		return fmt.Errorf("instances must be between 1 and 10")
	}
	if s.Count() > 1 && s.VolumePath != "" {
		return fmt.Errorf("a service with a persistent folder runs a single instance (the folder cannot be shared)")
	}
	if s.VolumePath != "" {
		if !pathRe.MatchString(s.VolumePath) || s.VolumePath == "/" || strings.Contains(s.VolumePath, "..") {
			return fmt.Errorf("invalid volume path")
		}
		for _, p := range []string{"/proc", "/sys", "/dev", "/etc", "/bin", "/sbin", "/usr", "/lib"} {
			if s.VolumePath == p || strings.HasPrefix(s.VolumePath, p+"/") {
				return fmt.Errorf("volume path %s is not allowed", p)
			}
		}
	}
	if len(s.Env) > 100 {
		return fmt.Errorf("at most 100 environment variables")
	}
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid environment variable name %q", k)
		}
		if strings.HasPrefix(strings.ToUpper(k), "PLATFORM_") {
			return fmt.Errorf("environment variable names starting with PLATFORM_ are reserved")
		}
		if len(v) > 4096 || strings.ContainsRune(v, 0) {
			return fmt.Errorf("value of %s is too long or contains invalid characters", k)
		}
	}
	return nil
}

// ParseEnv reads KEY=VALUE lines; blank lines and # comments are ignored.
func ParseEnv(text string) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", i+1)
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

func FormatEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + env[k] + "\n")
	}
	return b.String()
}

// DefaultHost is the platform-provided address of a service. Slugs are at most
// 20 characters each, so the first label stays under the 63-character DNS limit.
func DefaultHost(service, project, org, appsDomain string) string {
	return service + "-" + project + "-" + org + "." + appsDomain
}

var roleRank = map[string]int{"viewer": 1, "developer": 2, "admin": 3, "owner": 4}

func ValidRole(r string) bool { return roleRank[r] > 0 }

func RoleAtLeast(role, min string) bool { return roleRank[role] >= roleRank[min] && roleRank[min] > 0 }

// Compact turns a UUID into 32 hex characters for resource names.
func Compact(id string) string { return strings.ReplaceAll(id, "-", "") }

// NetworkName is the per-project Docker network. "pt-" + 12 hex = 15 characters,
// the Linux interface-name limit, so the bridge can carry the same name.
func NetworkName(projectID string) string { return "pt-" + Compact(projectID)[:12] }

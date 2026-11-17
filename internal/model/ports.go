package model

import (
	"fmt"
	"strconv"
	"strings"
)

// PublicPort exposes a container port (TCP or UDP) on a public port of the platform,
// for services that do not speak HTTP: game servers, MQTT, custom protocols.
type PublicPort struct {
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
	PublicPort    int    `json:"public_port,omitempty"` // assigned by the platform
}

const MaxPublicPorts = 5

// Key is "25565/tcp".
func (p PublicPort) Key() string { return strconv.Itoa(p.ContainerPort) + "/" + p.Protocol }

// ParsePorts reads one port per line (or comma separated): "25565", "25565/tcp", "27015/udp".
func ParsePorts(text string) ([]PublicPort, error) {
	var out []PublicPort
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' || r == '\r' }) {
		port, proto, _ := strings.Cut(strings.ToLower(strings.TrimSpace(f)), "/")
		if proto == "" {
			proto = "tcp"
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || (proto != "tcp" && proto != "udp") {
			return nil, fmt.Errorf("%q: give a port and protocol, such as 25565/tcp or 27015/udp", f)
		}
		p := PublicPort{ContainerPort: n, Protocol: proto}
		if !seen[p.Key()] {
			seen[p.Key()] = true
			out = append(out, p)
		}
	}
	if len(out) > MaxPublicPorts {
		return nil, fmt.Errorf("at most %d public ports per service", MaxPublicPorts)
	}
	return out, nil
}

// PortsText is the ports as the settings form shows them.
func PortsText(ports []PublicPort) string {
	var keys []string
	for _, p := range ports {
		keys = append(keys, p.Key())
	}
	return strings.Join(keys, "\n")
}

func validatePorts(s Spec) error {
	if len(s.Ports) > MaxPublicPorts {
		return fmt.Errorf("at most %d public ports per service", MaxPublicPorts)
	}
	seen := map[string]bool{}
	for _, p := range s.Ports {
		if p.ContainerPort < 1 || p.ContainerPort > 65535 || (p.Protocol != "tcp" && p.Protocol != "udp") || p.PublicPort < 0 || p.PublicPort > 65535 {
			return fmt.Errorf("invalid public port %s", p.Key())
		}
		if seen[p.Key()] {
			return fmt.Errorf("port %s is listed twice", p.Key())
		}
		seen[p.Key()] = true
	}
	if len(s.Ports) > 0 && s.Count() > 1 {
		return fmt.Errorf("a service with public TCP/UDP ports runs a single instance (a public port goes to one container)")
	}
	return nil
}

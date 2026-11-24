package web

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Build servers (migration 069): an organization's own servers can run its builds, like
// self-hosted CI agents. A service set to build on "servers" queues builds that only the
// organization's servers with builds turned on, and every label the service asks for,
// can claim. The server gets what the build needs and nothing more: the source (and its
// read token), and a push token for that build's repository that works while it runs.
// It reports the log as it goes and the result at the end; the API then records the
// image and queues the deployment, as the platform's builder does.

var labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,29}$`)

// parseLabels reads "gpu, arm64 large" into a sorted, de-duplicated list.
func ParseLabels(raw string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if !labelRe.MatchString(l) {
			return nil, errors.New("Labels: lowercase letters, digits, dots, dashes and underscores, up to 30 characters each.")
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) > 10 {
		return nil, errors.New("Labels: at most 10.")
	}
	sort.Strings(out)
	return out, nil
}

// serverBuilds is what a server does with builds.
type ServerBuilds struct {
	Enabled   bool     `json:"enabled"`
	Labels    []string `json:"labels"`
	MaxBuilds int      `json:"max_builds"`
	Clean     bool     `json:"clean"`
}

// saveServerBuilds stores a server's build settings; the server's name, or no rows.
func (s *Server) SaveServerBuilds(r *http.Request, tx pgx.Tx, o *OrgInfo, id string, b ServerBuilds) (string, error) {
	var name string
	if err := tx.QueryRow(r.Context(), `UPDATE nodes SET builds = $2, labels = $3, max_builds = $4, clean_builds = $5 WHERE id = $1 AND role = 'customer' RETURNING name`,
		id, b.Enabled, b.Labels, b.MaxBuilds, b.Clean).Scan(&name); err != nil {
		return "", err
	}
	s.Audit(r, tx, o.ID, "server.builds", "server:"+name, map[string]any{"id": id, "builds": b.Enabled, "labels": b.Labels, "max_builds": b.MaxBuilds, "clean": b.Clean})
	s.BuildWake.Signal()
	return name, nil
}

const MaxBuildsErr = "Builds at once: 1 to 8."

// ---------- node API ----------

// buildWake wakes the build servers' held claim requests when a build may be waiting.
type BuildWake struct {
	mu sync.Mutex
	Ch chan struct{}
}

func (b *BuildWake) Wait() <-chan struct{} {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Ch
}

// signal wakes the waiting claims shortly, once the caller's transaction committed.
func (b *BuildWake) Signal() {
	if b == nil {
		return
	}
	time.AfterFunc(300*time.Millisecond, func() {
		b.mu.Lock()
		close(b.Ch)
		b.Ch = make(chan struct{})
		b.mu.Unlock()
	})
}

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/web"
	"platform/internal/lib/db"
)

// Activity: everything of an organization that is in progress (builds waiting or
// running, deployments, databases starting, backups and restores),
// shown live in the console header so that nothing seems to hang without a word.

type activityItem struct {
	Kind   string    `json:"kind"`   // build, deploy, database, backup
	Label  string    `json:"label"`  // what, e.g. "store/grafana"
	State  string    `json:"state"`  // short state, e.g. "queued", "building", "deploying"
	Detail string    `json:"detail"` // a sentence for the user
	Since  time.Time `json:"since"`
	Link   string    `json:"link"`
	Key    string    `json:"key"` // stable identifier, to notice when it finishes
}

func durationText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// lastStep is the last "==> step" line of a build log, for a running build.
func lastStep(log string) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "==> ") {
			return strings.TrimPrefix(l, "==> ")
		}
	}
	return ""
}

func (s *API) loadActivity(ctx context.Context, tx pgx.Tx, o *web.OrgInfo) ([]activityItem, error) {
	items := []activityItem{}
	now := time.Now()
	base := o.Path()

	// Builds: queued (with their place in the queue) or running (with their current step).
	rows, err := tx.Query(ctx, `
		SELECT b.id, b.number, b.status, b.trigger, b.created_at, coalesce(b.started_at, b.created_at), b.log, p.slug, s.slug
		FROM builds b JOIN services s ON s.id = b.service_id JOIN projects p ON p.id = s.project_id
		WHERE b.status IN ('queued', 'running') ORDER BY b.created_at`)
	if err != nil {
		return nil, err
	}
	type build struct {
		ID, Status, Trigger, Log, Project, Service string
		Number                                     int
		Created, Started                           time.Time
	}
	var builds []build
	for rows.Next() {
		var b build
		if err := rows.Scan(&b.ID, &b.Number, &b.Status, &b.Trigger, &b.Created, &b.Started, &b.Log, &b.Project, &b.Service); err != nil {
			rows.Close()
			return nil, err
		}
		builds = append(builds, b)
	}
	rows.Close()
	for _, b := range builds {
		it := activityItem{Kind: "build", Label: b.Project + "/" + b.Service, Link: fmt.Sprintf("%s/p/%s/s/%s/builds/%d", base, b.Project, b.Service, b.Number),
			Key: "build:" + b.ID, Since: b.Created}
		if b.Status == "queued" {
			var ahead int
			_ = tx.QueryRow(ctx, `SELECT build_queue_ahead($1)`, b.ID).Scan(&ahead)
			it.State = "queued"
			switch ahead {
			case 0:
				it.Detail = fmt.Sprintf("Build #%d starts in a moment.", b.Number)
			case 1:
				it.Detail = fmt.Sprintf("Build #%d waits for 1 build to finish (builds run one at a time).", b.Number)
			default:
				it.Detail = fmt.Sprintf("Build #%d waits for %d builds to finish (builds run one at a time).", b.Number, ahead)
			}
		} else {
			it.State, it.Since = "building", b.Started
			it.Detail = fmt.Sprintf("Build #%d running for %s", b.Number, durationText(now.Sub(b.Started)))
			if step := lastStep(b.Log); step != "" {
				it.Detail += ": " + step
			}
			it.Detail += "."
		}
		items = append(items, it)
	}

	// Deployments waiting or switching over.
	rows, err = tx.Query(ctx, `
		SELECT d.id, d.revision, d.status, d.message, d.created_at, p.slug, s.slug, coalesce(s.preview_name, '')
		FROM deployments d JOIN services s ON s.id = d.service_id JOIN projects p ON p.id = s.project_id
		WHERE d.status IN ('pending', 'deploying') ORDER BY d.created_at`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, status, msg, project, service, preview string
		var rev int
		var created time.Time
		if err := rows.Scan(&id, &rev, &status, &msg, &created, &project, &service, &preview); err != nil {
			rows.Close()
			return nil, err
		}
		label := project + "/" + service
		if preview != "" {
			label += " (" + preview + ")"
		}
		detail := fmt.Sprintf("Revision %d waits to be deployed.", rev)
		if status == "deploying" {
			detail = fmt.Sprintf("Revision %d is starting (%s)", rev, durationText(now.Sub(created)))
			if msg != "" {
				detail += ": " + msg
			}
			detail += "."
		}
		items = append(items, activityItem{Kind: "deploy", Label: label, State: map[string]string{"pending": "waiting", "deploying": "deploying"}[status],
			Detail: detail, Since: created, Link: fmt.Sprintf("%s/p/%s/s/%s", base, project, service), Key: "deploy:" + id})
	}
	rows.Close()

	// Databases starting.
	rows, err = tx.Query(ctx, `
		SELECT d.id, d.slug, d.engine, d.message, d.created_at, p.slug FROM databases d JOIN projects p ON p.id = d.project_id
		WHERE d.status = 'pending' ORDER BY d.created_at`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, slug, engine, msg, project string
		var created time.Time
		if err := rows.Scan(&id, &slug, &engine, &msg, &created, &project); err != nil {
			rows.Close()
			return nil, err
		}
		detail := fmt.Sprintf("%s is starting (%s); the first start can take a few minutes.", web.EngineName(engine), durationText(now.Sub(created)))
		if msg != "" {
			detail = web.EngineName(engine) + ": " + msg
		}
		items = append(items, activityItem{Kind: "database", Label: project + "/" + slug, State: "starting", Detail: detail, Since: created,
			Link: fmt.Sprintf("%s/p/%s/d/%s", base, project, slug), Key: "db:" + id})
	}
	rows.Close()

	// Backups and restores.
	rows, err = tx.Query(ctx, `
		SELECT j.id, j.kind, j.status, j.created_at, coalesce(p.slug, ''), coalesce(d.slug, s.slug, ''), d.id IS NOT NULL
		FROM backup_jobs j LEFT JOIN databases d ON d.id = j.database_id LEFT JOIN services s ON s.id = j.service_id
		LEFT JOIN projects p ON p.id = coalesce(d.project_id, s.project_id)
		WHERE j.status IN ('queued', 'running') ORDER BY j.created_at`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, kind, status, project, name string
		var isDB bool
		var created time.Time
		if err := rows.Scan(&id, &kind, &status, &created, &project, &name, &isDB); err != nil {
			rows.Close()
			return nil, err
		}
		what := "Backup"
		if kind == "restore" {
			what = "Restore"
		}
		link := fmt.Sprintf("%s/p/%s/s/%s#backups", base, project, name)
		if isDB {
			link = fmt.Sprintf("%s/p/%s/d/%s#backups", base, project, name)
		}
		items = append(items, activityItem{Kind: "backup", Label: project + "/" + name, State: map[string]string{"queued": "waiting", "running": strings.ToLower(what) + "ing"}[status],
			Detail: fmt.Sprintf("%s %s (%s).", what, map[string]string{"queued": "waiting to start", "running": "in progress"}[status], durationText(now.Sub(created))),
			Since:  created, Link: link, Key: "backup:" + id})
	}
	rows.Close()

	return items, nil
}

func (s *API) activityJSON(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Org(w, r, "viewer")
	if !ok {
		return
	}
	var items []activityItem
	if err := db.WithOrg(r.Context(), s.Pool, o.ID, func(tx pgx.Tx) error {
		var err error
		items, err = s.loadActivity(r.Context(), tx, o)
		return err
	}); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	if o.Scoped { // a member limited to some projects sees only their activity
		kept := items[:0]
		for _, it := range items {
			if proj, _, _ := strings.Cut(it.Label, "/"); o.Sees(proj) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	w.Header().Set("Cache-Control", "no-store")
	web.WriteJSON(w, map[string]any{"items": items})
}

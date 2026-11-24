package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const MaxActiveBuildsPerOrg = 5

type SourceInfo struct {
	Repo, Branch, Dir             string
	HasToken, HasHook, PRPreviews bool
	BuildOn                       string   // platform or servers
	BuildLabels                   []string // labels a build server must have
}

func (si SourceInfo) LabelText() string { return strings.Join(si.BuildLabels, ", ") }

func LoadSource(r *http.Request, tx pgx.Tx, serviceID string) (SourceInfo, error) {
	var si SourceInfo
	err := tx.QueryRow(r.Context(), `
		SELECT source_repo, source_branch, source_dir, source_token_enc IS NOT NULL, build_hook_hash IS NOT NULL, pr_previews, build_on, build_labels
		FROM services WHERE id = $1`, serviceID).Scan(&si.Repo, &si.Branch, &si.Dir, &si.HasToken, &si.HasHook, &si.PRPreviews, &si.BuildOn, &si.BuildLabels)
	return si, err
}

type BuildView struct {
	Number                  int
	Status, Trigger, Commit string
	Image, Error, Log, By   string
	Deploy                  bool
	Branch                  string
	PR                      *int
	Created                 time.Time
	Started, Finished       *time.Time
	Runner, Server          string // where it runs: platform, or servers (and which one)
}

func (b BuildView) Duration() string {
	if b.Started == nil {
		return ""
	}
	end := time.Now()
	if b.Finished != nil {
		end = *b.Finished
	}
	return end.Sub(*b.Started).Round(time.Second).String()
}

func (b BuildView) ShortCommit() string {
	if len(b.Commit) > 12 {
		return b.Commit[:12]
	}
	return b.Commit
}

const BuildCols = `b.number, b.status, b.trigger, b.commit, b.image, b.error, b.log, coalesce(u.email, ''), b.deploy, b.branch, b.pr_number, b.created_at, b.started_at, b.finished_at, b.runner, coalesce((SELECT n.name FROM nodes n WHERE n.id = b.node_id), '')`

func LoadBuilds(r *http.Request, tx pgx.Tx, serviceID string, limit int) ([]BuildView, error) {
	rows, err := tx.Query(r.Context(), `SELECT `+BuildCols+` FROM builds b LEFT JOIN users u ON u.id = b.created_by
		WHERE b.service_id = $1 ORDER BY b.number DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[BuildView])
}

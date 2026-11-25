package web

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Object storage: S3-compatible buckets and access keys of an organization, served by
// Garage. The API creates buckets and keys in Garage right away (a key's secret is only
// known at creation); removals go through storage_trash and are done by the agent,
// which also empties buckets, meters usage and applies suspensions.

const MaxUploadBytes = 25 << 20 // console uploads; larger files go through the S3 API

var (
	BucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	DocumentRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

type BucketInfo struct {
	ID, Name, FullName, GarageID, IndexDocument, ErrorDocument string
	QuotaMB                                                    int
	Website                                                    bool
	ConsoleKeyID                                               string
	ConsoleSecretEnc                                           []byte
	Objects, Bytes                                             int64
	UsageAt                                                    *time.Time
	CreatedAt                                                  time.Time
	CORSOrigins                                                []string // web pages allowed to use the bucket from the browser
}

// UsedMB and UsedPercent describe the bucket's fill level for templates.
func (b *BucketInfo) UsedMB() float64 { return float64(b.Bytes) / (1 << 20) }
func (b *BucketInfo) UsedPercent() int {
	if b.QuotaMB == 0 {
		return 0
	}
	return int(float64(b.Bytes) * 100 / float64(int64(b.QuotaMB)<<20))
}

const BucketCols = `id, name, full_name, garage_id, index_document, error_document, quota_mb, website, console_key_id, console_secret_enc,
	objects, bytes, usage_at, created_at, cors_origins`

func ScanBucket(row pgx.Row) (*BucketInfo, error) {
	var b BucketInfo
	err := row.Scan(&b.ID, &b.Name, &b.FullName, &b.GarageID, &b.IndexDocument, &b.ErrorDocument, &b.QuotaMB, &b.Website,
		&b.ConsoleKeyID, &b.ConsoleSecretEnc, &b.Objects, &b.Bytes, &b.UsageAt, &b.CreatedAt, &b.CORSOrigins)
	return &b, err
}

func LoadBuckets(ctx context.Context, tx pgx.Tx) ([]*BucketInfo, error) {
	rows, err := tx.Query(ctx, `SELECT `+BucketCols+` FROM buckets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BucketInfo
	for rows.Next() {
		b, err := ScanBucket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func NullableUser(r *http.Request) any {
	if sess := SessionFrom(r); sess != nil && sess.UserID != "" {
		return sess.UserID
	}
	return nil
}

func Ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func HostOnly(hostport string) string {
	if h, _, ok := strings.Cut(hostport, ":"); ok {
		return h
	}
	return hostport
}

// humanBytes formats a size as "12.3 MB" (binary units).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

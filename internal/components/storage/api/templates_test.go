package api

import (
	"io"
	"strings"
	"testing"
	"time"

	"platform/internal/components/storage/garage"
	"platform/internal/core/web"
)

// The storage and bucket pages render with buckets, keys, a new key's secret, a folder
// listing and website settings.
func TestStoragePagesRender(t *testing.T) {
	pages, err := web.ParseTemplates(Templates)
	if err != nil {
		t.Fatal(err)
	}
	org := &web.OrgInfo{Slug: "acme", Name: "Acme", Role: "admin"}
	now := time.Now()
	b := &web.BucketInfo{ID: "b1", Name: "uploads", FullName: "acme-uploads", QuotaMB: 100, Bytes: 25 << 20, Objects: 3, Website: true, IndexDocument: "index.html", CreatedAt: now}
	var out strings.Builder
	if err := pages["storage"].ExecuteTemplate(&out, "content", web.Data{
		"Org": org, "Buckets": []*web.BucketInfo{b}, "Quota": storageQuota{MaxBuckets: 10, MaxStorageMB: 10240, Buckets: 1, AllocatedMB: 100},
		"Keys":   []storageKeyView{{ID: "k1", Name: "ci", AccessKeyID: "GKabc", Created: now, Grants: []grantView{{Bucket: "uploads", Write: true}}}},
		"NewKey": garage.Key{AccessKeyID: "GKnew", SecretAccessKey: "s3cr3t", Name: "laptop"},
		"S3URL":  "https://s3.example.test", "Region": "us-east-1", "CanEdit": true, "CanDelete": true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"acme-uploads", "s3cr3t", "GKabc", "read-write", "public website", `name="perm_uploads"`, "25%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("storage page lacks %q", want)
		}
	}
	out.Reset()
	if err := pages["bucket"].ExecuteTemplate(&out, "content", web.Data{
		"Org": org, "Bucket": b, "Prefix": "docs/", "Parent": "", "Path": "/o/acme/storage/b/uploads",
		"Objects":  []objectRow{{Key: "docs/sub/", Name: "sub/", Folder: true}, {Key: "docs/a b.txt", Name: "a b.txt", Size: 2048, Modified: now}},
		"Services": []string{"shop/web"}, "S3URL": "https://s3.example.test", "Region": "us-east-1", "WebsiteURL": "https://acme-uploads.web.apps.test/",
		"CanEdit": true, "CanDelete": true, "MaxUploadMB": 25,
	}); err != nil {
		t.Fatal(err)
	}
	out.WriteString(settingsPane(t, pages["bucket"], web.Data{"Org": org, "Bucket": b, "Path": "/o/acme/storage/b/uploads", "CanEdit": true, "CanDelete": true}))
	for _, want := range []string{"key=docs%2fa%20b.txt", "prefix=docs%2fsub%2f", "2.0 KB", "https://acme-uploads.web.apps.test/", "shop/web", "Delete bucket"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("bucket page lacks %q", want)
		}
	}
}

// settingsPane renders a page's settings view (its settings tab, on a page of its own).
func settingsPane(t *testing.T, page interface {
	ExecuteTemplate(io.Writer, string, any) error
}, d web.Data) string {
	t.Helper()
	sd := web.Data{}
	for k, v := range d {
		sd[k] = v
	}
	sd["View"] = "settings"
	var b strings.Builder
	if err := page.ExecuteTemplate(&b, "content", sd); err != nil {
		t.Fatal("settings view:", err)
	}
	return b.String()
}

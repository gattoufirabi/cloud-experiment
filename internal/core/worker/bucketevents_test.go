package worker

import (
	"testing"
)

func TestS3Event(t *testing.T) {
	line := func(method, uri string, status int) []byte {
		return []byte(`{"ts":1795000000,"logger":"http.log.access.s3","request":{"method":"` + method + `","host":"s3.example.test","uri":"` + uri + `","headers":{}},"bytes_read":42,"status":` + itoa(status) + `}`)
	}
	cases := []struct {
		method, uri string
		status      int
		ok          bool
		bucket, key string
		name        string
	}{
		{"PUT", "/acme-uploads/photos/cat%201.jpg", 200, true, "acme-uploads", "photos/cat 1.jpg", "ObjectCreated:Put"},
		{"PUT", "/acme-uploads/big.bin?partNumber=2&uploadId=x", 200, false, "", "", ""},
		{"POST", "/acme-uploads/big.bin?uploadId=x", 200, true, "acme-uploads", "big.bin", "ObjectCreated:CompleteMultipartUpload"},
		{"POST", "/acme-uploads/big.bin?uploads", 200, false, "", "", ""},
		{"DELETE", "/acme-uploads/old.txt", 204, true, "acme-uploads", "old.txt", "ObjectRemoved:Delete"},
		{"PUT", "/acme-uploads/denied.txt", 403, false, "", "", ""},
		{"GET", "/acme-uploads/read.txt", 200, false, "", "", ""},
		{"PUT", "/acme-uploads", 200, false, "", "", ""},
	}
	for _, c := range cases {
		b, k, n, size, _, ok := s3Event(line(c.method, c.uri, c.status))
		if ok != c.ok || b != c.bucket || k != c.key || n != c.name || (ok && size != 42) {
			t.Errorf("%s %s %d: got %v %q %q %q %d", c.method, c.uri, c.status, ok, b, k, n, size)
		}
	}
}

func itoa(n int) string {
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

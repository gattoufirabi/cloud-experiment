package s3

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Examples from the AWS documentation "Signature Calculations for the Authorization
// Header" and "Authenticating Requests: Using Query Parameters" (Amazon S3).
var awsCreds = Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", Region: "us-east-1"}
var awsTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func TestSignGetObjectAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	Sign(req, awsCreds, emptySHA256, awsTime)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestPresignAWSExample(t *testing.T) {
	u, err := Presign(awsCreds, http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", 24*time.Hour, awsTime)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404") {
		t.Errorf("signature differs: %s", u)
	}
}

func TestURIEncode(t *testing.T) {
	if got := uriEncode("photos/2024 summer/ç+a.jpg", true); got != "photos/2024%20summer/%C3%A7%2Ba.jpg" {
		t.Errorf("got %s", got)
	}
	if got := ObjectURL("http://s3.test/", "b", "a b"); got != "http://s3.test/b/a%20b" {
		t.Errorf("got %s", got)
	}
}

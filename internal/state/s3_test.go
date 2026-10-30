package state

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/s3fake"
)

func TestS3Source(t *testing.T) {
	for _, tc := range []struct {
		src, bucket, key, redacted string
		ok, bad                    bool
	}{
		{src: "s3://b/env/prod.tfstate", bucket: "b", key: "env/prod.tfstate", redacted: "s3://b/env/prod.tfstate", ok: true},
		{src: "S3://b/a%zz?x#y", bucket: "b", key: "a%zz?x#y", redacted: "s3://b/a%zz?x#y", ok: true},
		{src: "s3://b", bucket: "b", redacted: "s3://b/", ok: true},
		{src: "s3://user:secret@b/k", redacted: "<invalid URL>", ok: true, bad: true},
		{src: "s3:///k", redacted: "<invalid URL>", ok: true, bad: true},
		{src: "https://b/k", redacted: "https://b/k"},
		{src: "state.tfstate", redacted: "state.tfstate"},
	} {
		bucket, key, ok, err := S3Source(tc.src)
		if ok != tc.ok || (err != nil) != tc.bad || bucket != tc.bucket || key != tc.key {
			t.Errorf("S3Source(%q) = %q, %q, %v, %v", tc.src, bucket, key, ok, err)
		}
		if got := Redact(tc.src); got != tc.redacted || strings.Contains(got, "secret") {
			t.Errorf("Redact(%q) = %q, want %q", tc.src, got, tc.redacted)
		}
	}
	for src, want := range map[string]bool{"s3://b": true, "s3://b/": true, "s3://b/env/": true, "s3://b/env": false, "s3://a@b/": false, "/tmp/x/": false} {
		if got := IsS3Prefix(src); got != want {
			t.Errorf("IsS3Prefix(%q) = %v", src, got)
		}
	}
}

func TestS3ListAndGet(t *testing.T) {
	fake := &s3fake.Server{Bucket: "states", PageSize: 2, Objects: map[string]string{
		"env/a.tfstate":          `{"version":4,"lineage":"a","resources":[]}`,
		"env/b.tfstate":          `{"version":4,"lineage":"b","resources":[]}`,
		"env/b.tfstate.backup":   `{}`,
		"env/notes.txt":          `x`,
		"env:/dev/env/a.tfstate": `{"version":4,"lineage":"c","resources":[]}`,
		"other/c.tfstate":        `{"version":4,"lineage":"d","resources":[]}`,
		"env/a b+\x01.tfstate":   `{"version":4,"lineage":"e","resources":[]}`,
		"env/\x01.txt":           `x`,
	}}
	fake.Start(t)
	ctx := context.Background()
	c := &S3{}
	if got, _, err := c.List(ctx, "s3://states/env", 0); err == nil {
		t.Errorf("a key without '/' listed: %v", got)
	}
	got, other, err := c.List(ctx, "s3://states/env/", 0)
	if err != nil || other != 3 || !slices.Equal(got, []string{"s3://states/env/a b+\x01.tfstate", "s3://states/env/a.tfstate", "s3://states/env/b.tfstate"}) {
		t.Errorf("List(env/) = %v, %d, %v", got, other, err)
	}
	// The whole bucket, across pages, workspaces included.
	got, _, err = c.List(ctx, "s3://states", 0)
	if err != nil || len(got) != 5 || !slices.Contains(got, "s3://states/env:/dev/env/a.tfstate") {
		t.Errorf("List(bucket) = %v, %v", got, err)
	}
	if _, _, err := c.List(ctx, "s3://states/", 3); err == nil || !strings.Contains(err.Error(), "more than 3 states") {
		t.Errorf("limit: %v", err)
	}
	// The decoded key reads the object back.
	if data, err := c.Get(ctx, "s3://states/env/a b+\x01.tfstate", DefaultMaxSize); err != nil || !strings.Contains(string(data), `"e"`) {
		t.Errorf("Get of an encoded key = %s, %v", data, err)
	}
	data, err := c.Get(ctx, "s3://states/env:/dev/env/a.tfstate", DefaultMaxSize)
	if err != nil || !strings.Contains(string(data), `"c"`) {
		t.Errorf("Get = %s, %v", data, err)
	}
	var size *TooLargeError
	if _, err := c.Get(ctx, "s3://states/env/a.tfstate", 10); !errors.As(err, &size) {
		t.Errorf("size limit: %v", err)
	}
	if _, err := c.Get(ctx, "s3://states/env/", DefaultMaxSize); err == nil || !strings.Contains(err.Error(), "is a prefix") {
		t.Errorf("Get of a prefix: %v", err)
	}
	if data, err := Fetch(ctx, "s3://states/other/c.tfstate", false, DefaultMaxSize); err != nil || !strings.Contains(string(data), `"d"`) {
		t.Errorf("Fetch = %s, %v", data, err)
	}
	if _, err := Fetch(ctx, "s3://states/missing.tfstate", false, DefaultMaxSize); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("missing object: %v", err)
	}
	if n := fake.Unsigned.Load(); n != 0 {
		t.Errorf("%d requests without a signature", n)
	}
}

func TestS3PlainHTTPEndpoint(t *testing.T) {
	(&s3fake.Server{Bucket: "states"}).Start(t)
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://127.0.0.1:1")
	_, err := (&S3{}).Get(context.Background(), "s3://states/a.tfstate", DefaultMaxSize)
	if err == nil || !strings.Contains(err.Error(), "-allow-http") {
		t.Errorf("plain http endpoint: %v", err)
	}
	// With -allow-http the request is made (and fails to connect here).
	_, err = (&S3{AllowHTTP: true}).Get(context.Background(), "s3://states/a.tfstate", DefaultMaxSize)
	if err == nil || strings.Contains(err.Error(), "-allow-http") {
		t.Errorf("plain http endpoint with -allow-http: %v", err)
	}
}

// A redirect is never followed: the SDK alone would follow a 307 from the
// https endpoint to plain http.
func TestS3NoRedirect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("redirect followed to %s", r.URL)
		fmt.Fprint(w, `{"version":4}`)
	}))
	defer plain.Close()
	fake := &s3fake.Server{Bucket: "states", Redirect: map[string]string{"a.tfstate": plain.URL + "/states/a.tfstate"}}
	fake.Start(t)
	if _, err := (&S3{AllowHTTP: true}).Get(context.Background(), "s3://states/a.tfstate", DefaultMaxSize); err == nil || !strings.Contains(err.Error(), "307") {
		t.Errorf("redirect: %v", err)
	}
}

// Web identity credentials work with no region set anywhere: the STS
// client is built while the configuration loads.
func TestS3WebIdentityWithoutRegion(t *testing.T) {
	fake := &s3fake.Server{Bucket: "states", Objects: map[string]string{"a.tfstate": `{"version":4}`}}
	fake.Start(t)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_REGION": "", "AWS_DEFAULT_REGION": "", "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "",
		"AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/read", "AWS_WEB_IDENTITY_TOKEN_FILE": token,
		"AWS_ENDPOINT_URL_STS": fake.URL,
	} {
		t.Setenv(k, v)
	}
	if _, err := (&S3{}).Get(context.Background(), "s3://states/a.tfstate", DefaultMaxSize); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fake.STSCalls.Load() == nil || fake.Tokens.Load() != "ROLETOKEN" {
		t.Errorf("STS asked: %v, session token %v", fake.STSCalls.Load(), fake.Tokens.Load())
	}
}

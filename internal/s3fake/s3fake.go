// Package s3fake is an S3 endpoint for tests: path-style ListObjectsV2 and
// GetObject over TLS, reached through AWS_ENDPOINT_URL_S3.
package s3fake

import (
	"encoding/pem"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// Server serves Objects (key to body) of one Bucket. A key in Denied
// answers 403. Lists return PageSize keys per page.
type Server struct {
	Bucket  string
	Objects map[string]string
	Denied  map[string]bool
	// Redirect answers a key with a 307 to this URL.
	Redirect map[string]string
	PageSize int
	// Unsigned counts requests without a SigV4 Authorization header.
	Unsigned atomic.Int32
	URL      string
}

// Start serves s and points the AWS SDK of this test at it, with fixed
// credentials and no shared configuration.
func (s *Server) Start(t *testing.T) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"AWS_ENDPOINT_URL_S3":         srv.URL,
		"AWS_CA_BUNDLE":               ca,
		"AWS_ACCESS_KEY_ID":           "AKIDTEST",
		"AWS_SECRET_ACCESS_KEY":       "secret",
		"AWS_REGION":                  "eu-north-1",
		"AWS_CONFIG_FILE":             filepath.Join(dir, "none"),
		"AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "none"),
		"AWS_EC2_METADATA_DISABLED":   "true",
		"AWS_PROFILE":                 "",
		"AWS_SESSION_TOKEN":           "",
		"AWS_ENDPOINT_URL":            "",
	} {
		t.Setenv(k, v)
	}
}

type content struct {
	Key  string
	Size int
}

type listResult struct {
	XMLName               xml.Name  `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name                  string    `xml:"Name"`
	Prefix                string    `xml:"Prefix"`
	KeyCount              int       `xml:"KeyCount"`
	IsTruncated           bool      `xml:"IsTruncated"`
	NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
	Contents              []content `xml:"Contents"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		s.Unsigned.Add(1)
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != s.Bucket || r.Method != http.MethodGet {
		http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
		return
	}
	if key == "" && r.URL.Query().Get("list-type") == "2" {
		prefix := r.URL.Query().Get("prefix")
		var keys []string
		for k := range s.Objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		if after := r.URL.Query().Get("continuation-token"); after != "" {
			i, _ := slices.BinarySearch(keys, after)
			keys = keys[i:]
		}
		res := listResult{Name: s.Bucket, Prefix: prefix}
		if s.PageSize > 0 && len(keys) > s.PageSize {
			res.IsTruncated, res.NextContinuationToken = true, keys[s.PageSize]
			keys = keys[:s.PageSize]
		}
		for _, k := range keys {
			res.Contents = append(res.Contents, content{k, len(s.Objects[k])})
		}
		res.KeyCount = len(keys)
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(res)
		return
	}
	if to := s.Redirect[key]; to != "" {
		http.Redirect(w, r, to, http.StatusTemporaryRedirect)
		return
	}
	if s.Denied[key] {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>"))
		return
	}
	body, ok := s.Objects[key]
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>"))
		return
	}
	_, _ = w.Write([]byte(body))
}

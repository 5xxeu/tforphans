package state

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// bucketPattern accepts current bucket names and the legacy ones (capitals,
// underscores, up to 255 characters). None of '@', ':', '/', '?' or '#' is
// allowed, so an s3:// source never carries user info.
var bucketPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,255}$`)

// S3Source splits "s3://bucket/key" into the bucket and the key. The key is
// taken as written (no percent-decoding, '?' and '#' are part of it), like
// the key argument of Terraform's s3 backend. ok is false for any other
// scheme; err is set for an s3:// source that is not valid.
func S3Source(src string) (bucket, key string, ok bool, err error) {
	if !strings.EqualFold(urlScheme(src), "s3") {
		return "", "", false, nil
	}
	rest := src[len("s3://"):]
	bucket, key, _ = strings.Cut(rest, "/")
	if !bucketPattern.MatchString(bucket) {
		return "", "", true, errors.New("invalid S3 state source: want s3://BUCKET/KEY or s3://BUCKET/PREFIX/")
	}
	return bucket, key, true, nil
}

// IsS3Prefix tells whether src names every state under an S3 prefix: the
// whole bucket or a key ending in '/'.
func IsS3Prefix(src string) bool {
	_, key, ok, err := S3Source(src)
	return ok && err == nil && (key == "" || strings.HasSuffix(key, "/"))
}

// S3 reads states from S3 with the AWS SDK's default credentials (the
// environment, shared config and SSO profiles, web identity, ECS and EC2
// roles). Each bucket is read in its own region, found with an anonymous
// HeadBucket unless AWS_ENDPOINT_URL or AWS_ENDPOINT_URL_S3 names another
// service (MinIO, Ceph, R2), which is then addressed path-style.
type S3 struct {
	// AllowHTTP permits a plain http endpoint (-allow-http).
	AllowHTTP bool

	mu      sync.Mutex
	cfg     *aws.Config
	clients map[string]*s3.Client
}

// Get reads one object, "s3://bucket/key", in its latest version.
func (c *S3) Get(ctx context.Context, src string, maxSize int64) ([]byte, error) {
	bucket, key, ok, err := S3Source(src)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, fmt.Errorf("not an S3 source: %s", Redact(src))
	case key == "" || strings.HasSuffix(key, "/"):
		return nil, fmt.Errorf("%s is a prefix, not a state object", Redact(src))
	}
	client, err := c.client(ctx, bucket)
	if err != nil {
		return nil, err
	}
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return ReadLimited(out.Body, maxSize)
}

// List returns the state objects under an "s3://bucket/prefix/" source, as
// s3:// sources, and how many other objects it passed over. A state object
// is a key ending in ".tfstate", which includes the workspaces Terraform
// keeps under "env:/NAME/". More than maxObjects states is an error before
// any is read (0: no limit).
func (c *S3) List(ctx context.Context, src string, maxObjects int) (states []string, other int, err error) {
	bucket, prefix, ok, err := S3Source(src)
	switch {
	case err != nil:
		return nil, 0, err
	case !ok || !IsS3Prefix(src):
		return nil, 0, fmt.Errorf("not an S3 prefix: %s", Redact(src))
	}
	client, err := c.client(ctx, bucket)
	if err != nil {
		return nil, 0, err
	}
	// Keys come back URL-encoded: a key may hold characters XML cannot
	// carry, and one such object would fail the whole page.
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix), EncodingType: types.EncodingTypeUrl,
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, 0, err
		}
		for _, obj := range page.Contents {
			// S3 encodes like a form: a space is "+".
			key, err := url.QueryUnescape(aws.ToString(obj.Key))
			if err != nil {
				return nil, 0, fmt.Errorf("listing %s: undecodable key: %w", Redact(src), err)
			}
			if !strings.HasSuffix(key, ".tfstate") {
				other++
				continue
			}
			if maxObjects > 0 && len(states) == maxObjects {
				return nil, 0, fmt.Errorf("%s holds more than %d states; narrow the prefix or raise -max-roots", Redact(src), maxObjects)
			}
			states = append(states, "s3://"+bucket+"/"+key)
		}
	}
	return states, other, nil
}

// client returns the client for bucket, loading the shared configuration
// and finding the bucket's region the first time.
func (c *S3) client(ctx context.Context, bucket string) (*s3.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client := c.clients[bucket]; client != nil {
		return client, nil
	}
	if c.cfg == nil {
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithHTTPClient(awshttp.NewBuildableClient()),
			// Only a starting point, used when nothing sets a region: the
			// bucket's own region is looked up. It is given here, not after
			// loading, because loading already builds the STS client of web
			// identity and assumed-role credentials.
			config.WithDefaultRegion("us-east-1"))
		if err != nil {
			return nil, fmt.Errorf("AWS configuration: %w", err)
		}
		// The SDK follows 307 and 308 to any scheme, which would take the
		// state (and the signed headers) from https to plain http. Nothing
		// here needs a redirect: the bucket's region is looked up first. The
		// transport keeps what the configuration set, AWS_CA_BUNDLE included.
		built, ok := cfg.HTTPClient.(*awshttp.BuildableClient)
		if !ok {
			return nil, fmt.Errorf("AWS configuration: unexpected HTTP client %T", cfg.HTTPClient)
		}
		cfg.HTTPClient = &http.Client{
			Transport: built.GetTransport(),
			Timeout:   2 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		c.cfg = &cfg
	}
	custom := ""
	client := s3.NewFromConfig(*c.cfg, func(o *s3.Options) {
		if o.BaseEndpoint != nil {
			custom = *o.BaseEndpoint
			o.UsePathStyle = true
		}
		// The SDK logs to stderr when an object has no checksum to check.
		o.DisableLogOutputChecksumValidationSkipped = true
	})
	if custom != "" {
		u, err := url.Parse(custom)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("invalid AWS endpoint URL: want https://HOST")
		}
		if u.Scheme == "http" && !c.AllowHTTP {
			return nil, errors.New("refusing to read state from an S3 endpoint over plain http, where it can be read and altered in transit; use https, or -allow-http")
		}
	} else {
		region, err := manager.GetBucketRegion(ctx, client, bucket)
		if err != nil {
			return nil, fmt.Errorf("region of S3 bucket %s: %w", bucket, err)
		}
		if region != c.cfg.Region {
			client = s3.NewFromConfig(*c.cfg, func(o *s3.Options) {
				o.Region = region
				o.DisableLogOutputChecksumValidationSkipped = true
			})
		}
	}
	if c.clients == nil {
		c.clients = map[string]*s3.Client{}
	}
	c.clients[bucket] = client
	return client, nil
}

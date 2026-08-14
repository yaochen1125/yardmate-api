package imageingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// ObjectStore is the narrow R2 interface the ingestor depends on. *R2Client
// satisfies it in production; tests inject a mock so they never touch the
// network (SPEC §10 — ingestor_test mocks r2).
type ObjectStore interface {
	// Exists reports whether the object already lives in the bucket
	// (idempotency double-check; R2 is the source of truth, SPEC §9 #8).
	Exists(ctx context.Context, key string) (bool, error)
	// Put uploads body verbatim under key with the given Content-Type and
	// Cache-Control (no re-encode — SPEC §2.6 / §7 D2).
	Put(ctx context.Context, key string, body []byte, contentType, cacheControl string) error
	// Delete removes the object at key (idempotent; a missing object is NOT an
	// error). Used to compensate a ledger write that failed AFTER a successful Put
	// so the "R2 object ⇔ ledger license row" invariant holds (SPEC §2.6 / finding
	// #3).
	Delete(ctx context.Context, key string) error
}

// R2Client wraps an aws-sdk-go-v2 S3 client pointed at Cloudflare R2.
//
// Built with explicit static credentials (NEVER config.LoadDefaultConfig
// without creds — it would pick up ambient AWS env / ~/.aws creds on the box,
// SPEC §9 #12), Region "auto", and BaseEndpoint set to the R2 endpoint. R2
// ignores S3 ACLs; public read is configured at the bucket / custom-domain
// level (already serving the curated AAA images), so no ACL param is sent.
type R2Client struct {
	client *s3.Client
	bucket string
}

// R2Config holds the R2 connection parameters resolved from the secrets vault.
type R2Config struct {
	AccountID       string // R2_ACCOUNT_ID (used to derive the endpoint when Endpoint is empty)
	AccessKeyID     string // R2_ACCESS_KEY_ID
	SecretAccessKey string // R2_SECRET_ACCESS_KEY
	Bucket          string // R2_BUCKET (yardmate-static)
	Endpoint        string // R2_ENDPOINT (optional; derived from AccountID when empty)
}

// NewR2Client builds an R2-pointed S3 client. Returns an error when required
// config is missing so main can WARN + disable the service (graceful, mirrors
// buildEnrichmentService).
func NewR2Client(cfg R2Config) (*R2Client, error) {
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("imageingest/r2: missing access key id / secret")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("imageingest/r2: missing bucket")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, errors.New("imageingest/r2: need R2_ENDPOINT or R2_ACCOUNT_ID")
		}
		endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	}

	creds := credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")
	client := s3.New(s3.Options{
		Region:       "auto",
		Credentials:  creds,
		BaseEndpoint: aws.String(endpoint),
	})
	return &R2Client{client: client, bucket: cfg.Bucket}, nil
}

// Exists issues a HeadObject; a 404 (NotFound / NoSuchKey) returns (false, nil),
// any other error propagates so the caller can distinguish "not present" from
// "R2 unreachable".
func (c *R2Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return false, nil
		}
	}
	return false, fmt.Errorf("imageingest/r2: head %q: %w", key, err)
}

// Put uploads the bytes verbatim. ContentType is the real sniffed MIME (SPEC
// §2.6); CacheControl is long+immutable for heroes, short for credits.json.
func (c *R2Client) Put(ctx context.Context, key string, body []byte, contentType, cacheControl string) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(c.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String(cacheControl),
	})
	if err != nil {
		return fmt.Errorf("imageingest/r2: put %q: %w", key, err)
	}
	return nil
}

// Get downloads the object at key and reports whether it exists: a missing
// object (404 NotFound / NoSuchKey) returns (nil, false, nil) — same 404
// mapping as Exists — so callers (rarity version read-modify-write) can
// distinguish "first publish" from "R2 unreachable".
func (c *R2Client) Get(ctx context.Context, key string) ([]byte, bool, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "NotFound", "NoSuchKey":
				return nil, false, nil
			}
		}
		return nil, false, fmt.Errorf("imageingest/r2: get %q: %w", key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, fmt.Errorf("imageingest/r2: get %q: read body: %w", key, err)
	}
	return body, true, nil
}

// Delete removes the object at key. A missing object (404 NotFound / NoSuchKey) is
// NOT an error — the delete is idempotent (S3/R2 DeleteObject also returns success
// for an absent key) — because its caller uses it to restore the R2⇔ledger
// invariant after a failed ledger write (SPEC §2.6 / finding #3), where the object
// being already gone is the desired end state.
func (c *R2Client) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return nil
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return nil
		}
	}
	return fmt.Errorf("imageingest/r2: delete %q: %w", key, err)
}

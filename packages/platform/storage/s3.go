package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is one connection to an S3-compatible service: MinIO, Cloudflare R2,
// Backblaze B2, or AWS S3 itself. The API is the same, so one driver serves all
// of them and the choice is configuration.
type S3Config struct {
	// Endpoint is the host, without a scheme, e.g. "minio:9000".
	Endpoint string
	// Region is required by AWS and ignored by most alternatives.
	Region string
	// Bucket is created when it is missing.
	Bucket    string
	AccessKey string
	SecretKey string
	// UseSSL switches to https.
	UseSSL bool
	// PathStyle uses bucket-in-path addressing, which MinIO and R2 want.
	PathStyle bool
}

// S3Store stores objects in an S3-compatible bucket.
type S3Store struct {
	client *minio.Client
	bucket string
}

// NewS3 connects and makes sure the bucket exists.
//
// Creating the bucket here rather than in a migration keeps the deployment to
// one step: the API either has somewhere to write or it fails at startup with a
// clear message, instead of failing on the first upload.
func NewS3(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("%w: endpoint and bucket are required", ErrNotConfigured)
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: build s3 client: %w", err)
	}
	if cfg.PathStyle {
		client.SetAppInfo("bolu", "path-style")
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage: check bucket %s: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("storage: create bucket %s: %w", cfg.Bucket, err)
		}
	}

	return &S3Store{client: client, bucket: cfg.Bucket}, nil
}

// Driver implements Store.
func (s *S3Store) Driver() string { return "s3" }

// List implements Store, paging through the bucket under a prefix.
func (s *S3Store) List(ctx context.Context, prefix string) ([]string, error) {
	keys := []string{}
	// A bucket listing is paged, so the walk continues while the provider says there is more: a
	// single call would silently stop at a thousand objects, which for "delete everything this
	// tenant owns" is the difference between erasing a workspace and erasing part of it.
	continuation := ""

	for {
		options := minio.ListObjectsOptions{Prefix: prefix, Recursive: true}
		if continuation != "" {
			options.StartAfter = continuation
		}

		page := 0
		for object := range s.client.ListObjects(ctx, s.bucket, options) {
			if object.Err != nil {
				return nil, fmt.Errorf("storage: list %s: %w", prefix, object.Err)
			}
			keys = append(keys, object.Key)
			continuation = object.Key
			page++
		}

		if page == 0 {
			return keys, nil
		}
	}
}

// Put implements Store.
func (s *S3Store) Put(ctx context.Context, key, contentType string, content []byte) (Object, error) {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	info, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(content), int64(len(content)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return Object{}, fmt.Errorf("storage: put %s: %w", key, err)
	}

	return Object{
		Key:            key,
		ContentType:    contentType,
		ByteSize:       info.Size,
		ChecksumSHA256: Digest(content),
	}, nil
}

// Get implements Store.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("storage: get %s: %w", key, err)
	}
	defer func() { _ = object.Close() }()

	// The first read surfaces a missing object: minio's client is lazy, so the
	// error arrives here rather than from GetObject.
	content, err := io.ReadAll(object)
	if err != nil {
		if isMissing(err) {
			return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return nil, fmt.Errorf("storage: read %s: %w", key, err)
	}
	return content, nil
}

// Delete implements Store.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("storage: delete %s: %w", key, err)
	}
	return nil
}

// PresignGet returns a URL that serves one object for a limited time.
//
// It is what lets a browser fetch a large attachment directly from storage
// instead of streaming it through the API, which is the difference between one
// process serving a file and one process proxying it.
func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	url, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("storage: presign %s: %w", key, err)
	}
	return url.String(), nil
}

func isMissing(err error) bool {
	var response minio.ErrorResponse
	if ok := asErrorResponse(err, &response); ok {
		return response.StatusCode == http.StatusNotFound || response.Code == "NoSuchKey"
	}
	return false
}

func asErrorResponse(err error, target *minio.ErrorResponse) bool {
	// errors.As rather than a type assertion: the driver wraps what it returns,
	// and an unwrapped assertion would silently miss every wrapped failure.
	return errors.As(err, target)
}

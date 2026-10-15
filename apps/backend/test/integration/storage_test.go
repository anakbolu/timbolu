package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/robzlabz/employeebot/packages/platform/storage"
)

// s3Endpoint starts an S3-compatible server and returns its host:port.
//
// The driver is the one every deployment uses, so it is exercised against a real
// server rather than a fake: the interesting failures are in the protocol — a
// missing object, a bucket that does not exist yet, a presigned URL — and a
// hand-written fake would test the fake instead.
//
// The image is a mock rather than MinIO itself: MinIO's Docker Hub repository was
// withdrawn, so pinning it would make this test fail for everyone.
func s3Endpoint(t *testing.T) string {
	t.Helper()

	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "adobe/s3mock:latest",
			ExposedPorts: []string{"9090/tcp"},
			Env: map[string]string{
				// The mock accepts any credentials, but the client still signs
				// every request, which is what the driver exercises.
				"COM_ADOBE_TESTING_S3MOCK_STORE_VALID_KMS_IDS": "false",
			},
			WaitingFor: wait.ForHTTP("/favicon.ico").WithPort("9090/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	require.NoError(t, err, "start the s3 mock")

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		require.NoError(t, container.Terminate(cleanupCtx))
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "9090")
	require.NoError(t, err)

	return host + ":" + port.Port()
}

func newS3Store(t *testing.T) *storage.S3Store {
	t.Helper()

	endpoint := s3Endpoint(t)

	store, err := storage.NewS3(context.Background(), storage.S3Config{
		Endpoint:  endpoint,
		Bucket:    "bolu",
		AccessKey: "bolu",
		SecretKey: "bolu-secret",
		PathStyle: true,
	})
	require.NoError(t, err)
	require.Equal(t, "s3", store.Driver())

	return store
}

// TestS3RoundTrip is the deployment path of every stored object: an attachment
// and a sandboxed document both go through here.
func TestS3RoundTrip(t *testing.T) {
	store := newS3Store(t)
	ctx := context.Background()

	key := "content/workspaces/a/objects/b/index.html"
	content := []byte("<html><script>document.cookie</script></html>")

	object, err := store.Put(ctx, key, "text/html; charset=utf-8", content)
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), object.ByteSize)
	require.Equal(t, storage.Digest(content), object.ChecksumSHA256)

	read, err := store.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, string(content), string(read))

	// The bucket was created by NewS3, which is what keeps the deployment to one
	// step: the API either has somewhere to write or it fails at startup.
	_, err = store.Get(ctx, "content/missing")
	require.ErrorIs(t, err, storage.ErrObjectNotFound)

	require.NoError(t, store.Delete(ctx, key))
	_, err = store.Get(ctx, key)
	require.ErrorIs(t, err, storage.ErrObjectNotFound)

	// Deleting something absent converges.
	require.NoError(t, store.Delete(ctx, key))
}

// TestS3PresignedURL is what lets a browser fetch a large attachment directly
// rather than streaming it through the API.
func TestS3PresignedURL(t *testing.T) {
	store := newS3Store(t)
	ctx := context.Background()

	key := "attachments/workspaces/a/objects/b/daftar harga.pdf"
	_, err := store.Put(ctx, key, "application/pdf", []byte("harga"))
	require.NoError(t, err)

	url, err := store.PresignGet(ctx, key, time.Minute)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(url, "http"), "a presigned URL is absolute: %s", url)
	require.Contains(t, url, "bolu", "the bucket is in the URL")
}

// TestS3RejectsAnUnreachableEndpoint keeps a misconfigured deployment a startup
// failure rather than a first-upload failure.
func TestS3RejectsAnUnreachableEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := storage.NewS3(ctx, storage.S3Config{
		Endpoint:  "127.0.0.1:1",
		Bucket:    "bolu",
		AccessKey: "a",
		SecretKey: "b",
	})
	require.Error(t, err)
}

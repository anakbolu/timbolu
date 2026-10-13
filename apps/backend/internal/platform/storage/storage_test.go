package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *LocalStore {
	t.Helper()

	store, err := NewLocal(t.TempDir())
	require.NoError(t, err)
	return store
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	object, err := store.Put(ctx, "content/workspaces/a/objects/b/index.html", "text/html", []byte("<h1>halo</h1>"))
	require.NoError(t, err)
	require.Equal(t, int64(13), object.ByteSize)
	require.Len(t, object.ChecksumSHA256, 64)
	require.Equal(t, Digest([]byte("<h1>halo</h1>")), object.ChecksumSHA256)

	content, err := store.Get(ctx, object.Key)
	require.NoError(t, err)
	require.Equal(t, "<h1>halo</h1>", string(content))

	require.NoError(t, store.Delete(ctx, object.Key))
	_, err = store.Get(ctx, object.Key)
	require.ErrorIs(t, err, ErrObjectNotFound)

	// Deleting something absent converges instead of failing, so a retry after a
	// partial failure is safe.
	require.NoError(t, store.Delete(ctx, object.Key))
}

func TestPutOverwritesTheSameKey(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	_, err := store.Put(ctx, "content/a", "text/plain", []byte("first"))
	require.NoError(t, err)
	_, err = store.Put(ctx, "content/a", "text/plain", []byte("second"))
	require.NoError(t, err)

	content, err := store.Get(ctx, "content/a")
	require.NoError(t, err)
	require.Equal(t, "second", string(content))
}

// TestKeysCannotEscapeTheRoot is the path-traversal guard: a key that would land
// outside the storage root is refused rather than resolved.
func TestKeysCannotEscapeTheRoot(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(root)
	require.NoError(t, err)

	outside := filepath.Join(filepath.Dir(root), "escaped.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, key := range []string{
		"../escaped.txt",
		"../../etc/passwd",
		"content/../../escaped.txt",
		"",
		"   ",
	} {
		t.Run(key, func(t *testing.T) {
			_, err := store.Get(context.Background(), key)
			require.Error(t, err)

			_, err = store.Put(context.Background(), key, "text/plain", []byte("x"))
			require.Error(t, err)
		})
	}

	// The refused write did not land anywhere.
	content, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "secret", string(content))
}

func TestNewLocalRequiresARoot(t *testing.T) {
	_, err := NewLocal("")
	require.ErrorIs(t, err, ErrNotConfigured)

	_, err = NewLocal("   ")
	require.ErrorIs(t, err, ErrNotConfigured)
}

func TestNewLocalCreatesTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "storage")

	store, err := NewLocal(root)
	require.NoError(t, err)
	require.Equal(t, "local", store.Driver())

	info, err := os.Stat(root)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

// TestReadAllBoundsAnUpload keeps a client from streaming an unbounded body into
// memory.
func TestReadAllBoundsAnUpload(t *testing.T) {
	content, err := ReadAll(strings.NewReader("12345"), 5)
	require.NoError(t, err)
	require.Equal(t, "12345", string(content))

	_, err = ReadAll(strings.NewReader("123456"), 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")

	_, err = ReadAll(strings.NewReader(""), 0)
	require.Error(t, err)
}

func TestS3RequiresAnEndpointAndBucket(t *testing.T) {
	_, err := NewS3(context.Background(), S3Config{Bucket: "bolu"})
	require.ErrorIs(t, err, ErrNotConfigured)

	_, err = NewS3(context.Background(), S3Config{Endpoint: "minio:9000"})
	require.ErrorIs(t, err, ErrNotConfigured)
}

// TestDeletingTheLastObjectLeavesNoDirectoryBehind: the local driver is the one that keeps a
// directory tree, so it is the one that can leave a trace the deployments which matter cannot. A
// directory named after a workspace that was just erased is exactly that trace, and it comes back
// with a sibling object kept intact.
func TestDeletingTheLastObjectLeavesNoDirectoryBehind(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	require.NoError(t, err)

	ctx := t.Context()

	// Two objects: one whose parents should go, and one whose parents must stay.
	lonely := "attachments/workspaces/aaaa/objects/one/nota.pdf"
	kept := "attachments/workspaces/aaaa/objects/two/nota.pdf"
	neighbour := "attachments/workspaces/aaaa/objects/two/lain.pdf"

	for _, key := range []string{lonely, kept, neighbour} {
		_, err := store.Put(ctx, key, "application/pdf", []byte("isi"))
		require.NoError(t, err)
	}

	require.NoError(t, store.Delete(ctx, lonely))

	// The key is gone, listing finds nothing under it, and its directories are gone too.
	_, err = store.Get(ctx, lonely)
	require.ErrorIs(t, err, ErrObjectNotFound)

	keys, err := store.List(ctx, "attachments/workspaces/aaaa/objects/one")
	require.NoError(t, err)
	require.Empty(t, keys)

	entries, err := os.ReadDir(filepath.Join(store.root, "attachments", "workspaces", "aaaa", "objects"))
	require.NoError(t, err)

	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{"two"}, names, "the emptied directory is gone and the used one stays")

	// And removing the last object of the other branch prunes up to the root's own child.
	require.NoError(t, store.Delete(ctx, kept))
	require.NoError(t, store.Delete(ctx, neighbour))

	entries, err = os.ReadDir(store.root)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing named after the workspace is left")
}

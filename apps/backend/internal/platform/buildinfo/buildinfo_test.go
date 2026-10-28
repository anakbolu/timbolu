package buildinfo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSnapshotReportsUnknownForUnsetValues keeps a build that set nothing
// readable: an empty field would look like a broken probe instead of a build
// that carries no identity.
func TestSnapshotReportsUnknownForUnsetValues(t *testing.T) {
	restore := save()
	t.Cleanup(restore)

	Commit, BuiltAt, Fingerprint = "", "", ""

	require.Equal(t, Info{Commit: "unknown", BuiltAt: "unknown", Build: "unknown"}, Snapshot())
}

// TestSnapshotReportsTheInjectedValues is what makes the endpoint usable as
// deploy evidence: whatever the build injected must reach the caller unchanged.
func TestSnapshotReportsTheInjectedValues(t *testing.T) {
	restore := save()
	t.Cleanup(restore)

	Commit, BuiltAt, Fingerprint = "0d5c17d", "2026-10-29T10:00:00Z", "8c258b2de0c32616"

	require.Equal(t, Info{
		Commit:  "0d5c17d",
		BuiltAt: "2026-10-29T10:00:00Z",
		Build:   "8c258b2de0c32616",
	}, Snapshot())
}

// save captures the package state so a test that mutates it cannot leak into
// another one.
func save() func() {
	commit, builtAt, fingerprint := Commit, BuiltAt, Fingerprint

	return func() {
		Commit, BuiltAt, Fingerprint = commit, builtAt, fingerprint
	}
}

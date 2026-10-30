package kms

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/packages/platform/crypto"
)

// key renders a 32-byte key the way a deployment writes one in the environment.
func key(seed byte) string {
	raw := make([]byte, crypto.KeySize)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func newManager(t *testing.T, activeID, activeKey string, retired map[string]string) *Local {
	t.Helper()

	manager, err := NewLocal(activeID, activeKey, retired)
	require.NoError(t, err)
	return manager
}

// TestADataKeyRoundTrips is the envelope's basic contract.
func TestADataKeyRoundTrips(t *testing.T) {
	manager := newManager(t, "v1", key(1), nil)
	workspace := uuid.New()
	ctx := context.Background()

	dataKey, err := NewDataKey()
	require.NoError(t, err)
	require.Len(t, dataKey, DataKeySize)

	wrapped, err := manager.Wrap(ctx, workspace, dataKey)
	require.NoError(t, err)
	require.Equal(t, "v1", wrapped.KeyID)
	require.NotEmpty(t, wrapped.Bytes)
	require.NotEqual(t, string(dataKey), string(wrapped.Bytes), "the data key must not be stored in the clear")

	opened, err := manager.Unwrap(ctx, workspace, wrapped)
	require.NoError(t, err)
	require.Equal(t, dataKey, opened)
}

// TestAWrapOnlyOpensForItsOwnWorkspace is why the workspace is bound into the
// wrap: a stored value moved to another workspace's row must not open there.
func TestAWrapOnlyOpensForItsOwnWorkspace(t *testing.T) {
	manager := newManager(t, "v1", key(2), nil)
	ctx := context.Background()

	dataKey, err := NewDataKey()
	require.NoError(t, err)

	wrapped, err := manager.Wrap(ctx, uuid.New(), dataKey)
	require.NoError(t, err)

	_, err = manager.Unwrap(ctx, uuid.New(), wrapped)
	require.Error(t, err, "a wrap bound to one workspace must not open as another's")
}

// TestRotationKeepsAnExistingConnectionWorking is the acceptance gate for a key
// rotation: the data key still opens, and the next wrap records the new key.
func TestRotationKeepsAnExistingConnectionWorking(t *testing.T) {
	ctx := context.Background()
	workspace := uuid.New()

	oldKey := key(3)
	before := newManager(t, "v1", oldKey, nil)

	dataKey, err := NewDataKey()
	require.NoError(t, err)
	wrapped, err := before.Wrap(ctx, workspace, dataKey)
	require.NoError(t, err)
	require.Equal(t, "v1", wrapped.KeyID)

	// The operator rotates: v2 becomes active and v1 is kept as retired.
	after := newManager(t, "v2", key(4), map[string]string{"v1": oldKey})
	require.Equal(t, "v2", after.ActiveKeyID())
	require.Equal(t, []string{"v1"}, after.RetiredIDs())

	opened, err := after.Unwrap(ctx, workspace, wrapped)
	require.NoError(t, err, "a wrap made under the retired key must still open")
	require.Equal(t, dataKey, opened)

	// And the next wrap uses the new key, so the rotation converges.
	rewrapped, err := after.Wrap(ctx, workspace, opened)
	require.NoError(t, err)
	require.Equal(t, "v2", rewrapped.KeyID)
	require.NotEqual(t, wrapped.Bytes, rewrapped.Bytes)

	back, err := after.Unwrap(ctx, workspace, rewrapped)
	require.NoError(t, err)
	require.Equal(t, dataKey, back)
}

// TestADroppedKeyIsReported is the failure an operator causes by rotating and
// removing the old key from the configuration: it must name what is missing.
func TestADroppedKeyIsReported(t *testing.T) {
	ctx := context.Background()
	workspace := uuid.New()

	before := newManager(t, "v1", key(5), nil)
	dataKey, err := NewDataKey()
	require.NoError(t, err)
	wrapped, err := before.Wrap(ctx, workspace, dataKey)
	require.NoError(t, err)

	// v2 is active and v1 was not carried over.
	after := newManager(t, "v2", key(6), nil)

	_, err = after.Unwrap(ctx, workspace, wrapped)
	require.ErrorIs(t, err, ErrUnknownKey)
	require.Contains(t, err.Error(), "v1", "the error must name the key that is missing")
}

// TestAMissingConfigurationIsNotAStartupFailure keeps a deployment without a key
// running: integrations are simply unavailable.
func TestAMissingConfigurationIsNotAStartupFailure(t *testing.T) {
	manager, err := NewLocalFromConfig("", "", "")
	require.NoError(t, err)
	require.Nil(t, manager)

	var absent *Local
	_, err = absent.Wrap(context.Background(), uuid.New(), make([]byte, DataKeySize))
	require.ErrorIs(t, err, ErrNoKey)
	require.Empty(t, absent.ActiveKeyID())
}

// TestRetiredKeysAreParsedFromConfiguration covers the environment format.
func TestRetiredKeysAreParsedFromConfiguration(t *testing.T) {
	manager, err := NewLocalFromConfig(key(7), "v2", "v1:"+key(8)+", v0:"+key(9))
	require.NoError(t, err)
	require.Equal(t, "v2", manager.ActiveKeyID())
	require.Equal(t, []string{"v0", "v1"}, manager.RetiredIDs())

	// A pair without an id is a configuration mistake, not a silent skip.
	_, err = NewLocalFromConfig(key(7), "v2", "no-colon-here")
	require.ErrorContains(t, err, "id:key")

	_, err = NewLocalFromConfig(key(7), "v2", ":")
	require.ErrorContains(t, err, "id:key")
}

// TestAnUnnamedActiveKeyGetsAStableID keeps a single-key deployment working
// without ceremony, while still recording something a rotation can refer to.
func TestAnUnnamedActiveKeyGetsAStableID(t *testing.T) {
	first, err := NewLocalFromConfig(key(10), "", "")
	require.NoError(t, err)
	second, err := NewLocalFromConfig(key(10), "", "")
	require.NoError(t, err)

	require.NotEmpty(t, first.ActiveKeyID())
	require.Equal(t, first.ActiveKeyID(), second.ActiveKeyID(), "the id must be stable across restarts")
	require.NotContains(t, first.ActiveKeyID(), "=", "the id is a fingerprint, not the key")
}

// TestAWrapOfTheWrongSizeIsRefused keeps a programming mistake from becoming a
// stored value nothing can open.
func TestAWrapOfTheWrongSizeIsRefused(t *testing.T) {
	manager := newManager(t, "v1", key(11), nil)

	_, err := manager.Wrap(context.Background(), uuid.New(), []byte("too short"))
	require.ErrorContains(t, err, "32 bytes")
}

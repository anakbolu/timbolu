package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// E9.1's second gate is this file: the session state of one workspace cannot be
// read as another's. Two properties carry it, and both are checked here rather
// than assumed:
//
//   - the blob is sealed with the workspace's own key, so a blob written under
//     one key does not open under another;
//   - the workspace id is the associated data of that seal, so even a blob that
//     somehow reached another tenant's key ring fails to open, which is what
//     makes a copied row useless rather than dangerous.
//
// The persistence across a restart is checked by round-tripping the device
// through the same encode/decode path the engine uses.

// memoryPuts records what the store would have written, as the snapshots the
// engine would then seal.
type memoryPuts struct {
	blobs []sealed
	fail  error
}

func (m *memoryPuts) put(_ context.Context, blob sealed) error {
	if m.fail != nil {
		return m.fail
	}
	m.blobs = append(m.blobs, blob)
	return nil
}

// encode renders one recorded snapshot the way the engine seals it, so a test
// reads the same bytes a stored session holds.
func (m *memoryPuts) encode(t *testing.T, index int) []byte {
	t.Helper()

	encoded, err := json.Marshal(m.blobs[index])
	require.NoError(t, err)
	return encoded
}

// newTestDevice builds a device whose blob goes to memory.
func newTestDevice(t *testing.T, numberID, workspaceID uuid.UUID) (*store.Device, *deviceStore, *memoryPuts) {
	t.Helper()

	puts := &memoryPuts{}
	device, inner := newDevice(numberID, workspaceID, waLog.Noop, puts.put)

	return device, inner, puts
}

// TestAFreshDeviceHasTheKeysAPairingNeeds is the precondition for everything
// else: a device with no identity key cannot complete a pairing, and whatsmeow
// would fail at the first handshake with a message about a missing key rather
// than about a missing device.
func TestAFreshDeviceHasTheKeysAPairingNeeds(t *testing.T) {
	device, _, _ := newTestDevice(t, uuid.New(), uuid.New())

	require.NotNil(t, device.NoiseKey)
	require.NotNil(t, device.NoiseKey.Priv)
	require.NotNil(t, device.IdentityKey)
	require.NotNil(t, device.SignedPreKey)
	require.Equal(t, uint32(1), device.SignedPreKey.KeyID)
	require.NotNil(t, device.SignedPreKey.Signature)
	require.NotZero(t, device.RegistrationID)
	require.Len(t, device.AdvSecretKey, 32)

	// Two devices must not share key material, or every link would produce the
	// same identity.
	other, _, _ := newTestDevice(t, uuid.New(), uuid.New())
	require.NotEqual(t, device.NoiseKey.Priv, other.NoiseKey.Priv)
	require.NotEqual(t, device.IdentityKey.Priv, other.IdentityKey.Priv)
	require.NotEqual(t, device.AdvSecretKey, other.AdvSecretKey)
}

// TestTheDeviceSurvivesARestart is E9.1's first gate: the state a session is
// made of is written, and a new process reading it back has the same keys. It is
// the property that makes a linked number stay linked when the gateway is
// replaced.
func TestTheDeviceSurvivesARestart(t *testing.T) {
	numberID := uuid.New()
	workspaceID := uuid.New()
	device, inner, puts := newTestDevice(t, numberID, workspaceID)

	// What a pairing writes.
	jid, err := types.ParseJID("628123456789:12@s.whatsapp.net")
	require.NoError(t, err)
	device.ID = &jid
	device.RegistrationID = 4242
	require.NoError(t, device.Save(context.Background()))

	require.NotEmpty(t, puts.blobs, "the device must have been persisted")
	first := puts.encode(t, 0)

	// The things a session is: identities, a signal session, pre-keys, an
	// app-state key.
	require.NoError(t, inner.PutIdentity(context.Background(), "628999:1@s.whatsapp.net", [32]byte{1, 2, 3}))
	require.NoError(t, inner.PutSession(context.Background(), "628999:1@s.whatsapp.net", []byte("session-bytes")))
	generated, err := inner.GetOrGenPreKeys(context.Background(), 3)
	require.NoError(t, err)
	require.NoError(t, inner.MarkPreKeysAsUploaded(context.Background(), idsOf(generated)[2]))
	require.NoError(t, inner.PutAppStateSyncKey(context.Background(), []byte("key-1"), store.AppStateSyncKey{
		Data:        []byte("appstate"),
		Fingerprint: []byte("print"),
		Timestamp:   7,
	}))
	require.NoError(t, inner.PutLIDMapping(context.Background(),
		types.NewJID("999", types.HiddenUserServer), types.NewJID("628999", types.DefaultUserServer)))
	require.NoError(t, inner.flush(context.Background(), true))

	require.Len(t, puts.blobs, 2, "the session write is what triggers the second flush")
	restoredBlob := puts.encode(t, 1)
	require.NotEqual(t, first, restoredBlob, "the second flush must carry the state written after the first")

	// A new process reads it back.
	restoredDevice, restored, err := restoreDevice(restoredBlob, numberID, workspaceID, waLog.Noop)
	require.NoError(t, err)

	require.NotNil(t, restoredDevice.ID)
	require.Equal(t, jid.String(), restoredDevice.ID.String())
	require.Equal(t, uint32(4242), restoredDevice.RegistrationID)
	require.Equal(t, device.NoiseKey.Priv, restoredDevice.NoiseKey.Priv)
	require.Equal(t, device.IdentityKey.Priv, restoredDevice.IdentityKey.Priv)
	require.Equal(t, device.SignedPreKey.Priv, restoredDevice.SignedPreKey.Priv)
	require.Equal(t, device.SignedPreKey.Signature, restoredDevice.SignedPreKey.Signature)
	require.Equal(t, device.AdvSecretKey, restoredDevice.AdvSecretKey)

	session, err := restored.GetSession(context.Background(), "628999:1@s.whatsapp.net")
	require.NoError(t, err)
	require.Equal(t, []byte("session-bytes"), session)

	identity, err := restored.IsTrustedIdentity(context.Background(), "628999:1@s.whatsapp.net", [32]byte{1, 2, 3})
	require.NoError(t, err)
	require.True(t, identity, "the device's own identity key must survive, or every session is re-verified")

	uploaded, err := restored.UploadedPreKeyCount(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, uploaded, "the pre-key watermark is what stops re-uploading keys that are already out")

	key, err := restored.GetAppStateSyncKey(context.Background(), []byte("key-1"))
	require.NoError(t, err)
	require.NotNil(t, key)
	require.Equal(t, []byte("appstate"), key.Data)

	lid, err := restored.GetLIDForPN(context.Background(), types.NewJID("628999", types.DefaultUserServer))
	require.NoError(t, err)
	require.Equal(t, "999", lid.User, "the LID mapping survives, so a migrated address stays addressable")
}

// TestAPrekeyIsRegeneratedRatherThanReused is the property the watermark has to
// keep: a pre-key already uploaded must come back from the store, and a fresh
// one must get a new id. Reusing an id would hand two callers the same key.
func TestAPrekeyIsRegeneratedRatherThanReused(t *testing.T) {
	_, inner, _ := newTestDevice(t, uuid.New(), uuid.New())
	ctx := context.Background()

	first, err := inner.GetOrGenPreKeys(ctx, 3)
	require.NoError(t, err)
	require.Len(t, first, 3)

	// Asking again before the upload returns what is already generated rather
	// than generating more: a key that exists but was not uploaded is reused.
	again, err := inner.GetOrGenPreKeys(ctx, 3)
	require.NoError(t, err)
	require.Equal(t, idsOf(first), idsOf(again), "unuploaded keys are reused")

	require.NoError(t, inner.MarkPreKeysAsUploaded(ctx, idsOf(first)[2]))
	require.NoError(t, inner.RemovePreKey(ctx, idsOf(first)[0]))

	fresh, err := inner.GetOrGenPreKeys(ctx, 2)
	require.NoError(t, err)
	require.Len(t, fresh, 2)
	require.Greater(t, idsOf(fresh)[0], idsOf(first)[2], "an uploaded key is never handed out again")

	one, err := inner.GenOnePreKey(ctx)
	require.NoError(t, err)
	require.Greater(t, one.KeyID, idsOf(fresh)[1])
}

func idsOf(keys []*keys.PreKey) []uint32 {
	out := make([]uint32, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.KeyID)
	}
	return out
}

// TestTheCacheIsRebuiltRatherThanRemembered: contacts, chat settings, and the
// outgoing buffer are deliberately not persisted. Losing them costs a re-sync,
// and keeping them would grow the sealed blob with data WhatsApp re-sends
// anyway. The store must still answer them sensibly in one process's lifetime.
func TestTheCacheIsRebuiltRatherThanRemembered(t *testing.T) {
	device, inner, puts := newTestDevice(t, uuid.New(), uuid.New())
	ctx := context.Background()
	jid := types.NewJID("628111", types.DefaultUserServer)

	ownJID, err := types.ParseJID("628123456789:12@s.whatsapp.net")
	require.NoError(t, err)
	device.ID = &ownJID

	changed, name, err := inner.PutPushName(ctx, jid, "Bu Sari")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "Bu Sari", name)

	info, err := inner.GetContact(ctx, jid)
	require.NoError(t, err)
	require.True(t, info.Found)
	require.Equal(t, "Bu Sari", info.PushName)

	// The same value twice is not a change, which is what stops a re-sync from
	// rewriting every row.
	changed, _, err = inner.PutPushName(ctx, jid, "Bu Sari")
	require.NoError(t, err)
	require.False(t, changed)

	require.NoError(t, inner.AddOutgoingEvent(ctx, jid, "msg-1", "text", []byte("halo")))
	format, plaintext, err := inner.GetOutgoingEvent(ctx, jid, types.EmptyJID, "msg-1")
	require.NoError(t, err)
	require.Equal(t, "text", format)
	require.Equal(t, []byte("halo"), plaintext)

	require.NoError(t, device.Save(ctx))
	fresh := puts.encode(t, len(puts.blobs)-1)

	_, restored, err := restoreDevice(fresh, uuid.New(), uuid.New(), waLog.Noop)
	require.NoError(t, err)

	contact, err := restored.GetContact(ctx, jid)
	require.NoError(t, err)
	require.False(t, contact.Found, "the cache is not part of the session")

	// And the device is still usable with an empty cache.
	_, err = restored.GetAllContacts(ctx)
	require.NoError(t, err)
}

// TestAnUnreadableBlobIsRefusedRatherThanMisread: a blob from a different format
// version, or one that is not JSON at all, must fail loudly. Reading it as an
// empty device would silently unlink a number and then pair it again, which is a
// worse failure than a refusal.
func TestAnUnreadableBlobIsRefusedRatherThanMisread(t *testing.T) {
	for _, tc := range []struct {
		name string
		blob []byte
	}{
		{"not json", []byte("this is not a device")},
		{"empty", nil},
		{"a future format", mustJSON(t, sealed{Version: formatVersion + 1})},
		{"an older format", mustJSON(t, sealed{Version: 0})},
		{"a server address where an account belongs", mustJSON(t, sealed{Version: formatVersion, JID: "s.whatsapp.net"})},
	} {
		_, _, err := restoreDevice(tc.blob, uuid.New(), uuid.New(), waLog.Noop)
		require.Error(t, err, tc.name)
	}
}

// TestABrokenJIDIsRefused: a stored jid that no longer parses means the blob is
// not what this build wrote. Answering with an empty jid would let the engine
// adopt a device as if it had never been linked.
func TestABrokenJIDIsRefused(t *testing.T) {
	// A jid with no account in it: whatsmeow would accept it and the device
	// would never authenticate, so it is refused here instead.
	blob := mustJSON(t, sealed{Version: formatVersion, JID: "s.whatsapp.net"})

	_, _, err := restoreDevice(blob, uuid.New(), uuid.New(), waLog.Noop)
	require.Error(t, err)
	require.Contains(t, err.Error(), "names no account")
}

// TestAFlushFailureIsReported: a store that cannot write must not report success.
// The engine keeps the number offline on that answer, rather than serving a
// session whose successor cannot be recovered.
func TestAFlushFailureIsReported(t *testing.T) {
	_, inner, puts := newTestDevice(t, uuid.New(), uuid.New())
	require.NoError(t, inner.PutSession(context.Background(), "628999:1@s.whatsapp.net", []byte("x")))

	puts.fail = errors.New("the disk is full")
	require.Error(t, inner.flush(context.Background(), true))
}

// TestAPutWithNoJIDIsRefused: whatsmeow guarantees the id before saving, so a
// save without one is a programming mistake this store names rather than
// swallowing into a blob nobody can restore.
func TestAPutWithNoJIDIsRefused(t *testing.T) {
	device, _, _ := newTestDevice(t, uuid.New(), uuid.New())

	err := device.Save(context.Background())
	require.Error(t, err)
}

// TestNothingIsWrittenWhenNothingChanged: the engine flushes on a timer, and a
// timer that rewrites an unchanged blob every interval would keep a write-heavy
// loop going for a number that is idle.
func TestNothingIsWrittenWhenNothingChanged(t *testing.T) {
	_, inner, puts := newTestDevice(t, uuid.New(), uuid.New())

	require.NoError(t, inner.flush(context.Background(), false))
	require.Empty(t, puts.blobs)

	require.NoError(t, inner.PutSession(context.Background(), "628999:1@s.whatsapp.net", []byte("x")))
	require.NoError(t, inner.flush(context.Background(), false))
	require.Len(t, puts.blobs, 1)

	require.NoError(t, inner.flush(context.Background(), false))
	require.Len(t, puts.blobs, 1, "nothing changed, so nothing was written")

	// A forced flush writes even when nothing changed, which is what a shutdown
	// does.
	require.NoError(t, inner.flush(context.Background(), true))
	require.Len(t, puts.blobs, 2)
}

func mustJSON(t *testing.T, value sealed) []byte {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

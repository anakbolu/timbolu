package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// The store answers every method whatsmeow asks it for, and this file is where that
// is checked method by method.
//
// It matters more than it looks. This is the one part of the gateway with a fixed
// interface it does not own: whatsmeow calls these while a connection is being set up
// and while messages are being decrypted, and a method that answers wrongly is a
// handshake that fails or a message that cannot be read — at a point where the only
// symptom is "the number does not work". A round-trip through each one is cheap and
// proves the store keeps what it was given.

// newStoreFor builds a device store with a workspace, ready to be asked.
func newStoreFor(t *testing.T) *deviceStore {
	t.Helper()

	_, inner, _ := newTestDevice(t, uuid.New(), uuid.New())
	inner.put = nil

	return inner
}

// TestTheStoreRoundTripsEverySessionInterface exercises the parts a connection is
// built from: identities, sessions, pre-keys, sender keys, and the app-state keys.
// The groups are separate because whatsmeow treats them as separate stores even
// though one device holds them.
func TestTheStoreRoundTripsEverySessionInterface(t *testing.T) {
	s := newStoreFor(t)
	ctx := t.Context()
	address := "628999:1"

	// Identities: the key a session is verified against.
	key := [32]byte{1, 2, 3, 4}
	require.NoError(t, s.PutIdentity(ctx, address, key))
	trusted, err := s.IsTrustedIdentity(ctx, address, key)
	require.NoError(t, err)
	require.True(t, trusted)
	trusted, err = s.IsTrustedIdentity(ctx, address, [32]byte{9, 9})
	require.NoError(t, err)
	require.False(t, trusted, "a key that changed is not trusted")
	// An address nobody has seen is accepted: whatsmeow asks about strangers on
	// every first message, and refusing them would refuse every new contact.
	trusted, err = s.IsTrustedIdentity(ctx, "628111:1", key)
	require.NoError(t, err)
	require.True(t, trusted)

	require.NoError(t, s.DeleteIdentity(ctx, address))

	// Sessions, one and many.
	require.NoError(t, s.PutSession(ctx, address, []byte("one")))
	ok, err := s.HasSession(ctx, address)
	require.NoError(t, err)
	require.True(t, ok)

	session, err := s.GetSession(ctx, address)
	require.NoError(t, err)
	require.Equal(t, []byte("one"), session)

	missing, err := s.GetSession(ctx, "628000:1")
	require.NoError(t, err)
	require.Nil(t, missing, "an address with no session answers nil rather than an error")

	require.NoError(t, s.PutManySessions(ctx, map[string][]byte{
		"628888:1": []byte("two"),
		"628777:1": []byte("three"),
	}))
	many, err := s.GetManySessions(ctx, []string{"628888:1", "628777:1", "nobody"})
	require.NoError(t, err)
	require.Len(t, many, 2, "only the addresses that have a session come back")

	require.NoError(t, s.DeleteSession(ctx, address))
	has, err := s.HasSession(ctx, address)
	require.NoError(t, err)
	require.False(t, has)

	// Pre-keys: generated, told apart, and removed once used.
	generated, err := s.GetOrGenPreKeys(ctx, 4)
	require.NoError(t, err)
	require.Len(t, generated, 4)

	first, err := s.GetPreKey(ctx, generated[0].KeyID)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, generated[0].KeyID, first.KeyID)

	missingKey, err := s.GetPreKey(ctx, 99999)
	require.NoError(t, err)
	require.Nil(t, missingKey, "a pre-key nobody generated answers nil")

	require.NoError(t, s.RemovePreKey(ctx, generated[0].KeyID))
	missingKey, err = s.GetPreKey(ctx, generated[0].KeyID)
	require.NoError(t, err)
	require.Nil(t, missingKey)

	uploaded, err := s.UploadedPreKeyCount(ctx)
	require.NoError(t, err)
	require.Zero(t, uploaded)
	require.NoError(t, s.MarkPreKeysAsUploaded(ctx, generated[3].KeyID))
	uploaded, err = s.UploadedPreKeyCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, uploaded, "three of the four remain, and the uploaded watermark says which")

	// Sender keys, per group and per member.
	require.NoError(t, s.PutSenderKey(ctx, "group-1", "628999", []byte("sender-key")))
	senderKey, err := s.GetSenderKey(ctx, "group-1", "628999")
	require.NoError(t, err)
	require.Equal(t, []byte("sender-key"), senderKey)

	unknownSenderKey, err := s.GetSenderKey(ctx, "group-2", "628999")
	require.NoError(t, err)
	require.Nil(t, unknownSenderKey, "a group nobody wrote to answers nil")

	// App-state sync keys: the ones that survive a restart too.
	keyID := []byte("key-1")
	require.NoError(t, s.PutAppStateSyncKey(ctx, keyID, store.AppStateSyncKey{
		Data: []byte("data"), Fingerprint: []byte("print"), Timestamp: 11,
	}))

	latest, err := s.GetLatestAppStateSyncKeyID(ctx)
	require.NoError(t, err)
	require.Equal(t, keyID, latest, "the newest key is the latest")

	require.NoError(t, s.PutAppStateSyncKey(ctx, []byte("key-2"), store.AppStateSyncKey{Data: []byte("more")}))
	latest, err = s.GetLatestAppStateSyncKeyID(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("key-2"), latest)

	all, err := s.GetAllAppStateSyncKeys(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	fetched, err := s.GetAppStateSyncKey(ctx, keyID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	require.Equal(t, []byte("data"), fetched.Data)

	nothing, err := s.GetAppStateSyncKey(ctx, []byte("key-3"))
	require.NoError(t, err)
	require.Nil(t, nothing)

	// With no key at all, the answer is nil rather than a zero id: whatsmeow asks
	// this before the first sync and has to tell "none yet" from "the empty one".
	empty := newStoreFor(t)
	none, err := empty.GetLatestAppStateSyncKeyID(ctx)
	require.NoError(t, err)
	require.Empty(t, none)

	require.NoError(t, s.DeleteAppStateVersion(ctx, "regular"))
	require.NoError(t, s.PutNCTSalt(ctx, []byte("salt")))
	salt, err := s.GetNCTSalt(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte("salt"), salt)
	require.NoError(t, s.DeleteNCTSalt(ctx))
	salt, err = s.GetNCTSalt(ctx)
	require.NoError(t, err)
	require.Empty(t, salt)
}

// TestTheStoreRoundTripsTheAppStateVersions: the version of each collection is what
// tells whatsmeow whether it is behind. A version that does not come back is a
// re-sync on every start, and a mutation that was applied twice is a contact list
// with the same change twice.
func TestTheStoreRoundTripsTheAppStateVersions(t *testing.T) {
	s := newStoreFor(t)
	ctx := t.Context()

	// Nothing recorded answers zero and an empty hash rather than an error.
	version, hash, err := s.GetAppStateVersion(ctx, "regular")
	require.NoError(t, err)
	require.Zero(t, version)
	require.Equal(t, [128]byte{}, hash)

	var fingerprint [128]byte
	fingerprint[0] = 7
	fingerprint[127] = 9
	require.NoError(t, s.PutAppStateVersion(ctx, "regular", 42, fingerprint))

	version, hash, err = s.GetAppStateVersion(ctx, "regular")
	require.NoError(t, err)
	require.Equal(t, uint64(42), version)
	require.Equal(t, fingerprint, hash)

	// Mutations: recorded by index, read back, and forgotten once superseded.
	require.NoError(t, s.PutAppStateMutationMACs(ctx, "regular", 42, []store.AppStateMutationMAC{
		{IndexMAC: []byte("index-1"), ValueMAC: []byte("value-1")},
		{IndexMAC: []byte("index-2"), ValueMAC: []byte("value-2")},
	}))

	value, err := s.GetAppStateMutationMAC(ctx, "regular", []byte("index-1"))
	require.NoError(t, err)
	require.Equal(t, []byte("value-1"), value)

	unknown, err := s.GetAppStateMutationMAC(ctx, "regular", []byte("index-9"))
	require.NoError(t, err)
	require.Nil(t, unknown)

	require.NoError(t, s.DeleteAppStateMutationMACs(ctx, "regular", [][]byte{[]byte("index-1")}))
	forgotten, err := s.GetAppStateMutationMAC(ctx, "regular", []byte("index-1"))
	require.NoError(t, err)
	require.Nil(t, forgotten)

	// Dropping the collection's version drops its mutations with it: a version that
	// was thrown away has no mutations to reconcile against.
	require.NoError(t, s.DeleteAppStateVersion(ctx, "regular"))
	version, _, err = s.GetAppStateVersion(ctx, "regular")
	require.NoError(t, err)
	require.Zero(t, version)
	dropped, err := s.GetAppStateMutationMAC(ctx, "regular", []byte("index-2"))
	require.NoError(t, err)
	require.Nil(t, dropped)
}

// TestTheStoreRoundTripsContactsAndChatSettings covers the cache: it is deliberately
// not persisted, and it still has to work for the lifetime of a connection.
//
// The "changed" answers matter as much as the values: whatsmeow uses them to decide
// whether a re-sync is worth writing, and answering "changed" for every message is a
// write loop over a contact list.
func TestTheStoreRoundTripsContactsAndChatSettings(t *testing.T) {
	s := newStoreFor(t)
	ctx := t.Context()
	jid := types.NewJID("628999", types.DefaultUserServer)

	changed, name, err := s.PutPushName(ctx, jid, "Bu Sari")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "Bu Sari", name)

	changed, _, err = s.PutPushName(ctx, jid, "Bu Sari")
	require.NoError(t, err)
	require.False(t, changed, "the same name twice is not a change")

	changed, _, err = s.PutBusinessName(ctx, jid, "Toko Sinar")
	require.NoError(t, err)
	require.True(t, changed)
	changed, _, err = s.PutBusinessName(ctx, jid, "Toko Sinar")
	require.NoError(t, err)
	require.False(t, changed)

	require.NoError(t, s.PutContactName(ctx, jid, "Sari Dewi", "Sari"))
	require.NoError(t, s.PutAllContactNames(ctx, []store.ContactEntry{
		{JID: types.NewJID("628111", types.DefaultUserServer), FirstName: "Ani", FullName: "Ani S"},
	}))
	require.NoError(t, s.PutManyRedactedPhones(ctx, []store.RedactedPhoneEntry{
		{JID: types.NewJID("628222", types.HiddenUserServer), RedactedPhone: "+62∙∙∙∙∙∙∙∙80"},
	}))

	info, err := s.GetContact(ctx, jid)
	require.NoError(t, err)
	require.True(t, info.Found)
	require.Equal(t, "Bu Sari", info.PushName)
	require.Equal(t, "Toko Sinar", info.BusinessName)
	require.Equal(t, "Sari Dewi", info.FullName)
	require.Equal(t, "Sari", info.FirstName)

	stranger, err := s.GetContact(ctx, types.NewJID("628000", types.DefaultUserServer))
	require.NoError(t, err)
	require.False(t, stranger.Found, "a contact nobody has seen is not found")

	all, err := s.GetAllContacts(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "Ani S", all[types.NewJID("628111", types.DefaultUserServer)].FullName)

	// Chat settings: muted, pinned, archived, and the WAS root secret id.
	muted := time.Now().Add(time.Hour)
	require.NoError(t, s.PutMutedUntil(ctx, jid, muted))
	require.NoError(t, s.PutPinned(ctx, jid, true))
	require.NoError(t, s.PutArchived(ctx, jid, true))
	require.NoError(t, s.PutWASARootSecretID(ctx, jid, types.MessageID("msg-root")))

	settings, err := s.GetChatSettings(ctx, jid)
	require.NoError(t, err)
	require.True(t, settings.Found)
	require.True(t, settings.Pinned)
	require.True(t, settings.Archived)
	require.WithinDuration(t, muted, settings.MutedUntil, time.Second)

	root, err := s.GetWASARootSecretID(ctx, jid)
	require.NoError(t, err)
	require.Equal(t, types.MessageID("msg-root"), root)

	fresh, err := s.GetChatSettings(ctx, types.NewJID("628000", types.DefaultUserServer))
	require.NoError(t, err)
	require.False(t, fresh.Found)
}

// TestTheStoreRoundTripsSecretsAndTokens: message secrets are what allow a message
// the device sent to be decrypted by the device itself, and privacy tokens are what
// allow a status to be posted. Both are cache, and both have to answer while the
// connection lives.
func TestTheStoreRoundTripsSecretsAndTokens(t *testing.T) {
	s := newStoreFor(t)
	ctx := t.Context()
	chat := types.NewJID("628999", types.DefaultUserServer)
	sender := types.NewJID("628999", types.DefaultUserServer)

	require.NoError(t, s.PutMessageSecrets(ctx, []store.MessageSecretInsert{
		{Chat: chat, Sender: sender, ID: "msg-1", Secret: []byte("secret-1")},
	}))
	require.NoError(t, s.PutMessageSecret(ctx, chat, sender, "msg-2", []byte("secret-2")))

	secret, who, err := s.GetMessageSecret(ctx, chat, sender, "msg-1")
	require.NoError(t, err)
	require.Equal(t, []byte("secret-1"), secret)
	require.Equal(t, sender, who)

	secret, _, err = s.GetMessageSecret(ctx, chat, sender, "msg-2")
	require.NoError(t, err)
	require.Equal(t, []byte("secret-2"), secret)

	none, _, err := s.GetMessageSecret(ctx, chat, sender, "msg-9")
	require.NoError(t, err)
	require.Nil(t, none)

	// Privacy tokens, and the sweep that drops the ones older than WhatsApp's own
	// window.
	now := time.Now()
	require.NoError(t, s.PutPrivacyTokens(ctx,
		store.PrivacyToken{User: sender, Token: []byte("fresh"), Timestamp: now},
		store.PrivacyToken{User: types.NewJID("628111", types.DefaultUserServer), Token: []byte("stale"), Timestamp: now.Add(-48 * time.Hour)},
	))

	fresh, err := s.GetPrivacyToken(ctx, sender)
	require.NoError(t, err)
	require.NotNil(t, fresh)
	require.Equal(t, []byte("fresh"), fresh.Token)

	missing, err := s.GetPrivacyToken(ctx, types.NewJID("628000", types.DefaultUserServer))
	require.NoError(t, err)
	require.Nil(t, missing)

	removed, err := s.DeleteExpiredPrivacyTokens(ctx, now.Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), removed, "only the token past the cutoff goes")

	gone, err := s.GetPrivacyToken(ctx, types.NewJID("628111", types.DefaultUserServer))
	require.NoError(t, err)
	require.Nil(t, gone)
}

// TestTheStoreKeepsTheEventBuffer: it is what lets a retry re-encrypt the same body
// rather than a new one, and what lets a message that arrived before its key did be
// decrypted once the key arrives. Both are about a retry being invisible.
func TestTheStoreKeepsTheEventBuffer(t *testing.T) {
	s := newStoreFor(t)
	ctx := t.Context()
	chat := types.NewJID("628999", types.DefaultUserServer)

	var hash [32]byte
	hash[0] = 3
	serverTime := time.Now().Add(-time.Minute)

	require.NoError(t, s.PutBufferedEvent(ctx, hash, []byte("plaintext"), serverTime))

	buffered, err := s.GetBufferedEvent(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, buffered)
	require.Equal(t, []byte("plaintext"), buffered.Plaintext)
	require.WithinDuration(t, serverTime, buffered.ServerTime, time.Second)

	// Once the plaintext is cleared the hash stays, which is what stops the same
	// ciphertext being buffered twice.
	require.NoError(t, s.ClearBufferedEventPlaintext(ctx, hash))
	buffered, err = s.GetBufferedEvent(ctx, hash)
	require.NoError(t, err)
	require.NotNil(t, buffered)
	require.Nil(t, buffered.Plaintext)

	unknown, err := s.GetBufferedEvent(ctx, [32]byte{9})
	require.NoError(t, err)
	require.Nil(t, unknown)

	// The sweep forgets one that has been waiting past the window, so a device that
	// stayed up does not hold every undelivered message forever.
	old := [32]byte{4}
	require.NoError(t, s.PutBufferedEvent(ctx, old, []byte("ancient"), time.Now()))
	s.buffered[old] = store.BufferedEvent{Plaintext: []byte("ancient"), InsertTime: time.Now().Add(-time.Hour)}
	require.NoError(t, s.DeleteOldBufferedHashes(ctx))
	buffered, err = s.GetBufferedEvent(ctx, old)
	require.NoError(t, err)
	require.Nil(t, buffered)

	// Outgoing events: the plaintext of what was sent, so a retry re-uses it. The
	// alternative chat is consulted as well, because a message addressed by LID can
	// be retried by phone number.
	require.NoError(t, s.AddOutgoingEvent(ctx, chat, types.MessageID("sent-1"), "text", []byte("halo")))
	format, plaintext, err := s.GetOutgoingEvent(ctx, chat, types.EmptyJID, types.MessageID("sent-1"))
	require.NoError(t, err)
	require.Equal(t, "text", format)
	require.Equal(t, []byte("halo"), plaintext)

	alt := types.NewJID("628888", types.HiddenUserServer)
	_, plaintext, err = s.GetOutgoingEvent(ctx, chat, alt, types.MessageID("sent-1"))
	require.NoError(t, err)
	require.Equal(t, []byte("halo"), plaintext, "the alternative address finds the same entry")

	_, plaintext, err = s.GetOutgoingEvent(ctx, chat, types.EmptyJID, types.MessageID("never-sent"))
	require.NoError(t, err)
	require.Nil(t, plaintext)

	require.NoError(t, s.DeleteOldOutgoingEvents(ctx))

	// The decryption transaction runs its callback: there is one in-memory state, so
	// there is nothing to isolate, and skipping the callback would silently drop the
	// decryption.
	ran := false
	require.NoError(t, s.DoDecryptionTxn(ctx, func(context.Context) error {
		ran = true
		return nil
	}))
	require.True(t, ran)
}

// TestTheStoreKeepsTheLidMappings: WhatsApp is moving accounts from phone numbers to
// LIDs, and a message addressed one way has to be resolvable the other. Without the
// mapping, a group message from a member the device knows by LID cannot be replied to.
func TestTheStoreKeepsTheLidMappings(t *testing.T) {
	s := newStoreFor(t)
	ctx := t.Context()

	pn := types.NewJID("628999", types.DefaultUserServer)
	lid := types.NewJID("123456789", types.HiddenUserServer)

	require.NoError(t, s.PutLIDMapping(ctx, lid, pn))

	byPN, err := s.GetLIDForPN(ctx, pn)
	require.NoError(t, err)
	require.Equal(t, lid, byPN)

	byLID, err := s.GetPNForLID(ctx, lid)
	require.NoError(t, err)
	require.Equal(t, pn, byLID)

	// Mapping many at once is what the first sync does.
	second := types.NewJID("628888", types.DefaultUserServer)
	secondLID := types.NewJID("987654321", types.HiddenUserServer)
	require.NoError(t, s.PutManyLIDMappings(ctx, []store.LIDMapping{{LID: secondLID, PN: second}}))

	many, err := s.GetManyLIDsForPNs(ctx, []types.JID{pn, second, types.NewJID("628000", types.DefaultUserServer)})
	require.NoError(t, err)
	require.Len(t, many, 2, "only the addresses that have a mapping come back")
	require.Equal(t, secondLID, many[second])

	// An address nobody mapped answers empty rather than an error: whatsmeow falls
	// back to the phone number when there is no LID yet.
	unknown, err := s.GetLIDForPN(ctx, types.NewJID("628000", types.DefaultUserServer))
	require.NoError(t, err)
	require.True(t, unknown.IsEmpty())

	unknownPN, err := s.GetPNForLID(ctx, types.NewJID("111", types.HiddenUserServer))
	require.NoError(t, err)
	require.True(t, unknownPN.IsEmpty())
}

// TestTheStoreReplacesSessionsByPhone is what a migration does: the device's
// sessions are re-keyed from phone numbers to LIDs, and the bytes have to move
// with the key.
//
// The key it moves to is the LID's signal address, not its JID: whatsmeow looks a
// session up as `<user>_<agent>:<device>`, and the agent carries the domain — a LID
// is `_1`, a phone number has none. A key built from the JID would never be found
// again, and the account would lose its sessions the moment WhatsApp migrated it.
// The forms are told apart here on purpose, including a hosted phone number whose
// old key carried a different domain.
func TestTheStoreReplacesSessionsByPhone(t *testing.T) {
	s := newStoreFor(t)
	ctx := t.Context()

	require.NoError(t, s.PutSession(ctx, "628999:12", []byte("direct")))
	require.NoError(t, s.PutSession(ctx, "628999_128:13", []byte("hosted")))
	// A number that merely starts with the same digits is a different account.
	require.NoError(t, s.PutSession(ctx, "6289990:12", []byte("other")))
	require.NoError(t, s.PutIdentity(ctx, "628999:12", [32]byte{1}))
	require.NoError(t, s.PutIdentity(ctx, "6289990:12", [32]byte{2}))

	lid := types.NewJID("123456789", types.HiddenUserServer)
	pn := types.NewJID("628999", types.DefaultUserServer)

	require.NoError(t, s.MigratePNToLID(ctx, pn, lid))

	// The phone-addressed sessions moved to the LID's own signal address — the
	// `_1` domain, and the device each entry carried. The key is written out rather
	// than computed from the JID, because computing it the way whatsmeow does is
	// exactly what the previous version got wrong.
	require.Equal(t, "123456789_1", lid.SignalAddressUser(), "a LID carries its domain in the agent part")

	moved, err := s.GetSession(ctx, "123456789_1:12")
	require.NoError(t, err)
	require.Equal(t, []byte("direct"), moved)

	hosted, err := s.GetSession(ctx, "123456789_1:13")
	require.NoError(t, err)
	require.Equal(t, []byte("hosted"), hosted, "a hosted number's session moves too, under the LID's domain")

	require.NotContains(t, s.sessions, "628999:12", "the phone-addressed entry is gone once it moved")
	require.NotContains(t, s.sessions, "628999_128:13")
	require.NotContains(t, s.sessions, "123456789:12", "and not under a key nothing looks up")

	// The neighbouring number keeps its session: migrating one account must not
	// move another's.
	kept, err := s.GetSession(ctx, "6289990:12")
	require.NoError(t, err)
	require.Equal(t, []byte("other"), kept)

	// Deleting by phone takes that number's identities and leaves the rest. The
	// maps are read directly because the interface cannot tell "deleted" from
	// "never seen": an identity nobody recorded is reported as trusted, which is
	// what makes a re-pair acceptable.
	require.NoError(t, s.DeleteAllIdentities(ctx, "628999"))
	require.NotContains(t, s.identities, "628999:12", "the deleted identity is gone")
	require.Contains(t, s.identities, "6289990:12", "the neighbouring number's identity is untouched")

	// And deleting all sessions by phone clears the ones that number had. A number
	// that merely starts with the same digits is a different account, and its
	// sessions are what a prefix match would have destroyed.
	require.NoError(t, s.PutSession(ctx, "628999:14", []byte("gone")))
	require.NoError(t, s.DeleteAllSessions(ctx, "628999"))
	require.NotContains(t, s.sessions, "628999:14")
	require.Contains(t, s.sessions, "6289990:12", "the neighbouring number's session is untouched")
}

// TestTheStoreWritesWhatItKeptOnADeviceSave is where the split between what survives
// and what does not is checked, from the direction that matters: a device saved the
// way whatsmeow saves it carries the parts a session is made of, and none of the cache.
func TestTheStoreWritesWhatItKeptOnADeviceSave(t *testing.T) {
	numberID, workspaceID := uuid.New(), uuid.New()
	var saved [][]byte

	device, inner := newDevice(numberID, workspaceID, nil, func(_ context.Context, blob sealed) error {
		encoded, err := json.Marshal(blob)
		if err != nil {
			return err
		}
		saved = append(saved, encoded)
		return nil
	})

	ctx := t.Context()
	jid, err := types.ParseJID("628123456789:12@s.whatsapp.net")
	require.NoError(t, err)
	device.ID = &jid

	// Something from each side: a session (persisted) and a contact (not).
	require.NoError(t, inner.PutSession(ctx, "628999:1", []byte("session")))
	require.NoError(t, inner.PutContactName(ctx, types.NewJID("628999", types.DefaultUserServer), "Bu Sari", "Sari"))
	require.NoError(t, device.Save(ctx))

	require.Len(t, saved, 1)
	document := string(saved[0])
	require.Contains(t, document, "session", "a session is part of what survives")
	require.NotContains(t, document, "Bu Sari", "a contact is not: WhatsApp re-sends it")

	_, restored, err := restoreDevice(saved[0], numberID, workspaceID, nil)
	require.NoError(t, err)

	// What came back is usable: the session is there and the cache is simply empty.
	session, err := restored.GetSession(ctx, "628999:1")
	require.NoError(t, err)
	require.Equal(t, []byte("session"), session)

	contact, err := restored.GetContact(ctx, types.NewJID("628999", types.DefaultUserServer))
	require.NoError(t, err)
	require.False(t, contact.Found)

	// And the blob does not carry the workspace in the clear, which is the property
	// the seal adds on top: what is written here is the document, and the engine
	// seals it. The keys are the account's, not the tenant's.
	require.NotContains(t, document, workspaceID.String())
	require.True(t, bytes.Contains(saved[0], []byte("628123456789")), "but the account it answers as is in it")
}

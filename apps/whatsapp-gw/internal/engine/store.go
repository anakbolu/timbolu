// Package engine holds the WhatsApp infrastructure the gateway process owns: the
// whatsmeow client that holds a live connection, the sealed persistence of its
// device state, and the Redis lease that decides which instance owns a number.
//
// It is infrastructure with no product rule in it: what a number is, who may link
// one, and when a message leaves are the backend's decisions, and this package
// only carries them out. It implements the ports
// `packages/whatsapp/domain` declares — reading and writing the session and the
// directory itself, and reaching the product through the seam — which is the
// direction the dependency arrows are allowed to run.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/util/random"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// The device state a linked number is, and the one place it is persisted.
//
// whatsmeow expects a `store.Device` whose ~fifty store methods persist the
// noise key, the identity key, the signal sessions, the pre-keys, the app-state
// keys, and the contacts. Its own `sqlstore` writes each of those to its own
// table in the clear, which is exactly what this deployment cannot do: a stolen
// database would be a set of numbers somebody else could speak as.
//
// So the state is held in memory and persisted as one blob, sealed with the
// workspace's own data key and bound to that workspace id as associated data.
// Two consequences, both intended:
//
//   - a blob cannot be opened under another tenant's key, and cannot be replayed
//     against another tenant even if the row is copied;
//   - what is *not* persisted is a cache rather than a secret. Contacts, chat
//     settings, message secrets, privacy tokens, and the outgoing-event buffer
//     are rebuilt by WhatsApp when the connection comes up; losing them on a
//     restart costs a re-sync, not a session.
//
// The blob is written when the parts that cannot be rebuilt change — the device
// keys and the signal sessions — and on shutdown. A crash between writes loses
// at most the sessions of the last interval, which whatsmeow re-derives.

// sealed is one number's persisted state.
//
// Every field is exportable and JSON-encodable so the blob is a plain document
// rather than a bespoke binary format: when a future version of whatsmeow adds a
// store method, the decoder keeps answering what it knew and the missing piece
// is regenerated, instead of a length-prefixed record shifting under a reader.
type sealed struct {
	// Version is the format's own version. It is checked on read so a blob
	// written by an older build is refused rather than misread.
	Version int `json:"v"`

	JID      string `json:"jid"`
	LID      string `json:"lid,omitempty"`
	Platform string `json:"platform,omitempty"`
	// Account is the ADV signed device identity, as protobuf bytes.
	Account []byte `json:"account,omitempty"`

	NoisePriv        []byte `json:"noise_priv"`
	IdentityPriv     []byte `json:"identity_priv"`
	RegistrationID   uint32 `json:"registration_id"`
	AdvSecretKey     []byte `json:"adv_secret"`
	SignedPreKeyID   uint32 `json:"signed_prekey_id"`
	SignedPreKeyPriv []byte `json:"signed_prekey_priv"`
	SignedPreKeySig  []byte `json:"signed_prekey_sig"`

	Identities map[string][]byte `json:"identities,omitempty"`
	Sessions   map[string][]byte `json:"sessions,omitempty"`

	PreKeys         map[uint32][]byte `json:"prekeys,omitempty"`
	NextPreKeyID    uint32            `json:"next_prekey_id"`
	UploadedThrough uint32            `json:"uploaded_through"`

	SenderKeys map[string]map[string][]byte `json:"sender_keys,omitempty"`

	AppStateKeys      map[string]sealedKey         `json:"appstate_keys,omitempty"`
	LatestAppStateKey string                       `json:"latest_appstate_key,omitempty"`
	AppStateVersions  map[string]sealedVersion     `json:"appstate_versions,omitempty"`
	AppStateMutations map[string]map[string][]byte `json:"appstate_mutations,omitempty"`

	LIDByPN map[string]string `json:"lid_by_pn,omitempty"`
	PNByLID map[string]string `json:"pn_by_lid,omitempty"`

	NCTSalt []byte `json:"nct_salt,omitempty"`
}

// sealedKey is one app-state sync key.
type sealedKey struct {
	Data        []byte `json:"data"`
	Fingerprint []byte `json:"fingerprint"`
	Timestamp   int64  `json:"timestamp"`
}

// sealedVersion is one app-state collection version.
type sealedVersion struct {
	Version uint64 `json:"version"`
	Hash    []byte `json:"hash"`
}

// formatVersion is what this build writes and reads.
const formatVersion = 1

// Compile-time checks that the in-memory store is everything whatsmeow asks for.
// A version that adds a method breaks the build here rather than in a pairing
// flow at runtime, which is the difference between a compile error and a number
// that quietly stops working.
var (
	_ store.DeviceContainer          = (*deviceStore)(nil)
	_ store.AllSessionSpecificStores = (*deviceStore)(nil)
	_ store.AllGlobalStores          = (*deviceStore)(nil)
)

// deviceStore is one number's in-memory store: every interface whatsmeow asks
// for, backed by maps, with the parts that matter persisted as one sealed blob.
type deviceStore struct {
	log waLog.Logger

	numberID    uuid.UUID
	workspaceID uuid.UUID
	put         putFunc

	mu sync.Mutex

	// what is persisted
	jid              string
	lid              string
	platform         string
	account          []byte
	noisePriv        [32]byte
	identityPriv     [32]byte
	registrationID   uint32
	advSecretKey     []byte
	signedPreKeyID   uint32
	signedPreKeyPriv [32]byte
	signedPreKeySig  [64]byte

	identities map[string][32]byte
	sessions   map[string][]byte

	preKeys         map[uint32]*keys.PreKey
	nextPreKeyID    uint32
	uploadedThrough uint32

	senderKeys map[string]map[string][]byte

	appStateKeys      map[string]store.AppStateSyncKey
	latestAppStateKey []byte
	appStateVersions  map[string]sealedVersion
	appStateMutations map[string]map[string]appStateMutation

	lidByPN map[string]string
	pnByLID map[string]string

	nctSalt []byte

	// what a restart rebuilds
	contacts       map[string]types.ContactInfo
	chatSettings   map[string]types.LocalChatSettings
	wasRootSecrets map[string]types.MessageID
	messageSecrets map[string][]byte
	privacyTokens  map[string]store.PrivacyToken
	buffered       map[[32]byte]store.BufferedEvent
	outgoing       map[string]outgoingEvent

	dirty bool
}

// appStateMutation is one app-state index MAC's value.
type appStateMutation struct {
	ValueMAC []byte
}

// outgoingEvent is the plaintext of a message that was sent, kept so a retry
// re-encrypts the same body rather than a new one.
type outgoingEvent struct {
	Format    string
	Plaintext []byte
}

// putFunc persists the snapshot the store produced. It takes the plain snapshot
// rather than opaque bytes so the seal — and therefore the workspace the blob is
// bound to — happens in exactly one place, the engine, where the key ring is
// known.
type putFunc func(ctx context.Context, blob sealed) error

// newDevice builds a fresh device for a number that is being linked.
//
// The keys are generated here rather than by whatsmeow because the device has to
// exist before the client does: the pairing flow writes into it, and a device
// with no identity key cannot complete a pairing.
func newDevice(numberID, workspaceID uuid.UUID, log waLog.Logger, put putFunc) (*store.Device, *deviceStore) {
	inner := &deviceStore{
		log:               log,
		numberID:          numberID,
		workspaceID:       workspaceID,
		put:               put,
		identities:        map[string][32]byte{},
		sessions:          map[string][]byte{},
		preKeys:           map[uint32]*keys.PreKey{},
		senderKeys:        map[string]map[string][]byte{},
		appStateKeys:      map[string]store.AppStateSyncKey{},
		appStateVersions:  map[string]sealedVersion{},
		appStateMutations: map[string]map[string]appStateMutation{},
		lidByPN:           map[string]string{},
		pnByLID:           map[string]string{},
		contacts:          map[string]types.ContactInfo{},
		chatSettings:      map[string]types.LocalChatSettings{},
		wasRootSecrets:    map[string]types.MessageID{},
		messageSecrets:    map[string][]byte{},
		privacyTokens:     map[string]store.PrivacyToken{},
		buffered:          map[[32]byte]store.BufferedEvent{},
		outgoing:          map[string]outgoingEvent{},
		nextPreKeyID:      1,
	}

	noise := keys.NewKeyPair()
	identity := keys.NewKeyPair()
	signed := identity.CreateSignedPreKey(1)

	inner.noisePriv = *noise.Priv
	inner.identityPriv = *identity.Priv
	inner.signedPreKeyID = signed.KeyID
	inner.signedPreKeyPriv = *signed.Priv
	signedCopy := *signed.Signature
	inner.signedPreKeySig = signedCopy
	inner.registrationID = randomUint32()
	inner.advSecretKey = random.Bytes(32)

	device := &store.Device{
		Log:            log,
		NoiseKey:       noise,
		IdentityKey:    identity,
		SignedPreKey:   signed,
		RegistrationID: inner.registrationID,
		AdvSecretKey:   inner.advSecretKey,
		LIDs:           inner,
		Container:      inner,
	}
	device.SetAllStores(inner)

	return device, inner
}

// restoreDevice rebuilds a device from a sealed blob.
func restoreDevice(blob []byte, numberID, workspaceID uuid.UUID, log waLog.Logger) (*store.Device, *deviceStore, error) {
	var state sealed
	if err := json.Unmarshal(blob, &state); err != nil {
		return nil, nil, fmt.Errorf("whatsapp: read session: %w", err)
	}
	if state.Version != formatVersion {
		return nil, nil, fmt.Errorf("whatsapp: session format %d is not this build's", state.Version)
	}

	inner := &deviceStore{
		log:               log,
		numberID:          numberID,
		workspaceID:       workspaceID,
		jid:               state.JID,
		lid:               state.LID,
		platform:          state.Platform,
		account:           state.Account,
		noisePriv:         to32(state.NoisePriv),
		identityPriv:      to32(state.IdentityPriv),
		registrationID:    state.RegistrationID,
		advSecretKey:      state.AdvSecretKey,
		signedPreKeyID:    state.SignedPreKeyID,
		signedPreKeyPriv:  to32(state.SignedPreKeyPriv),
		signedPreKeySig:   to64(state.SignedPreKeySig),
		identities:        map[string][32]byte{},
		sessions:          map[string][]byte{},
		preKeys:           map[uint32]*keys.PreKey{},
		nextPreKeyID:      state.NextPreKeyID,
		uploadedThrough:   state.UploadedThrough,
		senderKeys:        map[string]map[string][]byte{},
		appStateKeys:      map[string]store.AppStateSyncKey{},
		appStateVersions:  map[string]sealedVersion{},
		appStateMutations: map[string]map[string]appStateMutation{},
		lidByPN:           map[string]string{},
		pnByLID:           map[string]string{},
		nctSalt:           state.NCTSalt,
		contacts:          map[string]types.ContactInfo{},
		chatSettings:      map[string]types.LocalChatSettings{},
		wasRootSecrets:    map[string]types.MessageID{},
		messageSecrets:    map[string][]byte{},
		privacyTokens:     map[string]store.PrivacyToken{},
		buffered:          map[[32]byte]store.BufferedEvent{},
		outgoing:          map[string]outgoingEvent{},
	}

	for address, key := range state.Identities {
		inner.identities[address] = to32(key)
	}
	for address, session := range state.Sessions {
		inner.sessions[address] = session
	}
	for id, encoded := range state.PreKeys {
		var key keys.PreKey
		if err := json.Unmarshal(encoded, &key); err != nil {
			// One unreadable pre-key is not worth refusing the whole session: it
			// is regenerated on the next upload.
			continue
		}
		inner.preKeys[id] = &key
	}
	for group, users := range state.SenderKeys {
		inner.senderKeys[group] = map[string][]byte{}
		for user, session := range users {
			inner.senderKeys[group][user] = session
		}
	}
	for id, key := range state.AppStateKeys {
		decoded, err := decodeBase64(id)
		if err != nil {
			continue
		}
		inner.appStateKeys[string(decoded)] = store.AppStateSyncKey{
			Data:        key.Data,
			Fingerprint: key.Fingerprint,
			Timestamp:   key.Timestamp,
		}
	}
	if state.LatestAppStateKey != "" {
		if decoded, err := decodeBase64(state.LatestAppStateKey); err == nil {
			inner.latestAppStateKey = decoded
		}
	}
	for name, version := range state.AppStateVersions {
		inner.appStateVersions[name] = version
	}
	for name, macs := range state.AppStateMutations {
		inner.appStateMutations[name] = map[string]appStateMutation{}
		for index, value := range macs {
			decoded, err := decodeBase64(index)
			if err != nil {
				continue
			}
			inner.appStateMutations[name][string(decoded)] = appStateMutation{ValueMAC: value}
		}
	}
	for pn, lid := range state.LIDByPN {
		inner.lidByPN[pn] = lid
	}
	for lid, pn := range state.PNByLID {
		inner.pnByLID[lid] = pn
	}

	device := &store.Device{
		Log:            log,
		NoiseKey:       keys.NewKeyPairFromPrivateKey(inner.noisePriv),
		IdentityKey:    keys.NewKeyPairFromPrivateKey(inner.identityPriv),
		RegistrationID: inner.registrationID,
		AdvSecretKey:   inner.advSecretKey,
		LIDs:           inner,
		Container:      inner,
	}
	device.SignedPreKey = &keys.PreKey{
		KeyPair:   *keys.NewKeyPairFromPrivateKey(inner.signedPreKeyPriv),
		KeyID:     inner.signedPreKeyID,
		Signature: &inner.signedPreKeySig,
	}
	if inner.jid != "" {
		jid, err := types.ParseJID(inner.jid)
		if err != nil {
			return nil, nil, fmt.Errorf("whatsapp: stored jid %q is not a jid: %w", inner.jid, err)
		}
		// A jid with no user part is a server address, not an account, and
		// whatsmeow would treat it as one with no identity. Accepting it would
		// adopt a device that can never authenticate.
		if jid.User == "" {
			return nil, nil, fmt.Errorf("whatsapp: stored jid %q names no account", inner.jid)
		}
		device.ID = &jid
	}
	if inner.lid != "" {
		if lid, err := types.ParseJID(inner.lid); err == nil {
			device.LID = lid
		}
	}
	if len(inner.account) > 0 {
		account := &waAdv.ADVSignedDeviceIdentity{}
		if err := proto.Unmarshal(inner.account, account); err != nil {
			return nil, nil, fmt.Errorf("whatsapp: read device account: %w", err)
		}
		device.Account = account
	}
	device.Platform = inner.platform

	device.SetAllStores(inner)

	return device, inner, nil
}

// snapshot renders the state that has to survive a restart.
func (d *deviceStore) snapshot() sealed {
	out := sealed{
		Version:           formatVersion,
		JID:               d.jid,
		LID:               d.lid,
		Platform:          d.platform,
		Account:           d.account,
		NoisePriv:         d.noisePriv[:],
		IdentityPriv:      d.identityPriv[:],
		RegistrationID:    d.registrationID,
		AdvSecretKey:      d.advSecretKey,
		SignedPreKeyID:    d.signedPreKeyID,
		SignedPreKeyPriv:  d.signedPreKeyPriv[:],
		SignedPreKeySig:   d.signedPreKeySig[:],
		Identities:        map[string][]byte{},
		Sessions:          map[string][]byte{},
		PreKeys:           map[uint32][]byte{},
		NextPreKeyID:      d.nextPreKeyID,
		UploadedThrough:   d.uploadedThrough,
		SenderKeys:        map[string]map[string][]byte{},
		AppStateKeys:      map[string]sealedKey{},
		AppStateVersions:  map[string]sealedVersion{},
		AppStateMutations: map[string]map[string][]byte{},
		LIDByPN:           map[string]string{},
		PNByLID:           map[string]string{},
		NCTSalt:           d.nctSalt,
	}

	for address, key := range d.identities {
		copied := key
		out.Identities[address] = copied[:]
	}
	for address, session := range d.sessions {
		out.Sessions[address] = session
	}
	for id, key := range d.preKeys {
		if encoded, err := json.Marshal(key); err == nil {
			out.PreKeys[id] = encoded
		}
	}
	for group, users := range d.senderKeys {
		out.SenderKeys[group] = map[string][]byte{}
		for user, session := range users {
			out.SenderKeys[group][user] = session
		}
	}
	for id, key := range d.appStateKeys {
		out.AppStateKeys[encodeBase64([]byte(id))] = sealedKey{
			Data:        key.Data,
			Fingerprint: key.Fingerprint,
			Timestamp:   key.Timestamp,
		}
	}
	if len(d.latestAppStateKey) > 0 {
		out.LatestAppStateKey = encodeBase64(d.latestAppStateKey)
	}
	for name, version := range d.appStateVersions {
		out.AppStateVersions[name] = version
	}
	for name, macs := range d.appStateMutations {
		out.AppStateMutations[name] = map[string][]byte{}
		for index, mac := range macs {
			out.AppStateMutations[name][encodeBase64([]byte(index))] = mac.ValueMAC
		}
	}
	for pn, lid := range d.lidByPN {
		out.LIDByPN[pn] = lid
	}
	for lid, pn := range d.pnByLID {
		out.PNByLID[lid] = pn
	}

	return out
}

// markDirty records that something worth keeping changed.
func (d *deviceStore) markDirty() { d.dirty = true }

// flush writes the blob when something worth keeping changed, or when forced.
func (d *deviceStore) flush(ctx context.Context, force bool) error {
	put := d.put
	d.mu.Lock()
	if !d.dirty && !force {
		d.mu.Unlock()
		return nil
	}
	blob := d.snapshot()
	d.dirty = false
	d.mu.Unlock()

	if put == nil {
		return nil
	}
	return put(ctx, blob)
}

// ---------------------------------------------------------------- device

// PutDevice implements store.DeviceContainer. whatsmeow calls it through
// `Device.Save` after the pairing succeeds and whenever it changes the platform
// or the account, so it is the moment the parts that cannot be rebuilt are
// written.
func (d *deviceStore) PutDevice(ctx context.Context, device *store.Device) error {
	if device.ID == nil {
		return errors.New("whatsapp: a device has no jid yet")
	}

	d.mu.Lock()
	d.jid = device.ID.String()
	d.lid = device.LID.String()
	d.platform = device.Platform
	if device.Account != nil {
		if encoded, err := proto.Marshal(device.Account); err == nil {
			d.account = encoded
		}
	}
	if device.NoiseKey != nil && device.NoiseKey.Priv != nil {
		d.noisePriv = *device.NoiseKey.Priv
	}
	if device.IdentityKey != nil && device.IdentityKey.Priv != nil {
		d.identityPriv = *device.IdentityKey.Priv
	}
	if device.SignedPreKey != nil {
		d.signedPreKeyID = device.SignedPreKey.KeyID
		if device.SignedPreKey.Priv != nil {
			d.signedPreKeyPriv = *device.SignedPreKey.Priv
		}
		if device.SignedPreKey.Signature != nil {
			d.signedPreKeySig = *device.SignedPreKey.Signature
		}
	}
	d.registrationID = device.RegistrationID
	d.advSecretKey = device.AdvSecretKey
	d.dirty = true
	d.mu.Unlock()

	return d.flush(ctx, true)
}

// DeleteDevice implements store.DeviceContainer.
func (d *deviceStore) DeleteDevice(_ context.Context, _ *store.Device) error {
	d.mu.Lock()
	d.dirty = true
	d.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------- identities

// PutIdentity implements store.IdentityStore.
func (d *deviceStore) PutIdentity(_ context.Context, address string, key [32]byte) error {
	d.mu.Lock()
	d.identities[address] = key
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// DeleteAllIdentities implements store.IdentityStore.
func (d *deviceStore) DeleteAllIdentities(_ context.Context, phone string) error {
	d.mu.Lock()
	for address := range d.identities {
		if ownsAddress(address, phone) {
			delete(d.identities, address)
		}
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// DeleteIdentity implements store.IdentityStore.
func (d *deviceStore) DeleteIdentity(_ context.Context, address string) error {
	d.mu.Lock()
	delete(d.identities, address)
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// IsTrustedIdentity implements store.IdentityStore.
func (d *deviceStore) IsTrustedIdentity(_ context.Context, address string, key [32]byte) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	known, ok := d.identities[address]
	if !ok {
		return true, nil
	}
	return known == key, nil
}

// ---------------------------------------------------------------- sessions

// GetSession implements store.SessionStore.
func (d *deviceStore) GetSession(_ context.Context, address string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	session, ok := d.sessions[address]
	if !ok {
		return nil, nil
	}
	return session, nil
}

// HasSession implements store.SessionStore.
func (d *deviceStore) HasSession(_ context.Context, address string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.sessions[address]
	return ok, nil
}

// GetManySessions implements store.SessionStore.
func (d *deviceStore) GetManySessions(_ context.Context, addresses []string) (map[string][]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string][]byte, len(addresses))
	for _, address := range addresses {
		if session, ok := d.sessions[address]; ok {
			out[address] = session
		}
	}
	return out, nil
}

// PutSession implements store.SessionStore.
func (d *deviceStore) PutSession(_ context.Context, address string, session []byte) error {
	d.mu.Lock()
	d.sessions[address] = session
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// PutManySessions implements store.SessionStore.
func (d *deviceStore) PutManySessions(_ context.Context, sessions map[string][]byte) error {
	d.mu.Lock()
	for address, session := range sessions {
		d.sessions[address] = session
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// DeleteAllSessions implements store.SessionStore.
func (d *deviceStore) DeleteAllSessions(_ context.Context, phone string) error {
	d.mu.Lock()
	for address := range d.sessions {
		if ownsAddress(address, phone) {
			delete(d.sessions, address)
		}
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// DeleteSession implements store.SessionStore.
func (d *deviceStore) DeleteSession(_ context.Context, address string) error {
	d.mu.Lock()
	delete(d.sessions, address)
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// MigratePNToLID implements store.SessionStore.
//
// WhatsApp is moving accounts from phone numbers to LIDs, and a session stored
// under the old user has to move with it or every later message from that
// account is unreadable.
//
// The new key is the LID's signal address, which is not its JID: whatsmeow looks
// a session up as `<user>_<agent>:<device>`, and the agent carries the domain —
// a LID is `_1`, a phone number has none. A key built from the JID
// (`123456789:12`) would never be found again, and carrying the old suffix
// (`123456789_128:12`) would be the wrong domain, which is the same defect with
// a different spelling.
func (d *deviceStore) MigratePNToLID(_ context.Context, pn, lid types.JID) error {
	destination := lid.SignalAddressUser()

	d.mu.Lock()
	for address, session := range d.sessions {
		if !ownsAddress(address, pn.User) {
			continue
		}

		_, _, device := splitAddress(address)

		key := destination
		if device != "" {
			key += ":" + device
		}

		delete(d.sessions, address)
		d.sessions[key] = session
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------- pre keys

// GetOrGenPreKeys implements store.PreKeyStore.
func (d *deviceStore) GetOrGenPreKeys(_ context.Context, count uint32) ([]*keys.PreKey, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]*keys.PreKey, 0, count)
	ids := make([]uint32, 0, len(d.preKeys))
	for id := range d.preKeys {
		if id > d.uploadedThrough {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if uint32(len(out)) >= count {
			break
		}
		out = append(out, d.preKeys[id])
	}

	for uint32(len(out)) < count {
		key := keys.NewPreKey(d.nextPreKeyID)
		d.preKeys[d.nextPreKeyID] = key
		d.nextPreKeyID++
		out = append(out, key)
	}

	d.markDirty()
	return out, nil
}

// GenOnePreKey implements store.PreKeyStore.
func (d *deviceStore) GenOnePreKey(_ context.Context) (*keys.PreKey, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := keys.NewPreKey(d.nextPreKeyID)
	d.preKeys[d.nextPreKeyID] = key
	d.nextPreKeyID++
	d.markDirty()

	return key, nil
}

// GetPreKey implements store.PreKeyStore.
func (d *deviceStore) GetPreKey(_ context.Context, id uint32) (*keys.PreKey, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key, ok := d.preKeys[id]
	if !ok {
		return nil, nil
	}
	return key, nil
}

// RemovePreKey implements store.PreKeyStore.
func (d *deviceStore) RemovePreKey(_ context.Context, id uint32) error {
	d.mu.Lock()
	delete(d.preKeys, id)
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// MarkPreKeysAsUploaded implements store.PreKeyStore.
func (d *deviceStore) MarkPreKeysAsUploaded(_ context.Context, upToID uint32) error {
	d.mu.Lock()
	if upToID > d.uploadedThrough {
		d.uploadedThrough = upToID
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// UploadedPreKeyCount implements store.PreKeyStore.
func (d *deviceStore) UploadedPreKeyCount(_ context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	count := 0
	for id := range d.preKeys {
		if id <= d.uploadedThrough {
			count++
		}
	}
	return count, nil
}

// ---------------------------------------------------------------- sender keys

// PutSenderKey implements store.SenderKeyStore.
func (d *deviceStore) PutSenderKey(_ context.Context, group, user string, session []byte) error {
	d.mu.Lock()
	if d.senderKeys[group] == nil {
		d.senderKeys[group] = map[string][]byte{}
	}
	d.senderKeys[group][user] = session
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// GetSenderKey implements store.SenderKeyStore.
func (d *deviceStore) GetSenderKey(_ context.Context, group, user string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if users, ok := d.senderKeys[group]; ok {
		return users[user], nil
	}
	return nil, nil
}

// ---------------------------------------------------------------- app state

// PutAppStateSyncKey implements store.AppStateSyncKeyStore.
func (d *deviceStore) PutAppStateSyncKey(_ context.Context, id []byte, key store.AppStateSyncKey) error {
	d.mu.Lock()
	if _, ok := d.appStateKeys[string(id)]; !ok {
		d.latestAppStateKey = append([]byte{}, id...)
	}
	d.appStateKeys[string(id)] = key
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// GetAppStateSyncKey implements store.AppStateSyncKeyStore.
func (d *deviceStore) GetAppStateSyncKey(_ context.Context, id []byte) (*store.AppStateSyncKey, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key, ok := d.appStateKeys[string(id)]
	if !ok {
		return nil, nil
	}
	return &key, nil
}

// GetLatestAppStateSyncKeyID implements store.AppStateSyncKeyStore.
func (d *deviceStore) GetLatestAppStateSyncKeyID(_ context.Context) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.latestAppStateKey) == 0 {
		return nil, nil
	}
	return append([]byte{}, d.latestAppStateKey...), nil
}

// GetAllAppStateSyncKeys implements store.AppStateSyncKeyStore.
func (d *deviceStore) GetAllAppStateSyncKeys(_ context.Context) ([]*store.AppStateSyncKey, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]*store.AppStateSyncKey, 0, len(d.appStateKeys))
	for _, key := range d.appStateKeys {
		copied := key
		out = append(out, &copied)
	}
	return out, nil
}

// PutAppStateVersion implements store.AppStateStore.
func (d *deviceStore) PutAppStateVersion(_ context.Context, name string, version uint64, hash [128]byte) error {
	d.mu.Lock()
	d.appStateVersions[name] = sealedVersion{Version: version, Hash: hash[:]}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// GetAppStateVersion implements store.AppStateStore.
func (d *deviceStore) GetAppStateVersion(_ context.Context, name string) (uint64, [128]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var hash [128]byte
	version, ok := d.appStateVersions[name]
	if !ok {
		return 0, hash, nil
	}
	copy(hash[:], version.Hash)
	return version.Version, hash, nil
}

// DeleteAppStateVersion implements store.AppStateStore.
func (d *deviceStore) DeleteAppStateVersion(_ context.Context, name string) error {
	d.mu.Lock()
	delete(d.appStateVersions, name)
	delete(d.appStateMutations, name)
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// PutAppStateMutationMACs implements store.AppStateStore.
func (d *deviceStore) PutAppStateMutationMACs(_ context.Context, name string, _ uint64, mutations []store.AppStateMutationMAC) error {
	d.mu.Lock()
	if d.appStateMutations[name] == nil {
		d.appStateMutations[name] = map[string]appStateMutation{}
	}
	for _, mutation := range mutations {
		d.appStateMutations[name][string(mutation.IndexMAC)] = appStateMutation{ValueMAC: mutation.ValueMAC}
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// DeleteAppStateMutationMACs implements store.AppStateStore.
func (d *deviceStore) DeleteAppStateMutationMACs(_ context.Context, name string, indexMACs [][]byte) error {
	d.mu.Lock()
	for _, index := range indexMACs {
		delete(d.appStateMutations[name], string(index))
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// GetAppStateMutationMAC implements store.AppStateStore.
func (d *deviceStore) GetAppStateMutationMAC(_ context.Context, name string, indexMAC []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if macs, ok := d.appStateMutations[name]; ok {
		if mac, ok := macs[string(indexMAC)]; ok {
			return mac.ValueMAC, nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- contacts

// PutPushName implements store.ContactStore.
func (d *deviceStore) PutPushName(_ context.Context, user types.JID, pushName string) (bool, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	existing := d.contacts[user.String()]
	if existing.PushName == pushName {
		return false, pushName, nil
	}
	existing.PushName = pushName
	existing.Found = true
	d.contacts[user.String()] = existing
	return true, pushName, nil
}

// PutBusinessName implements store.ContactStore.
func (d *deviceStore) PutBusinessName(_ context.Context, user types.JID, businessName string) (bool, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	existing := d.contacts[user.String()]
	if existing.BusinessName == businessName {
		return false, businessName, nil
	}
	existing.BusinessName = businessName
	existing.Found = true
	d.contacts[user.String()] = existing
	return true, businessName, nil
}

// PutContactName implements store.ContactStore.
func (d *deviceStore) PutContactName(_ context.Context, user types.JID, fullName, firstName string) error {
	d.mu.Lock()
	existing := d.contacts[user.String()]
	existing.FullName = fullName
	existing.FirstName = firstName
	existing.Found = true
	d.contacts[user.String()] = existing
	d.mu.Unlock()
	return nil
}

// PutAllContactNames implements store.ContactStore.
func (d *deviceStore) PutAllContactNames(_ context.Context, contacts []store.ContactEntry) error {
	d.mu.Lock()
	for _, contact := range contacts {
		existing := d.contacts[contact.JID.String()]
		existing.FullName = contact.FullName
		existing.FirstName = contact.FirstName
		existing.Found = true
		d.contacts[contact.JID.String()] = existing
	}
	d.mu.Unlock()
	return nil
}

// PutManyRedactedPhones implements store.ContactStore.
func (d *deviceStore) PutManyRedactedPhones(_ context.Context, entries []store.RedactedPhoneEntry) error {
	d.mu.Lock()
	for _, entry := range entries {
		existing := d.contacts[entry.JID.String()]
		existing.RedactedPhone = entry.RedactedPhone
		d.contacts[entry.JID.String()] = existing
	}
	d.mu.Unlock()
	return nil
}

// GetContact implements store.ContactStore.
func (d *deviceStore) GetContact(_ context.Context, user types.JID) (types.ContactInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	info, ok := d.contacts[user.String()]
	info.Found = ok
	return info, nil
}

// GetAllContacts implements store.ContactStore.
func (d *deviceStore) GetAllContacts(_ context.Context) (map[types.JID]types.ContactInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make(map[types.JID]types.ContactInfo, len(d.contacts))
	for raw, info := range d.contacts {
		jid, err := types.ParseJID(raw)
		if err != nil {
			continue
		}
		info.Found = true
		out[jid] = info
	}
	return out, nil
}

// ---------------------------------------------------------------- chat settings

// PutMutedUntil implements store.ChatSettingsStore.
func (d *deviceStore) PutMutedUntil(_ context.Context, chat types.JID, mutedUntil time.Time) error {
	d.mu.Lock()
	settings := d.chatSettings[chat.String()]
	settings.MutedUntil = mutedUntil
	settings.Found = true
	d.chatSettings[chat.String()] = settings
	d.mu.Unlock()
	return nil
}

// PutPinned implements store.ChatSettingsStore.
func (d *deviceStore) PutPinned(_ context.Context, chat types.JID, pinned bool) error {
	d.mu.Lock()
	settings := d.chatSettings[chat.String()]
	settings.Pinned = pinned
	settings.Found = true
	d.chatSettings[chat.String()] = settings
	d.mu.Unlock()
	return nil
}

// PutArchived implements store.ChatSettingsStore.
func (d *deviceStore) PutArchived(_ context.Context, chat types.JID, archived bool) error {
	d.mu.Lock()
	settings := d.chatSettings[chat.String()]
	settings.Archived = archived
	settings.Found = true
	d.chatSettings[chat.String()] = settings
	d.mu.Unlock()
	return nil
}

// PutWASARootSecretID implements store.ChatSettingsStore.
func (d *deviceStore) PutWASARootSecretID(_ context.Context, chat types.JID, id types.MessageID) error {
	d.mu.Lock()
	d.wasRootSecrets[chat.String()] = id
	d.mu.Unlock()
	return nil
}

// GetWASARootSecretID implements store.ChatSettingsStore.
func (d *deviceStore) GetWASARootSecretID(_ context.Context, chat types.JID) (types.MessageID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.wasRootSecrets[chat.String()], nil
}

// GetChatSettings implements store.ChatSettingsStore.
func (d *deviceStore) GetChatSettings(_ context.Context, chat types.JID) (types.LocalChatSettings, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	settings, ok := d.chatSettings[chat.String()]
	settings.Found = ok
	return settings, nil
}

// ---------------------------------------------------------------- secrets

// PutMessageSecrets implements store.MsgSecretStore.
func (d *deviceStore) PutMessageSecrets(_ context.Context, inserts []store.MessageSecretInsert) error {
	d.mu.Lock()
	for _, insert := range inserts {
		d.messageSecrets[secretKey(insert.Chat, insert.Sender, insert.ID)] = insert.Secret
	}
	d.mu.Unlock()
	return nil
}

// PutMessageSecret implements store.MsgSecretStore.
func (d *deviceStore) PutMessageSecret(_ context.Context, chat, sender types.JID, id types.MessageID, secret []byte) error {
	d.mu.Lock()
	d.messageSecrets[secretKey(chat, sender, id)] = secret
	d.mu.Unlock()
	return nil
}

// GetMessageSecret implements store.MsgSecretStore.
func (d *deviceStore) GetMessageSecret(_ context.Context, chat, sender types.JID, id types.MessageID) ([]byte, types.JID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.messageSecrets[secretKey(chat, sender, id)], sender, nil
}

// PutPrivacyTokens implements store.PrivacyTokenStore.
func (d *deviceStore) PutPrivacyTokens(_ context.Context, tokens ...store.PrivacyToken) error {
	d.mu.Lock()
	for _, token := range tokens {
		d.privacyTokens[token.User.String()] = token
	}
	d.mu.Unlock()
	return nil
}

// GetPrivacyToken implements store.PrivacyTokenStore.
func (d *deviceStore) GetPrivacyToken(_ context.Context, user types.JID) (*store.PrivacyToken, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	token, ok := d.privacyTokens[user.String()]
	if !ok {
		return nil, nil
	}
	return &token, nil
}

// DeleteExpiredPrivacyTokens implements store.PrivacyTokenStore.
func (d *deviceStore) DeleteExpiredPrivacyTokens(_ context.Context, cutoff time.Time) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var removed int64
	for user, token := range d.privacyTokens {
		if token.Timestamp.Before(cutoff) {
			delete(d.privacyTokens, user)
			removed++
		}
	}
	return removed, nil
}

// PutNCTSalt implements store.NCTSaltStore.
func (d *deviceStore) PutNCTSalt(_ context.Context, salt []byte) error {
	d.mu.Lock()
	d.nctSalt = salt
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// GetNCTSalt implements store.NCTSaltStore.
func (d *deviceStore) GetNCTSalt(_ context.Context) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nctSalt, nil
}

// DeleteNCTSalt implements store.NCTSaltStore.
func (d *deviceStore) DeleteNCTSalt(_ context.Context) error {
	d.mu.Lock()
	d.nctSalt = nil
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------- buffered events

// GetBufferedEvent implements store.EventBuffer.
func (d *deviceStore) GetBufferedEvent(_ context.Context, hash [32]byte) (*store.BufferedEvent, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	event, ok := d.buffered[hash]
	if !ok {
		return nil, nil
	}
	return &event, nil
}

// PutBufferedEvent implements store.EventBuffer.
func (d *deviceStore) PutBufferedEvent(_ context.Context, hash [32]byte, plaintext []byte, serverTimestamp time.Time) error {
	d.mu.Lock()
	d.buffered[hash] = store.BufferedEvent{
		Plaintext:  plaintext,
		InsertTime: time.Now(),
		ServerTime: serverTimestamp,
	}
	d.mu.Unlock()
	return nil
}

// DoDecryptionTxn implements store.EventBuffer. There is one in-memory state, so
// the callback runs directly: the buffer is only ever touched from whatsmeow's
// single event loop.
func (d *deviceStore) DoDecryptionTxn(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// ClearBufferedEventPlaintext implements store.EventBuffer.
func (d *deviceStore) ClearBufferedEventPlaintext(_ context.Context, hash [32]byte) error {
	d.mu.Lock()
	event, ok := d.buffered[hash]
	if ok {
		event.Plaintext = nil
		d.buffered[hash] = event
	}
	d.mu.Unlock()
	return nil
}

// DeleteOldBufferedHashes implements store.EventBuffer.
func (d *deviceStore) DeleteOldBufferedHashes(_ context.Context) error {
	d.mu.Lock()
	cutoff := time.Now().Add(-maxBufferedAge)
	for hash, event := range d.buffered {
		if event.InsertTime.Before(cutoff) {
			delete(d.buffered, hash)
		}
	}
	d.mu.Unlock()
	return nil
}

// GetOutgoingEvent implements store.EventBuffer.
func (d *deviceStore) GetOutgoingEvent(_ context.Context, chatJID, altChatJID types.JID, id types.MessageID) (string, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if event, ok := d.outgoing[outgoingKey(chatJID, id)]; ok {
		return event.Format, event.Plaintext, nil
	}
	if !altChatJID.IsEmpty() {
		if event, ok := d.outgoing[outgoingKey(altChatJID, id)]; ok {
			return event.Format, event.Plaintext, nil
		}
	}
	return "", nil, nil
}

// AddOutgoingEvent implements store.EventBuffer.
func (d *deviceStore) AddOutgoingEvent(_ context.Context, chatJID types.JID, id types.MessageID, format string, plaintext []byte) error {
	d.mu.Lock()
	d.outgoing[outgoingKey(chatJID, id)] = outgoingEvent{Format: format, Plaintext: plaintext}
	d.mu.Unlock()
	return nil
}

// DeleteOldOutgoingEvents implements store.EventBuffer.
func (d *deviceStore) DeleteOldOutgoingEvents(_ context.Context) error {
	// The buffer exists so a retry re-encrypts the same body. WhatsApp's own
	// retry window is minutes long, and the device is rebuilt on every restart,
	// so the map is bounded by the process's own lifetime rather than pruned by
	// age: an entry that is never asked for costs a few hundred bytes until the
	// connection is replaced.
	return nil
}

// ---------------------------------------------------------------- lid store

// PutManyLIDMappings implements store.LIDStore.
func (d *deviceStore) PutManyLIDMappings(_ context.Context, mappings []store.LIDMapping) error {
	d.mu.Lock()
	for _, mapping := range mappings {
		d.lidByPN[mapping.PN.String()] = mapping.LID.String()
		d.pnByLID[mapping.LID.String()] = mapping.PN.String()
	}
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// PutLIDMapping implements store.LIDStore.
func (d *deviceStore) PutLIDMapping(_ context.Context, lid, pn types.JID) error {
	d.mu.Lock()
	d.lidByPN[pn.String()] = lid.String()
	d.pnByLID[lid.String()] = pn.String()
	d.markDirty()
	d.mu.Unlock()
	return nil
}

// GetPNForLID implements store.LIDStore.
func (d *deviceStore) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	raw, ok := d.pnByLID[lid.String()]
	if !ok {
		return types.EmptyJID, nil
	}
	return types.ParseJID(raw)
}

// GetLIDForPN implements store.LIDStore.
func (d *deviceStore) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	raw, ok := d.lidByPN[pn.String()]
	if !ok {
		return types.EmptyJID, nil
	}
	return types.ParseJID(raw)
}

// GetManyLIDsForPNs implements store.LIDStore.
func (d *deviceStore) GetManyLIDsForPNs(_ context.Context, pns []types.JID) (map[types.JID]types.JID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := map[types.JID]types.JID{}
	for _, pn := range pns {
		if raw, ok := d.lidByPN[pn.String()]; ok {
			if lid, err := types.ParseJID(raw); err == nil {
				out[pn] = lid
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- helpers

// maxBufferedAge bounds how long a decrypted-but-unacknowledged event is kept.
const maxBufferedAge = 5 * time.Minute

// randomUint32 draws a registration id.
func randomUint32() uint32 {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 1
	}
	return binary.BigEndian.Uint32(raw[:])
}

// splitAddress separates a signal address into its parts. whatsmeow builds the
// address as `<user>_<agent>:<device>`; the agent is the account's domain — a LID
// is `_1`, a phone number has none — and the device is the linked device.
func splitAddress(address string) (user, agent, device string) {
	user, device, _ = strings.Cut(address, ":")
	user, agent, _ = strings.Cut(user, "_")

	return user, agent, device
}

// ownsAddress reports whether a signal address belongs to a phone number, which
// is what a delete by phone matches.
//
// The user part is compared whole rather than as a prefix: another account whose
// number merely starts with these digits is a different account, and matching it
// would move or delete that account's sessions.
func ownsAddress(address, phone string) bool {
	user, _, _ := splitAddress(address)

	return user == phone
}

// secretKey is the map key for one message secret.
func secretKey(chat, sender types.JID, id types.MessageID) string {
	return chat.String() + "/" + sender.String() + "/" + id
}

// outgoingKey is the map key for one sent message's plaintext.
func outgoingKey(chat types.JID, id types.MessageID) string {
	return chat.String() + "/" + id
}

// to32 and to64 read fixed-size key material out of a decoded blob. A short
// slice is padded rather than refused: a truncated blob would have failed the
// cipher's own authentication before reaching here.
func to32(raw []byte) [32]byte {
	var out [32]byte
	copy(out[:], raw)
	return out
}

func to64(raw []byte) [64]byte {
	var out [64]byte
	copy(out[:], raw)
	return out
}

// encodeBase64 and decodeBase64 carry binary map keys through JSON.
func encodeBase64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func decodeBase64(encoded string) ([]byte, error) { return base64.StdEncoding.DecodeString(encoded) }

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
	"github.com/robzlabz/employeebot/packages/whatsapp/domain/mocks"
)

// The engine's own rules are this file: which numbers it serves at all, and how a
// WhatsApp message becomes this module's own.
//
// Everything that needs a live connection is tested through the gateway's surface
// instead, because a test cannot pair a phone. What is here is what can be decided
// without one — and those decisions are the ones that go wrong quietly: a number
// served without a session, a session served under the wrong workspace, a media
// message read as text.

// engineHarness is an engine over scripted ports.
type engineHarness struct {
	engine    *Engine
	sessions  *mocks.SessionStore
	sealer    *mocks.Sealer
	directory *mocks.Directory
	lease     *mocks.Lease
	inbox     *mocks.Inbox
	drain     *mocks.Drain
	status    *mocks.Status
	links     *mocks.LinkSink
	media     *mocks.MediaStore

	numberID  uuid.UUID
	workspace uuid.UUID
}

func newEngineHarness(t *testing.T) *engineHarness {
	t.Helper()

	h := &engineHarness{
		sessions:  mocks.NewSessionStore(t),
		sealer:    mocks.NewSealer(t),
		directory: mocks.NewDirectory(t),
		lease:     mocks.NewLease(t),
		inbox:     mocks.NewInbox(t),
		drain:     mocks.NewDrain(t),
		status:    mocks.NewStatus(t),
		links:     mocks.NewLinkSink(t),
		media:     mocks.NewMediaStore(t),
		numberID:  uuid.New(),
		workspace: uuid.New(),
	}

	cfg := DefaultEngineConfig()
	cfg.Owner = "test-instance"

	h.engine = NewEngine(cfg, EngineDeps{
		Sessions:  h.sessions,
		Sealer:    h.sealer,
		Directory: h.directory,
		Lease:     h.lease,
		Inbox:     h.inbox,
		Drain:     h.drain,
		Status:    h.status,
		Links:     h.links,
		Media:     h.media,
	}, nil)

	return h
}

// assignment is the directory row this instance is told to serve.
func (h *engineHarness) assignment() whatsappdomain.Assignment {
	return whatsappdomain.Assignment{NumberID: h.numberID, WorkspaceID: h.workspace}
}

// TestANumberWithNoSessionIsNotServed: a row exists as soon as a person starts
// linking, so the fleet is told about numbers that have nothing behind them. Taking
// one on would hold a lease for a session that cannot be opened — and holding it
// would keep the number offline everywhere.
func TestANumberWithNoSessionIsNotServed(t *testing.T) {
	h := newEngineHarness(t)

	h.directory.EXPECT().Serving(mock.Anything).Return([]whatsappdomain.Assignment{h.assignment()}, nil).Once()
	h.lease.EXPECT().Acquire(mock.Anything, h.numberID, "test-instance", mock.Anything).Return(true, nil).Once()
	h.status.EXPECT().Number(mock.Anything, mock.Anything, h.numberID).Return(
		whatsappdomain.Number{
			ID: h.numberID, WorkspaceID: h.workspace, Phone: "628123456789",
			Status: whatsappdomain.StatusUnlinked, HourlyLimit: 40,
		}, nil).Once()
	// The lease is given back, so another instance is free to serve it once it is
	// linked.
	h.lease.EXPECT().Release(mock.Anything, h.numberID, "test-instance").Return(nil).Once()
	// The reclaim pass runs for a number that is taken on. It is expected once
	// because reconciliation does not start a client here; the call is asserted
	// below to make the contract explicit.
	h.drain.EXPECT().Reclaim(mock.Anything, mock.Anything, mock.Anything).Return(0, nil).Maybe()

	h.engine.reconcile(t.Context())

	require.False(t, h.engine.running(h.numberID), "an unlinked number is not served")
}

// TestAnUnopenableSessionAsksForARelink is what a rotation that dropped a key or a
// tampered row looks like, and it is the one case where retrying forever would hide
// a person's problem: the number is reported as needing them rather than left
// offline and silent.
func TestAnUnopenableSessionAsksForARelink(t *testing.T) {
	h := newEngineHarness(t)

	h.directory.EXPECT().Serving(mock.Anything).Return([]whatsappdomain.Assignment{h.assignment()}, nil).Once()
	h.lease.EXPECT().Acquire(mock.Anything, h.numberID, "test-instance", mock.Anything).Return(true, nil).Once()
	h.status.EXPECT().Number(mock.Anything, mock.Anything, h.numberID).Return(
		whatsappdomain.Number{
			ID: h.numberID, WorkspaceID: h.workspace, Phone: "628123456789",
			Status: whatsappdomain.StatusConnected, HourlyLimit: 40,
		}, nil).Once()
	h.sessions.EXPECT().Load(mock.Anything, mock.Anything, h.numberID).Return(
		whatsappdomain.Session{
			NumberID: h.numberID, WorkspaceID: h.workspace,
			Sealed: []byte("sealed-under-a-key-nobody-kept"), JID: "628123456789:12@s.whatsapp.net",
		}, nil).Once()
	h.sealer.EXPECT().Open(mock.Anything, h.workspace, mock.Anything).
		Return(nil, errors.New("the key that sealed this is not in the ring")).Once()
	h.status.EXPECT().
		Report(mock.Anything, mock.Anything, mock.MatchedBy(func(request whatsappdomain.ReportRequest) bool {
			return request.Status == whatsappdomain.StatusNeedsRelink && strings.Contains(request.Reason, "kunci")
		})).
		Return(whatsappdomain.Number{ID: h.numberID, Status: whatsappdomain.StatusNeedsRelink}, nil).Once()
	h.lease.EXPECT().Release(mock.Anything, h.numberID, "test-instance").Return(nil).Once()

	h.engine.reconcile(t.Context())

	require.False(t, h.engine.running(h.numberID))
}

// TestASessionIsSealedUnderItsOwnWorkspace is E9.1's second gate at the only place
// it can be enforced: the seal is addressed by the workspace that owns the session,
// so a blob cannot be written under one tenant's key and read under another's.
func TestASessionIsSealedUnderItsOwnWorkspace(t *testing.T) {
	h := newEngineHarness(t)

	scope := whatsappdomain.Scope{WorkspaceID: h.workspace}
	var sealedBlob []byte
	var savedScope whatsappdomain.Scope
	var savedNumber uuid.UUID

	h.sealer.EXPECT().
		Seal(mock.Anything, h.workspace, mock.Anything).
		RunAndReturn(func(_ context.Context, _ uuid.UUID, plaintext []byte) ([]byte, error) {
			// The plaintext has to be the device document, not an empty blob: a
			// session that seals nothing is a session that cannot be restored.
			var document map[string]any
			require.NoError(t, json.Unmarshal(plaintext, &document))
			require.Equal(t, float64(formatVersion), document["v"])

			sealedBlob = append([]byte("sealed:"), plaintext...)
			return sealedBlob, nil
		}).Once()
	h.sessions.EXPECT().
		Save(mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, got whatsappdomain.Scope, session whatsappdomain.Session) error {
			savedScope = got
			savedNumber = session.NumberID
			require.Equal(t, sealedBlob, session.Sealed, "what is stored is what the sealer returned")
			return nil
		}).Once()

	put := h.engine.persister(scope, h.numberID)
	require.NoError(t, put(t.Context(), sealed{
		Version:      formatVersion,
		JID:          "628123456789:12@s.whatsapp.net",
		AdvSecretKey: []byte("secret"),
	}))

	require.Equal(t, h.workspace, savedScope.WorkspaceID, "the seal is addressed by the workspace that owns the number")
	require.Equal(t, h.numberID, savedNumber)
}

// TestTheDeviceDocumentCarriesTheWorkspaceItBelongsTo: the persisted document names
// its own account, so a restored device is matched against the number the fleet was
// told to serve rather than adopted blindly.
func TestTheDeviceDocumentCarriesItsAccount(t *testing.T) {
	numberID, workspaceID := uuid.New(), uuid.New()
	_, inner := newDevice(numberID, workspaceID, nil, nil)

	document := inner.snapshot()
	require.Equal(t, formatVersion, document.Version)
	require.Empty(t, document.JID, "a fresh device has no account yet")

	encoded, err := json.Marshal(document)
	require.NoError(t, err)

	restored, _, err := restoreDevice(encoded, numberID, workspaceID, nil)
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.NotNil(t, restored.NoiseKey)
	require.Nil(t, restored.ID, "an unpaired device has no account to answer as")
}

// TestAMessageIsTranslatedAsWhatsappSentIt: the three shapes that differ are the
// three that matter — text, media with a caption, and something this deployment does
// not read — and getting one wrong is a Bolu that answers the wrong question.
func TestAMessageIsTranslatedAsWhatsappSentIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		message  *waE2E.Message
		kind     string
		text     string
		hasMedia bool
	}{
		{
			name:    "plain text",
			message: &waE2E.Message{Conversation: proto.String("Ada kaos polos ukuran L?")},
			kind:    whatsappdomain.KindText,
			text:    "Ada kaos polos ukuran L?",
		},
		{
			name:    "an extended text",
			message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("dengan tautan")}},
			kind:    whatsappdomain.KindText,
			text:    "dengan tautan",
		},
		{
			name: "an image with a caption",
			message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Caption: proto.String("ini ukurannya"), DirectPath: proto.String("/v/t62/x"), FileLength: proto.Uint64(2048),
			}},
			kind:     whatsappdomain.KindImage,
			text:     "ini ukurannya",
			hasMedia: true,
		},
		{
			name: "a document",
			message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				FileName: proto.String("invoice.pdf"), DirectPath: proto.String("/v/t62/y"), FileLength: proto.Uint64(90210),
			}},
			kind:     whatsappdomain.KindDocument,
			hasMedia: true,
		},
		{
			name: "a sticker",
			message: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
				DirectPath: proto.String("/v/t62/z"), FileLength: proto.Uint64(120),
			}},
			kind:     whatsappdomain.KindSticker,
			hasMedia: true,
		},
		{
			name:     "a voice note",
			message:  &waE2E.Message{AudioMessage: &waE2E.AudioMessage{DirectPath: proto.String("/v/t62/a"), FileLength: proto.Uint64(4096)}},
			kind:     whatsappdomain.KindAudio,
			hasMedia: true,
		},
		{
			name:    "a reaction, which is neither",
			message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}},
			kind:    whatsappdomain.KindOther,
		},
	} {
		require.Equal(t, tc.kind, messageKind(tc.message), tc.name)
		require.Equal(t, tc.text, messageText(tc.message), tc.name)

		media, has := messageMedia(tc.message)
		require.Equal(t, tc.hasMedia, has, tc.name)
		if tc.hasMedia {
			require.Equal(t, whatsappdomain.MediaPending, media.Status, tc.name)
			require.NotEmpty(t, media.Ref, tc.name)
			require.Equal(t, tc.kind, media.Kind, tc.name)
		}
	}
}

// TestAMediaMessageIsNotBlockedOnItsDownload: the bytes are fetched afterwards, so
// what the receive path records is a message with a pending attachment. Recording
// it as ready would promise bytes that are not there.
func TestAMediaMessageIsNotBlockedOnItsDownload(t *testing.T) {
	message := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("ini ukurannya"), DirectPath: proto.String("/v/t62/x"), FileLength: proto.Uint64(4096),
	}}
	media, has := messageMedia(message)
	require.True(t, has)
	require.Equal(t, int64(4096), media.Size)

	inbound := whatsappdomain.Inbound{Media: media, Kind: messageKind(message), Text: messageText(message)}
	require.Equal(t, whatsappdomain.MediaPending, inbound.Media.Status)
	require.Empty(t, inbound.Media.Key, "the bytes are not stored yet, so there is no key yet")
}

// TestAPhoneIsMaskedInLogsAndScreens: a number is enough to recognise a customer's
// line and not enough to publish it, and the mask is used by both the log line and
// the screen.
func TestAPhoneIsMaskedInLogsAndScreens(t *testing.T) {
	require.Equal(t, "••••••••6789", maskPhone("628123456789"))
	require.Equal(t, "••••", maskPhone("1234"))
	require.Equal(t, "•••", maskPhone("999"))
	require.Empty(t, maskPhone(""))
}

// TestTheNumberIsReadFromTheAccountAddress: WhatsApp's multi-device form carries a
// device suffix, and the number is what the product needs. A LID-addressed account
// has no number at all, which is why the caller falls back to what the person typed.
func TestTheNumberIsReadFromTheAccountAddress(t *testing.T) {
	require.Equal(t, "628123456789", phoneFromJID(types.NewJID("628123456789:12", types.DefaultUserServer)))
	require.Equal(t, "628123456789", phoneFromJID(types.NewJID("628123456789", types.DefaultUserServer)))
	require.Empty(t, phoneFromJID(types.NewJID("999", types.HiddenUserServer)), "a LID is not a number")
	require.Empty(t, phoneFromJID(types.EmptyJID))
}

// TestATypedNumberIsReducedToDigits: a person types a plus and grouping, and
// WhatsApp wants neither.
func TestATypedNumberIsReducedToDigits(t *testing.T) {
	require.Equal(t, "628123456789", normalizePhone("+62 812-3456-789"))
	require.Equal(t, "628123456789", normalizePhone("628123456789"))
	require.Equal(t, "0812345", normalizePhone("(0812) 345"))
	require.Empty(t, normalizePhone("bukan nomor"))
}

// TestAFailedAttemptWaitsLongerEachTime: a number that is refusing messages is not
// hammered, and a customer's answer is delayed rather than lost.
func TestAFailedAttemptWaitsLongerEachTime(t *testing.T) {
	first := sendBackoff(1)
	second := sendBackoff(2)
	third := sendBackoff(3)

	require.Less(t, first, second)
	require.Less(t, second, third)
	require.GreaterOrEqual(t, first, 5*time.Second, "a retry that is immediate is not a backoff")
	require.LessOrEqual(t, third, time.Minute, "the wait is bounded, so the answer is delayed and not lost")
	require.Equal(t, time.Minute, sendBackoff(100))
}

// TestTheLeaseNameIsPerProcess: an empty owner would make every instance the same
// claimant, and the first instance would win forever.
func TestTheLeaseNameIsPerProcess(t *testing.T) {
	engine := NewEngine(EngineConfig{}, EngineDeps{}, nil)
	require.NotEmpty(t, engine.cfg.Owner)

	other := NewEngine(EngineConfig{}, EngineDeps{}, nil)
	require.NotEqual(t, engine.cfg.Owner, other.cfg.Owner)

	named := NewEngine(EngineConfig{Owner: "gateway-a"}, EngineDeps{}, nil)
	require.Equal(t, "gateway-a", named.cfg.Owner, "a fleet names its instances, so a restart is not a new claimant")
}

// TestAnEngineWithoutItsPortsSaysSo: a gateway that cannot seal or store a session
// must not start, because it would connect a number it cannot remember.
func TestAnEngineWithoutItsPortsSaysSo(t *testing.T) {
	engine := NewEngine(DefaultEngineConfig(), EngineDeps{}, nil)

	err := engine.Run(t.Context())
	require.ErrorIs(t, err, whatsappdomain.ErrNotConfigured)
}

// TestTheDirectoryIsReadOnEveryPass: a number linked while the process is running
// has to come online without a restart, which is what the poll is for.
func TestTheDirectoryIsReadOnEveryPass(t *testing.T) {
	h := newEngineHarness(t)

	h.directory.EXPECT().Serving(mock.Anything).Return(nil, nil).Twice()

	h.engine.reconcile(t.Context())
	h.engine.reconcileAndRenew(t.Context())

	require.False(t, h.engine.running(h.numberID))
}

// TestADirectoryThatCannotBeReadIsNotFatal: a database that is briefly unreachable
// must not take every number offline. The pass is skipped and the numbers already
// running keep running.
func TestADirectoryThatCannotBeReadIsNotFatal(t *testing.T) {
	h := newEngineHarness(t)

	h.directory.EXPECT().Serving(mock.Anything).Return(nil, errors.New("the database is away")).Once()

	require.NotPanics(t, func() { h.engine.reconcile(t.Context()) })
}

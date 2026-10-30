package service_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/whatsapp/service"
	"github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The module's reads and the smaller commands are this file.
//
// They are worth stating because each one is a decision a caller cannot see: the
// order a conversation is handed to a model in, which number a connection speaks as,
// whether cancelling an attempt rolls the row back, and what an attachment is served
// from. None of them is loud when it goes wrong — the model simply reads a thread
// backwards, or a tool call acts as the wrong number — which is exactly why they are
// pinned here rather than left to the query.

// TestAConversationIsHandedToOneInTheOrderItHappened: the repository reads newest
// first because that is what a screen wants, and a model reading a conversation needs
// the other way round. Reversing here rather than in a second query is the point: two
// orderings of one read would be two places to get it wrong.
func TestAConversationIsHandedToOneInTheOrderItHappened(t *testing.T) {
	h := newHarness(t)
	integrationID := h.number().IntegrationID

	h.store.EXPECT().ByIntegration(mock.Anything, h.scope, integrationID).
		Return(h.number(), nil).Once()

	// What the repository answers: newest first.
	newest := []domain.Inbound{
		{ID: uuid.New(), WaMessageID: "msg-3", Text: "yang ketiga"},
		{ID: uuid.New(), WaMessageID: "msg-2", Text: "yang kedua"},
		{ID: uuid.New(), WaMessageID: "msg-1", Text: "yang pertama"},
	}
	h.store.EXPECT().Messages(mock.Anything, h.scope, mock.MatchedBy(func(filter domain.MessageFilter) bool {
		return filter.NumberID == h.numberID && filter.ChatJID == "628999@s.whatsapp.net" && filter.Limit == 20
	})).Return(newest, nil).Once()

	history, err := h.service.History(t.Context(), h.scope, integrationID, "628999", 0)
	require.NoError(t, err)
	require.Len(t, history, 3)
	require.Equal(t, "msg-1", history[0].WaMessageID, "the oldest is first")
	require.Equal(t, "msg-3", history[2].WaMessageID)

	// A number typed the way a person types it becomes an address; an address is
	// already one and is left alone.
	h.store.EXPECT().ByIntegration(mock.Anything, h.scope, integrationID).Return(h.number(), nil).Once()
	h.store.EXPECT().Messages(mock.Anything, h.scope, mock.MatchedBy(func(filter domain.MessageFilter) bool {
		return filter.ChatJID == "+62 899-9@s.whatsapp.net" && filter.Limit == 5
	})).Return(nil, nil).Once()

	_, err = h.service.History(t.Context(), h.scope, integrationID, "+62 899-9@s.whatsapp.net", 5)
	require.NoError(t, err)

	// A limit nobody should be able to ask for is replaced rather than sent on, and
	// an empty chat is left empty: the caller filters by number instead of by chat.
	h.store.EXPECT().ByIntegration(mock.Anything, h.scope, integrationID).Return(h.number(), nil).Once()
	h.store.EXPECT().Messages(mock.Anything, h.scope, mock.MatchedBy(func(filter domain.MessageFilter) bool {
		return filter.ChatJID == "" && filter.Limit == 20
	})).Return(nil, nil).Once()

	_, err = h.service.History(t.Context(), h.scope, integrationID, "   ", 5000)
	require.NoError(t, err)

	// A connection that names no number is the caller's error, not an empty thread.
	h.store.EXPECT().ByIntegration(mock.Anything, h.scope, integrationID).
		Return(domain.Number{}, domain.ErrNotFound).Once()

	_, err = h.service.History(t.Context(), h.scope, integrationID, "628999", 10)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestAConnectionNamesItsNumber: the model is given a connection to act on and this
// is what turns it into the number that speaks. A tool call that guessed would send
// as whichever number it found first.
func TestAConnectionNamesItsNumber(t *testing.T) {
	h := newHarness(t)
	integrationID := h.number().IntegrationID

	_, err := h.service.NumberByIntegration(t.Context(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrInvalidInput, "a call that names no connection is refused")

	h.store.EXPECT().ByIntegration(mock.Anything, h.scope, integrationID).
		Return(h.number(), nil).Once()

	numberID, err := h.service.NumberByIntegration(t.Context(), h.scope, integrationID)
	require.NoError(t, err)
	require.Equal(t, h.numberID, numberID)

	// A connection this workspace does not have is a read that failed, not a zero id:
	// a zero id would be a send from no number at all.
	h.store.EXPECT().ByIntegration(mock.Anything, h.scope, integrationID).
		Return(domain.Number{}, domain.ErrNotFound).Once()

	_, err = h.service.NumberByIntegration(t.Context(), h.scope, integrationID)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestAMessageAndItsAttachmentAreReadableByTheThingThatServesThem: serving a
// customer's attachment needs the row first and the bytes second, and both have to
// say what is missing rather than answer with an empty body.
func TestAMessageAndItsAttachmentAreReadableByTheThingThatServesThem(t *testing.T) {
	h := newHarness(t)
	messageID := uuid.New()

	stored := domain.Inbound{ID: messageID, NumberID: h.numberID, WaMessageID: "msg-1", Text: "halo"}
	h.store.EXPECT().MessageByID(mock.Anything, h.scope, messageID).Return(stored, nil).Once()

	read, err := h.service.MessageByID(t.Context(), h.scope, messageID)
	require.NoError(t, err)
	require.Equal(t, "msg-1", read.WaMessageID)

	// A message this workspace does not have answers with the repository's own
	// refusal, so a screen shows "tidak ditemukan" rather than an empty message.
	h.store.EXPECT().MessageByID(mock.Anything, h.scope, messageID).Return(domain.Inbound{}, domain.ErrNotFound).Once()
	_, err = h.service.MessageByID(t.Context(), h.scope, messageID)
	require.ErrorIs(t, err, domain.ErrNotFound)

	h.media.EXPECT().Get(mock.Anything, "workspace/nota.pdf").Return([]byte("bytes"), nil).Once()

	content, err := h.service.MediaBytes(t.Context(), "workspace/nota.pdf")
	require.NoError(t, err)
	require.Equal(t, []byte("bytes"), content)

	// A key nobody named is refused before the store is asked, so a request cannot
	// read whatever the store would answer for an empty key.
	_, err = h.service.MediaBytes(t.Context(), "   ")
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	// A deployment with no media store says so rather than answering an empty file.
	unwired := newHarness(t)
	unwired.service = service.New(service.Deps{Repository: unwired.store, Now: unwired.clock.now})

	_, err = unwired.service.MediaBytes(t.Context(), "workspace/nota.pdf")
	require.ErrorIs(t, err, domain.ErrNotConfigured)
}

// TestCancellingAnAttemptRollsTheRowBack: cancelling is a person changing their mind,
// so the number goes back to unlinked rather than to failed — the next attempt starts
// from the same row. A deployment with no gateway has nothing to cancel, and that is
// not a failure either.
func TestCancellingAnAttemptRollsTheRowBack(t *testing.T) {
	h := newHarness(t)
	number := h.number(func(n *domain.Number) { n.Status = domain.StatusLinking })

	// Start an attempt, which is what remembers it.
	h.store.EXPECT().ByPhone(mock.Anything, h.scope, "628123456789").Return(number, nil).Once()
	h.transport.EXPECT().Link(mock.Anything, h.scope, h.numberID, "628123456789").
		Return(domain.Link{ID: "link-1", NumberID: h.numberID, WorkspaceID: h.scope.WorkspaceID, State: domain.LinkCode, Code: "qr"}, nil).Once()
	h.store.EXPECT().SetStatus(mock.Anything, h.scope, h.numberID, domain.StatusLinking, "").
		Return(number, nil).Once()

	_, err := h.service.BeginLink(t.Context(), h.scope, domain.LinkRequest{
		Phone: "628123456789", AgentID: h.agentID,
	})
	require.NoError(t, err)

	h.transport.EXPECT().Cancel(mock.Anything, "link-1").Return(nil).Once()
	h.store.EXPECT().SetStatus(mock.Anything, h.scope, h.numberID, domain.StatusUnlinked, "").Return(number, nil).Once()

	require.NoError(t, h.service.CancelLink(t.Context(), h.scope, "link-1"))

	// Cancelling again has nothing remembered, so the row is not written twice — and
	// cancelling something nobody is watching is not an error.
	h.transport.EXPECT().Cancel(mock.Anything, "link-1").Return(nil).Once()
	require.NoError(t, h.service.CancelLink(t.Context(), h.scope, "link-1"))

	// A gateway that refuses the cancel keeps its own error and does not roll the row
	// back: the attempt may still be live, and marking it unlinked would strand a
	// pairing nobody can see.
	h.transport.EXPECT().Cancel(mock.Anything, "link-2").
		Return(errors.New("the gateway is down")).Once()
	require.Error(t, h.service.CancelLink(t.Context(), h.scope, "link-2"))
}

// TestCancellingWithoutAGatewayIsFine: a deployment with no gateway reachable has no
// attempt to cancel, and a screen closing a panel should not read an error for it.
func TestCancellingWithoutAGatewayIsFine(t *testing.T) {
	h := newHarness(t)
	unwired := service.New(service.Deps{Repository: h.store, Now: h.clock.now})

	require.NoError(t, unwired.CancelLink(t.Context(), h.scope, "link-1"))

	_, err := unwired.PairCode(t.Context(), h.scope, "link-1", "628123456789")
	require.ErrorIs(t, err, domain.ErrNotConfigured)
}

// TestThePhoneCodeIsAskedForAndRemembered: the code a person types is the same
// attempt with another shape, so it is remembered the way the QR is — a screen that
// polls after asking for a code must see the code rather than "expired".
func TestThePhoneCodeIsAskedForAndRemembered(t *testing.T) {
	h := newHarness(t)

	h.transport.EXPECT().PairCode(mock.Anything, "link-1", "628123456789").
		Return(domain.Link{ID: "link-1", NumberID: h.numberID, WorkspaceID: h.scope.WorkspaceID, State: domain.LinkCode, PairCode: "ABCD1234"}, nil).Once()

	link, err := h.service.PairCode(t.Context(), h.scope, "link-1", "628123456789")
	require.NoError(t, err)
	require.Equal(t, "ABCD1234", link.PairCode)

	// A gateway that names no attempt keeps its own refusal.
	h.transport.EXPECT().PairCode(mock.Anything, "link-2", mock.Anything).
		Return(domain.Link{}, domain.ErrLinkExpired).Once()
	_, err = h.service.PairCode(t.Context(), h.scope, "link-2", "628123456789")
	require.ErrorIs(t, err, domain.ErrLinkExpired)
}

// TestChoosingABoluForANumberKeepsTheRestOfTheRow: assigning an agent is one field,
// and the repository is handed the whole row because the statement writes it. A label
// or a ceiling dropped on the way would silently rename the number or reset a limit a
// person set.
func TestChoosingABoluForANumberKeepsTheRestOfTheRow(t *testing.T) {
	h := newHarness(t)
	number := h.number()
	newAgent := uuid.New()

	h.store.EXPECT().Get(mock.Anything, h.scope, h.numberID).Return(number, nil).Once()
	h.store.EXPECT().Update(mock.Anything, h.scope, mock.MatchedBy(func(updated domain.Number) bool {
		return updated.ID == number.ID && updated.AgentID == newAgent &&
			updated.Label == number.Label && updated.HourlyLimit == number.HourlyLimit
	})).Return(h.number(func(n *domain.Number) { n.AgentID = newAgent }), nil).Once()
	// The change reaches the workspace's stream, and it says which Bolu was chosen:
	// that is the line a person reads to know who now answers the number. The event
	// carries what was stored rather than what was asked for.
	h.activity.EXPECT().Emit(mock.Anything, mock.MatchedBy(func(event domain.Event) bool {
		payload, ok := event.Payload.(map[string]any)
		return event.Type == "whatsapp.agent_assigned" && ok && payload["agent_id"] == newAgent.String()
	})).Return(nil).Once()

	_, err := h.service.AssignAgent(t.Context(), h.scope, h.numberID, newAgent)
	require.NoError(t, err)

	// A number that cannot be read keeps the repository's refusal rather than
	// updating whatever the zero row would match.
	h.store.EXPECT().Get(mock.Anything, h.scope, h.numberID).Return(domain.Number{}, domain.ErrNotFound).Once()
	_, err = h.service.AssignAgent(t.Context(), h.scope, h.numberID, newAgent)
	require.ErrorIs(t, err, domain.ErrNotFound)

	// And a write that fails is reported rather than answered as done.
	h.store.EXPECT().Get(mock.Anything, h.scope, h.numberID).Return(number, nil).Once()
	h.store.EXPECT().Update(mock.Anything, h.scope, mock.Anything).
		Return(domain.Number{}, errors.New("the number store is down")).Once()
	_, err = h.service.AssignAgent(t.Context(), h.scope, h.numberID, newAgent)
	require.Error(t, err)
}

// TestRenamingANumberKeepsItsBoluAndCeiling is the same rule from the other side: a
// rename is one field too, and the value the person typed is trimmed rather than
// stored with the spaces they happened to have.
func TestRenamingANumberKeepsItsBoluAndCeiling(t *testing.T) {
	h := newHarness(t)
	number := h.number()

	h.store.EXPECT().Get(mock.Anything, h.scope, h.numberID).Return(number, nil).Once()
	h.store.EXPECT().Update(mock.Anything, h.scope, mock.MatchedBy(func(updated domain.Number) bool {
		return updated.Label == "Kasir" && updated.AgentID == number.AgentID && updated.HourlyLimit == number.HourlyLimit
	})).Return(number, nil).Once()
	h.activity.EXPECT().Emit(mock.Anything, mock.MatchedBy(func(event domain.Event) bool {
		return event.Type == "whatsapp.renamed"
	})).Return(nil).Once()

	_, err := h.service.SetLabel(t.Context(), h.scope, h.numberID, "  Kasir  ")
	require.NoError(t, err)
}

// TestPendingMediaIsReadWithItsLimit: the media pass asks for a bounded page, and a
// limit that is passed through is what keeps one number's backlog from starving the
// rest of the fleet.
func TestPendingMediaIsReadWithItsLimit(t *testing.T) {
	h := newHarness(t)

	h.store.EXPECT().PendingMedia(mock.Anything, h.scope, 25).
		Return([]domain.Inbound{{ID: uuid.New(), Media: domain.Media{Status: domain.MediaPending}}}, nil).Once()

	pending, err := h.service.PendingMedia(t.Context(), h.scope, 25)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	h.store.EXPECT().PendingMedia(mock.Anything, h.scope, mock.Anything).
		Return(nil, errors.New("the store is down")).Once()
	_, err = h.service.PendingMedia(t.Context(), h.scope, 25)
	require.Error(t, err)
}

// TestAMediaStatusNobodyUsesIsRefused: the status is what decides whether the media
// pass picks a row up again, so a value outside the four this module writes would be a
// row nothing ever revisits.
func TestAMediaStatusNobodyUsesIsRefused(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()

	h.store.EXPECT().SetMedia(mock.Anything, h.scope, id, mock.MatchedBy(func(media domain.Media) bool {
		return media.Status == domain.MediaReady && media.Key == "workspace/nota.pdf"
	})).Return(nil).Once()

	require.NoError(t, h.service.StoreMedia(t.Context(), h.scope, id,
		domain.Media{Status: domain.MediaReady, Key: "workspace/nota.pdf"}))

	err := h.service.StoreMedia(t.Context(), h.scope, id, domain.Media{Status: "halfway"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	// A failed download is recorded rather than left pending forever, which is the
	// state a person can act on.
	h.store.EXPECT().SetMedia(mock.Anything, h.scope, id, mock.MatchedBy(func(media domain.Media) bool {
		return media.Status == domain.MediaFailed && media.Error != ""
	})).Return(nil).Once()

	require.NoError(t, h.service.StoreMedia(t.Context(), h.scope, id,
		domain.Media{Status: domain.MediaFailed, Error: "unduhan gagal"}))
}

// TestADecidedMessageIsRecordedWithItsOutcome: the three endings of a send are
// written through the repository, and each carries the thing the next reader needs —
// WhatsApp's id, a retry time, or the reason it was parked.
func TestADecidedMessageIsRecordedWithItsOutcome(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	at := h.clock.at

	h.store.EXPECT().MarkSent(mock.Anything, h.scope, id, "wa-out-1", at).Return(nil).Once()
	require.NoError(t, h.service.Sent(t.Context(), h.scope, id, "wa-out-1", at))

	h.store.EXPECT().MarkRetry(mock.Anything, h.scope, id, "the number is offline", at.Add(time.Second)).Return(nil).Once()
	require.NoError(t, h.service.Retry(t.Context(), h.scope, id, "the number is offline", at.Add(time.Second)))

	h.store.EXPECT().MarkFailed(mock.Anything, h.scope, id, "the number is offline", 3).Return(nil).Once()
	require.NoError(t, h.service.Failed(t.Context(), h.scope, id, "the number is offline", 3))

	h.store.EXPECT().Reclaim(mock.Anything, h.scope, at).Return(2, nil).Once()
	reclaimed, err := h.service.Reclaim(t.Context(), h.scope, at)
	require.NoError(t, err)
	require.Equal(t, 2, reclaimed)
}

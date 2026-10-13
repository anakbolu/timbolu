package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	whatsappdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/domain"
	whatsapprepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/repository"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// This file is EPIC 9's proof against a real Postgres. The claims are the ones a
// mock would only restate:
//
//   - a session belongs to one workspace and is invisible from another (E9.1);
//   - one WhatsApp message is one row however many times it is delivered (E9.4);
//   - one message goes to one sender, so two workers cannot both claim it (E9.5);
//   - the hourly ceiling is counted from what actually left (E9.6);
//   - the fleet directory is the one read that is not scoped, and it carries the
//     routing pair and nothing else.

// whatsappFixture is a workspace with a Bolu, a linked number, and the module's
// repository over the real database.
type whatsappFixture struct {
	repository *whatsapprepo.Repository
	pool       *database.Pool
	workspace  uuid.UUID
	userID     uuid.UUID
	agentID    uuid.UUID
	scope      whatsappdomain.Scope
	number     whatsappdomain.Number
}

func newWhatsAppFixture(t *testing.T, dsn string) *whatsappFixture {
	t.Helper()

	pool := poolFor(t, dsn)
	agents := agentrepo.New(pool)
	workspaces := workspacerepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(t.Context(), uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	result, err := workspaces.Onboard(t.Context(), user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	agentScope := agentdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID}
	agentService := agentservice.New(agentservice.Deps{Repository: agents})

	teams, err := agentService.Teams(t.Context(), agentScope)
	require.NoError(t, err)
	require.NotEmpty(t, teams)

	agent, err := agentService.Create(t.Context(), agentScope, agentdomain.CreateRequest{
		TeamID: teams[0].ID,
		Name:   "Oren",
	})
	require.NoError(t, err)

	fixture := &whatsappFixture{
		repository: whatsapprepo.New(pool),
		pool:       pool,
		workspace:  result.Workspace.ID,
		userID:     user.ID,
		agentID:    agent.ID,
		scope:      whatsappdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID},
	}

	// The connection the hub would have created for this number.
	connection := fixture.createConnection(t)

	accepted := time.Now().Add(-time.Hour)
	number, err := fixture.repository.Create(t.Context(), fixture.scope, whatsappdomain.Number{
		IntegrationID: connection,
		Phone:         "628" + uuid.NewString()[:9],
		Label:         "WhatsApp uji",
		AgentID:       agent.ID,
		Status:        whatsappdomain.StatusConnected,
		TOSAcceptedAt: &accepted,
		HourlyLimit:   5,
	})
	require.NoError(t, err)

	fixture.number = number
	return fixture
}

// createConnection inserts the `integrations` row a linked number points at. It is
// written directly because what is under test is the WhatsApp side of the pair.
func (f *whatsappFixture) createConnection(t *testing.T) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	require.NoError(t, f.pool.InScope(t.Context(), database.Scope{
		UserID:      f.userID,
		WorkspaceID: f.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO integrations (workspace_id, app, account_label, status)
			VALUES ($1, 'whatsapp', $2, 'connected')
			RETURNING id`,
			f.workspace, "••••"+uuid.NewString()[:4]).Scan(&id)
	}))

	return id
}

// countInTenant reads a count inside a tenant scope, because every WhatsApp table
// carries Row Level Security: a count outside a scope returns zero whatever the
// table holds, which would make an assertion pass for the wrong reason.
func countInTenant(t *testing.T, fixture *whatsappFixture, query string, args ...any) int {
	t.Helper()

	var count int
	require.NoError(t, fixture.pool.InScopeRead(t.Context(), database.Scope{
		UserID:      fixture.userID,
		WorkspaceID: fixture.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), query, args...).Scan(&count)
	}))

	return count
}

// inbound is one message from a customer, on the fixture's own number. The number
// travels with the message because that is where the receive path puts it before it
// reaches the repository.
func (f *whatsappFixture) inbound(waMessageID string) whatsappdomain.Inbound {
	return whatsappdomain.Inbound{
		NumberID:    f.number.ID,
		WaMessageID: waMessageID,
		ChatJID:     "628999888777@s.whatsapp.net",
		SenderJID:   "628999888777@s.whatsapp.net",
		SenderName:  "Bu Sari",
		Kind:        whatsappdomain.KindText,
		Text:        "Ada kaos polos ukuran L?",
		ReceivedAt:  time.Now(),
	}
}

// TestANumberCanBePreparedBeforeItIsLinked is the regression the browser found: a
// number is recorded *before* anything is linked — that is what the warning is
// accepted against and what the pairing lands on — so its row cannot demand a
// connection the hub has not created yet. The first version of the table did demand
// one, and preparing a number answered "internal server error" while the integration
// test passed, because the test built the connection first.
func TestANumberCanBePreparedBeforeItIsLinked(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	prepared, err := fixture.repository.Create(ctx, fixture.scope, whatsappdomain.Number{
		Phone:       "628" + uuid.NewString()[:9],
		Label:       "WhatsApp baru",
		AgentID:     fixture.agentID,
		Status:      whatsappdomain.StatusUnlinked,
		HourlyLimit: 40,
	})
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, prepared.IntegrationID, "a number that is not linked has no connection yet")
	require.Equal(t, whatsappdomain.StatusUnlinked, prepared.Status)
	require.False(t, prepared.Linked())

	// It is listed and readable while it has no connection, which is what the screen
	// shows between the two steps.
	listed, err := fixture.repository.List(ctx, fixture.scope)
	require.NoError(t, err)
	require.NotEmpty(t, listed)

	// And a call that names no connection cannot match it.
	_, err = fixture.repository.ByIntegration(ctx, fixture.scope, uuid.New())
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)

	// The connection arrives when the phone pairs, and it is written once.
	connection := fixture.createConnection(t)
	require.NoError(t, fixture.repository.SetIntegration(ctx, fixture.scope, prepared.ID, connection))

	linked, err := fixture.repository.ByIntegration(ctx, fixture.scope, connection)
	require.NoError(t, err)
	require.Equal(t, prepared.ID, linked.ID)
	require.Equal(t, connection, linked.IntegrationID)

	// The workspace can also find it by phone, which is the relink path.
	found, err := fixture.repository.ByPhone(ctx, fixture.scope, prepared.Phone)
	require.NoError(t, err)
	require.Equal(t, prepared.ID, found.ID)
}

// TestASessionBelongsToOneWorkspace is E9.1's second gate: the session blob of one
// workspace is not readable from another, and the table is what enforces it rather
// than the query.
func TestASessionBelongsToOneWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	mine := newWhatsAppFixture(t, dsn)
	theirs := newWhatsAppFixture(t, dsn)
	ctx := t.Context()

	sealed := []byte{0x01, 0x02, 0x03, 0x00, 0xff, 0xfe}
	require.NoError(t, mine.repository.Save(ctx, mine.scope, whatsappdomain.Session{
		NumberID:    mine.number.ID,
		WorkspaceID: mine.workspace,
		Sealed:      sealed,
		JID:         "628123456789:12@s.whatsapp.net",
	}))

	// It reads back byte for byte, which is what a device document needs: a
	// truncated blob is a session that cannot be restored.
	loaded, err := mine.repository.Load(ctx, mine.scope, mine.number.ID)
	require.NoError(t, err)
	require.Equal(t, sealed, loaded.Sealed)
	require.Equal(t, "628123456789:12@s.whatsapp.net", loaded.JID)

	// The other workspace cannot name it, because the row is not in its scope.
	_, err = theirs.repository.Load(ctx, theirs.scope, mine.number.ID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)

	// And it cannot write over it either: the policy refuses a row that names
	// somebody else's workspace.
	err = theirs.repository.Save(ctx, theirs.scope, whatsappdomain.Session{
		NumberID:    mine.number.ID,
		WorkspaceID: mine.workspace,
		Sealed:      []byte("someone else's session"),
	})
	require.Error(t, err)

	loaded, err = mine.repository.Load(ctx, mine.scope, mine.number.ID)
	require.NoError(t, err)
	require.Equal(t, sealed, loaded.Sealed, "the session was not replaced from outside its workspace")
}

// TestANumberIsInvisibleToAnotherWorkspace: the number, its messages, its queue, and
// its session are all one tenant's, and the policy is what makes that true rather
// than the filters in the queries.
func TestANumberIsInvisibleToAnotherWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	mine := newWhatsAppFixture(t, dsn)
	theirs := newWhatsAppFixture(t, dsn)
	ctx := t.Context()

	_, _, err := mine.repository.Record(ctx, mine.scope, mine.inbound("msg-isolation"))
	require.NoError(t, err)
	_, err = mine.repository.Enqueue(ctx, mine.scope, whatsappdomain.Outbox{
		NumberID: mine.number.ID, ToJID: "628999888777@s.whatsapp.net",
		Kind: whatsappdomain.KindText, Text: "halo", Status: whatsappdomain.OutboxQueued,
	})
	require.NoError(t, err)

	// The other workspace sees neither, and cannot name mine.
	_, err = theirs.repository.Get(ctx, theirs.scope, mine.number.ID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)

	messages, err := theirs.repository.Messages(ctx, theirs.scope, whatsappdomain.MessageFilter{})
	require.NoError(t, err)
	for _, message := range messages {
		require.NotEqual(t, "msg-isolation", message.WaMessageID)
	}

	// The counts are read inside each scope so the assertion is about the policy
	// rather than about a filter.
	require.Equal(t, 1, countInTenant(t, mine, `SELECT count(*) FROM whatsapp_messages WHERE wa_message_id = $1`, "msg-isolation"))
	require.Equal(t, 0, countInTenant(t, theirs, `SELECT count(*) FROM whatsapp_messages WHERE wa_message_id = $1`, "msg-isolation"))
	require.Equal(t, 1, countInTenant(t, mine, `SELECT count(*) FROM whatsapp_outbox`))
	require.Equal(t, 0, countInTenant(t, theirs, `SELECT count(*) FROM whatsapp_outbox`))
	require.Equal(t, 0, countInTenant(t, theirs, `SELECT count(*) FROM whatsapp_sessions`))
}

// TestOneWhatsappMessageIsOneRow is E9.4's first gate at the storage level: WhatsApp
// retries a delivery when an acknowledgement is slow, and a retry must not become a
// second message — which is what stops a second task for the same customer question.
func TestOneWhatsappMessageIsOneRow(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	first, created, err := fixture.repository.Record(ctx, fixture.scope, fixture.inbound("msg-duplicate"))
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, uuid.Nil, first.ID)

	second, created, err := fixture.repository.Record(ctx, fixture.scope, fixture.inbound("msg-duplicate"))
	require.NoError(t, err)
	require.False(t, created, "a redelivery is not a new message")
	require.Equal(t, first.ID, second.ID, "and the caller is answered with the row it already has")

	require.Equal(t, 1, countInTenant(t, fixture, `SELECT count(*) FROM whatsapp_messages WHERE wa_message_id = $1`, "msg-duplicate"))
}

// TestAMessageWithoutAnAttachmentIsNotPendingMedia: the media pass reads rows in
// `pending`, so a text message must not sit there waiting to be downloaded.
func TestAMessageWithoutAnAttachmentIsNotPendingMedia(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	_, _, err := fixture.repository.Record(ctx, fixture.scope, fixture.inbound("msg-text"))
	require.NoError(t, err)

	pending, err := fixture.repository.PendingMedia(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Empty(t, pending, "a text message has nothing to fetch")

	// A message with an attachment is waiting, and stops waiting once the bytes
	// are recorded.
	withMedia := fixture.inbound("msg-image")
	withMedia.Kind = whatsappdomain.KindImage
	withMedia.Media = whatsappdomain.Media{
		Ref: "/v/t62/x", Kind: whatsappdomain.KindImage, Size: 2048, Status: whatsappdomain.MediaPending,
	}
	stored, _, err := fixture.repository.Record(ctx, fixture.scope, withMedia)
	require.NoError(t, err)

	pending, err = fixture.repository.PendingMedia(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "msg-image", pending[0].WaMessageID)

	require.NoError(t, fixture.repository.SetMedia(ctx, fixture.scope, stored.ID, whatsappdomain.Media{
		Kind: whatsappdomain.KindImage, Size: 2048, Status: whatsappdomain.MediaReady,
		Key: "whatsapp/" + fixture.workspace.String() + "/" + stored.ID.String() + "/image-1.jpg",
	}))

	pending, err = fixture.repository.PendingMedia(ctx, fixture.scope, 10)
	require.NoError(t, err)
	require.Empty(t, pending)

	loaded, err := fixture.repository.MessageByID(ctx, fixture.scope, stored.ID)
	require.NoError(t, err)
	require.Equal(t, whatsappdomain.MediaReady, loaded.Media.Status)
	require.NotEmpty(t, loaded.Media.Key)
}

// TestOneMessageGoesToOneSender is E9.5's first gate at the storage level: the read
// and the claim are one statement, so two workers of one fleet cannot both send the
// same message — and a second reader skips a row the first is claiming rather than
// waiting for it.
func TestOneMessageGoesToOneSender(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	queued, err := fixture.repository.Enqueue(ctx, fixture.scope, whatsappdomain.Outbox{
		NumberID:  fixture.number.ID,
		ToJID:     "628999888777@s.whatsapp.net",
		Kind:      whatsappdomain.KindText,
		Text:      "pesanannya sudah dikirim",
		Status:    whatsappdomain.OutboxQueued,
		NotBefore: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)

	first, err := fixture.repository.Due(ctx, fixture.scope, fixture.number.ID, 5)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, queued.ID, first[0].ID)
	require.Equal(t, domain1Sending, first[0].Status)
	require.Equal(t, 1, first[0].Attempts, "the claim spends one attempt")

	// The second reader finds nothing: the row is claimed, so it is not due.
	second, err := fixture.repository.Due(ctx, fixture.scope, fixture.number.ID, 5)
	require.NoError(t, err)
	require.Empty(t, second, "a claimed message is not handed to a second sender")

	require.NoError(t, fixture.repository.MarkSent(ctx, fixture.scope, queued.ID, "wa-1", time.Now()))

	sent, err := fixture.repository.Outbox(ctx, fixture.scope, whatsappdomain.OutboxFilter{NumberID: fixture.number.ID})
	require.NoError(t, err)
	require.Len(t, sent, 1)
	require.Equal(t, whatsappdomain.OutboxSent, sent[0].Status)
	require.Equal(t, "wa-1", sent[0].WaMessageID)
	require.NotNil(t, sent[0].SentAt)
}

// domain1Sending is the status a claimed message carries.
const domain1Sending = whatsappdomain.OutboxSending

// TestTheCeilingIsCountedFromWhatLeft is E9.6's first gate at the storage level: the
// count is of messages actually sent, so a counter cannot drift away from reality.
func TestTheCeilingIsCountedFromWhatLeft(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	require.Zero(t, countSent(t, fixture, time.Hour))

	for range 3 {
		queued, err := fixture.repository.Enqueue(ctx, fixture.scope, whatsappdomain.Outbox{
			NumberID: fixture.number.ID, ToJID: "628999888777@s.whatsapp.net",
			Kind: whatsappdomain.KindText, Text: "halo", Status: whatsappdomain.OutboxQueued,
			NotBefore: time.Now().Add(-time.Second),
		})
		require.NoError(t, err)

		claimed, err := fixture.repository.Due(ctx, fixture.scope, fixture.number.ID, 1)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.Equal(t, queued.ID, claimed[0].ID)
		require.NoError(t, fixture.repository.MarkSent(ctx, fixture.scope, queued.ID, "wa-"+uuid.NewString(), time.Now()))
	}

	require.Equal(t, 3, countSent(t, fixture, time.Hour), "the count is of what left")

	// An hour later the window has moved past them, which is what makes the ceiling
	// a rate rather than a quota for the day.
	require.Zero(t, countSent(t, fixture, -time.Hour))
}

// countSent reads the send count over a window that reaches back that far, which is
// what the ceiling asks: "how many left in the last hour".
func countSent(t *testing.T, fixture *whatsappFixture, window time.Duration) int {
	t.Helper()

	sent, err := fixture.repository.SentSince(t.Context(), fixture.scope, fixture.number.ID, time.Now().Add(-window))
	require.NoError(t, err)
	return sent
}

// TestAHeldMessageKeepsItsPlace is what makes the ceiling a pace rather than a
// refusal: the message goes back to the queue with a later time, and it is still
// there.
func TestAHeldMessageKeepsItsPlace(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	queued, err := fixture.repository.Enqueue(ctx, fixture.scope, whatsappdomain.Outbox{
		NumberID: fixture.number.ID, ToJID: "628999888777@s.whatsapp.net",
		Kind: whatsappdomain.KindText, Text: "halo", Status: whatsappdomain.OutboxQueued,
		NotBefore: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)

	claimed, err := fixture.repository.Due(ctx, fixture.scope, fixture.number.ID, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, 1, claimed[0].Attempts)

	require.NoError(t, fixture.repository.MarkHeld(ctx, fixture.scope, queued.ID, "batas kirim per jam tercapai", time.Now().Add(5*time.Minute)))

	held, err := fixture.repository.Outbox(ctx, fixture.scope, whatsappdomain.OutboxFilter{NumberID: fixture.number.ID})
	require.NoError(t, err)
	require.Len(t, held, 1)
	require.Equal(t, whatsappdomain.OutboxQueued, held[0].Status, "a held message keeps its place in the queue")
	require.Contains(t, held[0].LastError, "batas")
	require.Equal(t, 0, held[0].Attempts, "a pace is not a failure, so the attempt is given back")

	// And it is not due until its time comes.
	due, err := fixture.repository.Due(ctx, fixture.scope, fixture.number.ID, 1)
	require.NoError(t, err)
	require.Empty(t, due, "the hold is a time, not a hint")
}

// TestAFailedAttemptKeepsTheAttemptCount: unlike a hold, a real failure has to
// accumulate, or a message that keeps failing would be retried forever.
func TestAFailedAttemptKeepsTheAttemptCount(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	queued, err := fixture.repository.Enqueue(ctx, fixture.scope, whatsappdomain.Outbox{
		NumberID: fixture.number.ID, ToJID: "628999888777@s.whatsapp.net",
		Kind: whatsappdomain.KindText, Text: "halo", Status: whatsappdomain.OutboxQueued,
		NotBefore: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)

	claimed, err := fixture.repository.Due(ctx, fixture.scope, fixture.number.ID, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, 1, claimed[0].Attempts)

	require.NoError(t, fixture.repository.MarkRetry(ctx, fixture.scope, queued.ID, "koneksi putus", time.Now().Add(5*time.Second)))

	retried, err := fixture.repository.Outbox(ctx, fixture.scope, whatsappdomain.OutboxFilter{NumberID: fixture.number.ID})
	require.NoError(t, err)
	require.Len(t, retried, 1)
	require.Equal(t, whatsappdomain.OutboxQueued, retried[0].Status)
	require.Equal(t, 1, retried[0].Attempts, "the attempt is kept, so a message that keeps failing is parked")
	require.Nil(t, retried[0].ClaimedAt, "and it is not left looking claimed")
}

// TestAMessageThatWillNotGoOutStaysVisible is E9.5's second gate at the storage
// level: a message that failed is parked with its reason rather than dropped, which
// is what a person reads when a customer asks why nothing came.
func TestAMessageThatWillNotGoOutStaysVisible(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	queued, err := fixture.repository.Enqueue(ctx, fixture.scope, whatsappdomain.Outbox{
		NumberID: fixture.number.ID, ToJID: "628999888777@s.whatsapp.net",
		Kind: whatsappdomain.KindText, Text: "halo", Status: whatsappdomain.OutboxQueued,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, queued.ID)

	require.NoError(t, fixture.repository.MarkFailed(ctx, fixture.scope, queued.ID, "nomor tidak dikenal", 3))

	failed, err := fixture.repository.Outbox(ctx, fixture.scope, whatsappdomain.OutboxFilter{Status: whatsappdomain.OutboxFailed})
	require.NoError(t, err)
	require.Len(t, failed, 1)
	require.Equal(t, "nomor tidak dikenal", failed[0].LastError)
	require.Equal(t, 3, failed[0].Attempts)
	require.Nil(t, failed[0].SentAt, "nothing was sent, so nothing records a send")
}

// TestAMessageAdeadGatewayLeftIsReturned is what a takeover does with the messages a
// replaced gateway had in flight: they go back to the queue rather than sitting in
// `sending` forever.
func TestAMessageADeadGatewayLeftIsReturned(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	queued, err := fixture.repository.Enqueue(ctx, fixture.scope, whatsappdomain.Outbox{
		NumberID: fixture.number.ID, ToJID: "628999888777@s.whatsapp.net",
		Kind: whatsappdomain.KindText, Text: "halo", Status: whatsappdomain.OutboxQueued,
		NotBefore: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, queued.ID)

	claimed, err := fixture.repository.Due(ctx, fixture.scope, fixture.number.ID, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// A gateway that is still alive keeps its claim: nothing is reclaimed on a
	// healthy instance.
	reclaimed, err := fixture.repository.Reclaim(ctx, fixture.scope, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.Zero(t, reclaimed)

	// A gateway that died leaves the row claimed with an old timestamp.
	reclaimed, err = fixture.repository.Reclaim(ctx, fixture.scope, time.Now().Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	returned, err := fixture.repository.Outbox(ctx, fixture.scope, whatsappdomain.OutboxFilter{NumberID: fixture.number.ID})
	require.NoError(t, err)
	require.Len(t, returned, 1)
	require.Equal(t, whatsappdomain.OutboxQueued, returned[0].Status)
	require.Contains(t, returned[0].LastError, "terputus")
}

// TestTheDirectoryIsReadWithoutATenant is the one unscoped read in the codebase, and
// this is what justifies it: a gateway has to discover which tenants have a number
// before it can open any workspace's scope at all.
func TestTheDirectoryIsReadWithoutATenant(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	before, err := fixture.repository.Serving(ctx)
	require.NoError(t, err)
	for _, assignment := range before {
		require.NotEqual(t, fixture.number.ID, assignment.NumberID)
	}

	require.NoError(t, fixture.repository.Publish(ctx, fixture.scope, fixture.number.ID, true))

	after, err := fixture.repository.Serving(ctx)
	require.NoError(t, err)

	found := false
	for _, assignment := range after {
		if assignment.NumberID == fixture.number.ID {
			found = true
			require.Equal(t, fixture.workspace, assignment.WorkspaceID, "the row names the tenant to scope under")
		}
	}
	require.True(t, found, "a published number is served by the fleet")

	// Disabling it takes it out of the fleet's view without deleting the number.
	require.NoError(t, fixture.repository.Publish(ctx, fixture.scope, fixture.number.ID, false))

	after, err = fixture.repository.Serving(ctx)
	require.NoError(t, err)
	for _, assignment := range after {
		require.NotEqual(t, fixture.number.ID, assignment.NumberID)
	}
}

// TestAPhoneNumberBelongsToOneWorkspace is why the uniqueness is deployment-wide
// rather than per tenant: a WhatsApp session belongs to the number, so a second
// workspace linking it would take the first one's line over. The conflict arrives as
// this module's own error rather than as a constraint violation, because Row Level
// Security hides the other tenant's row from the check that would have found it.
func TestAPhoneNumberBelongsToOneWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	mine := newWhatsAppFixture(t, dsn)
	theirs := newWhatsAppFixture(t, dsn)
	ctx := t.Context()

	theirsConnection := theirs.createConnection(t)
	_, err := theirs.repository.Create(ctx, theirs.scope, whatsappdomain.Number{
		IntegrationID: theirsConnection,
		Phone:         mine.number.Phone,
		Label:         "nomor yang sama",
		AgentID:       theirs.agentID,
		Status:        whatsappdomain.StatusUnlinked,
		HourlyLimit:   40,
	})
	require.ErrorIs(t, err, whatsappdomain.ErrAlreadyLinked)

	// And the same workspace cannot have it twice either, which is what makes a
	// relink find its row instead of adding a second one.
	_, err = mine.repository.Create(ctx, mine.scope, whatsappdomain.Number{
		IntegrationID: mine.createConnection(t),
		Phone:         mine.number.Phone,
		Label:         "nomor yang sama",
		AgentID:       mine.agentID,
		Status:        whatsappdomain.StatusUnlinked,
		HourlyLimit:   40,
	})
	require.ErrorIs(t, err, whatsappdomain.ErrAlreadyLinked)

	// The relink path finds it by phone, which is what makes "tautkan ulang" work
	// on the row that already exists.
	found, err := mine.repository.ByPhone(ctx, mine.scope, mine.number.Phone)
	require.NoError(t, err)
	require.Equal(t, mine.number.ID, found.ID)
}

// TestABoluFromAnotherWorkspaceCannotAnswerTheNumber: the insert is a SELECT, so a
// Bolu outside the caller's workspace comes back as no rows rather than as a foreign
// key violation — and the service turns that into a message about the choice rather
// than about the database.
func TestABoluFromAnotherWorkspaceCannotAnswerTheNumber(t *testing.T) {
	dsn := appDatabase(t)
	mine := newWhatsAppFixture(t, dsn)
	theirs := newWhatsAppFixture(t, dsn)

	_, err := mine.repository.Create(t.Context(), mine.scope, whatsappdomain.Number{
		IntegrationID: mine.createConnection(t),
		Phone:         "628" + uuid.NewString()[:9],
		Label:         "nomor lain",
		AgentID:       theirs.agentID,
		Status:        whatsappdomain.StatusUnlinked,
		HourlyLimit:   40,
	})
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)
}

// TestForgettingASessionKeepsTheNumber is the split that makes a relink possible:
// the session is deleted and the number stays, so what a customer wrote is still
// there and the row can be linked again.
func TestForgettingASessionKeepsTheNumber(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	require.NoError(t, fixture.repository.Save(ctx, fixture.scope, whatsappdomain.Session{
		NumberID:    fixture.number.ID,
		WorkspaceID: fixture.workspace,
		Sealed:      []byte("sealed"),
	}))
	_, _, err := fixture.repository.Record(ctx, fixture.scope, fixture.inbound("msg-kept"))
	require.NoError(t, err)

	require.NoError(t, fixture.repository.ForgetSession(ctx, fixture.scope, fixture.number.ID))

	_, err = fixture.repository.Load(ctx, fixture.scope, fixture.number.ID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)

	// The number and its history are untouched.
	kept, err := fixture.repository.Get(ctx, fixture.scope, fixture.number.ID)
	require.NoError(t, err)
	require.Equal(t, fixture.number.Phone, kept.Phone)

	require.Equal(t, 1, countInTenant(t, fixture, `SELECT count(*) FROM whatsapp_messages WHERE wa_message_id = $1`, "msg-kept"))
}

// TestTheGatewayCannotWriteOutsideATenant: every write this module makes carries a
// workspace, and one without it is refused before the database is touched. That is
// what stops a caller without a tenant from writing anywhere.
func TestTheGatewayCannotWriteOutsideATenant(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()
	empty := whatsappdomain.Scope{}

	_, err := fixture.repository.Create(ctx, empty, whatsappdomain.Number{})
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)

	_, err = fixture.repository.Get(ctx, empty, fixture.number.ID)
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)

	_, _, err = fixture.repository.Record(ctx, empty, fixture.inbound("msg-no-tenant"))
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)

	_, err = fixture.repository.Enqueue(ctx, empty, whatsappdomain.Outbox{})
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)

	require.ErrorIs(t, fixture.repository.ForgetSession(ctx, empty, fixture.number.ID), whatsappdomain.ErrInvalidInput)

	// The unscoped read is the one that needs no tenant, and it is the fleet's.
	_, err = fixture.repository.Serving(ctx)
	require.NoError(t, err)
}

// TestTheModuleServesItsReadsWithoutAGateway: the container is what decides whether
// a transport exists. With none, the repository still answers, which is what lets a
// deployment run the API before the gateway exists.
func TestTheModuleServesItsReadsWithoutAGateway(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))

	numbers, err := fixture.repository.List(t.Context(), fixture.scope)
	require.NoError(t, err)
	require.Len(t, numbers, 1)
	require.Equal(t, fixture.number.ID, numbers[0].ID)
	require.Equal(t, fixture.agentID, numbers[0].AgentID)
}

// TestADeletedNumberTakesItsSessionAndQueueWithIt: the cascade is what keeps an
// unlinked number from leaving a sealed session behind that nothing can reach.
func TestADeletedNumberTakesItsSessionAndQueueWithIt(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	require.NoError(t, fixture.repository.Save(ctx, fixture.scope, whatsappdomain.Session{
		NumberID: fixture.number.ID, WorkspaceID: fixture.workspace, Sealed: []byte("sealed"),
	}))
	_, err := fixture.repository.Enqueue(ctx, fixture.scope, whatsappdomain.Outbox{
		NumberID: fixture.number.ID, ToJID: "628999888777@s.whatsapp.net",
		Kind: whatsappdomain.KindText, Text: "halo", Status: whatsappdomain.OutboxQueued,
	})
	require.NoError(t, err)

	require.NoError(t, fixture.repository.Delete(ctx, fixture.scope, fixture.number.ID))

	require.Equal(t, 0, countInTenant(t, fixture, `SELECT count(*) FROM whatsapp_numbers`))
	require.Equal(t, 0, countInTenant(t, fixture, `SELECT count(*) FROM whatsapp_sessions`))
	require.Equal(t, 0, countInTenant(t, fixture, `SELECT count(*) FROM whatsapp_outbox`))
	require.Equal(t, 0, countInTenant(t, fixture, `SELECT count(*) FROM whatsapp_directory`))
}

// TestAListIsCapped: one request must not pull every message a number ever received,
// which is what a list limit is for.
func TestAListIsCapped(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	for index := range 5 {
		message := fixture.inbound("msg-list-" + string(rune('a'+index)))
		message.ReceivedAt = time.Now().Add(time.Duration(index) * time.Second)
		_, _, err := fixture.repository.Record(ctx, fixture.scope, message)
		require.NoError(t, err)
	}

	listed, err := fixture.repository.Messages(ctx, fixture.scope, whatsappdomain.MessageFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, listed, 2, "the caller's limit is honoured")
	// Newest first, which is what a screen reads.
	require.Greater(t, listed[0].ReceivedAt, listed[1].ReceivedAt)
}

// TestTheContainerWiresTheModule is the wiring check a unit test cannot make: the
// container's own repository and service satisfy the ports the module declares.
func TestTheContainerWiresTheModule(t *testing.T) {
	ctx := t.Context()
	app := newWhatsAppContainer(t, appDatabase(t))
	require.NotNil(t, app.Services.WhatsApp, "the module must be wired whenever there is a database")

	// It answers its own reads.
	_, err := app.Services.WhatsApp.Assignments(ctx)
	require.NoError(t, err)

	// And it reports a missing workspace rather than running against whatever the
	// connection happens to carry.
	_, err = app.Services.WhatsApp.Numbers(ctx, whatsappdomain.Scope{})
	require.ErrorIs(t, err, whatsappdomain.ErrInvalidInput)

	// The handler is mounted, so the routes a screen calls exist in a deployment.
	require.NotNil(t, app.Handlers.WhatsApp, "the routes must be mounted")
}

// TestASessionIsSealedWithItsWorkspacesOwnKey is the confidentiality half of E9.1
// at the level only the wired sealer can prove: the same bytes sealed under one
// workspace's data key do not open under another's, because the workspace is the
// associated data of the seal.
func TestASessionIsSealedWithItsWorkspacesOwnKey(t *testing.T) {
	dsn := appDatabase(t)
	mine := newWhatsAppFixture(t, dsn)
	theirs := newWhatsAppFixture(t, dsn)

	app := newWhatsAppContainer(t, dsn)
	ctx := t.Context()

	sealer := app.WhatsAppSealer()
	require.NotNil(t, sealer, "the container must seal sessions with the workspace key ring")

	sealed, err := sealer.Seal(ctx, mine.workspace, []byte("the device document"))
	require.NoError(t, err)

	opened, err := sealer.Open(ctx, mine.workspace, sealed)
	require.NoError(t, err)
	require.Equal(t, []byte("the device document"), opened)

	// The other workspace's key does not open it. The key ring derives a separate
	// data key per workspace and binds the id into the seal, so this fails rather
	// than returning the bytes.
	_, err = sealer.Open(ctx, theirs.workspace, sealed)
	require.Error(t, err, "a session sealed for one workspace must not open for another")

	// And a byte changed in the ciphertext is refused rather than returned.
	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 0xff
	_, err = sealer.Open(ctx, mine.workspace, tampered)
	require.Error(t, err, "a tampered session must not open")
}

// TestTheSessionOfADeadGatewayIsNotServable is where E9.1's two halves meet: a blob
// stored in one workspace's row is not readable through another's scope, so the row
// and the key both have to belong to the same tenant.
func TestTheSessionOfOneWorkspaceIsNotOnAnothersRows(t *testing.T) {
	dsn := appDatabase(t)
	mine := newWhatsAppFixture(t, dsn)
	theirs := newWhatsAppFixture(t, dsn)
	app := newWhatsAppContainer(t, dsn)
	ctx := t.Context()

	sealer := app.WhatsAppSealer()
	sealed, err := sealer.Seal(ctx, mine.workspace, []byte("mine"))
	require.NoError(t, err)

	// The blob is written on MY row, and MY scope opens it.
	require.NoError(t, mine.repository.Save(ctx, mine.scope, whatsappdomain.Session{
		NumberID: mine.number.ID, WorkspaceID: mine.workspace, Sealed: sealed,
	}))
	loaded, err := mine.repository.Load(ctx, mine.scope, mine.number.ID)
	require.NoError(t, err)
	opened, err := sealer.Open(ctx, mine.workspace, loaded.Sealed)
	require.NoError(t, err)
	require.Equal(t, []byte("mine"), opened)

	// The other tenant cannot even reach the row, so the pair cannot be assembled
	// from outside.
	_, err = theirs.repository.Load(ctx, theirs.scope, mine.number.ID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)
}

// newWhatsAppContainer builds the container the API process would: the module's
// repository, its service, and the key ring it seals sessions with.
func newWhatsAppContainer(t *testing.T, dsn string) *container.Container {
	t.Helper()

	ctx := t.Context()

	cfg := &config.Config{
		Application: config.AppConfig{Environment: "test", FrontendURL: "https://app.example.com"},
		Http:        config.HttpConfig{Address: ":0", ApiPrefix: "/api"},
		Database:    config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 1},
		JWT:         config.JWTConfig{Secret: "integration-test-secret", ExpireHours: 1},
		Auth: config.AuthConfig{
			AccessTokenMinutes: 15, RefreshHours: 24, VerificationHours: 24,
			ResetMinutes: 60, StateMinutes: 10, LoginThrottleCapacity: 100, LoginThrottleRefill: 100,
		},
		Mail:    config.MailConfig{Driver: "log"},
		Logging: config.LoggingConfig{Level: "error", Format: "json"},
		// The key ring is what seals a session, so it is configured here rather
		// than left empty: without it the module reports that it cannot store a
		// session, which is a different test.
		Kms: config.KmsConfig{MasterKey: kmsKey(41), MasterKeyID: "v1"},
	}

	app, err := container.New(ctx, cfg, container.WithLogger(zap.NewNop()))
	require.NoError(t, err)
	t.Cleanup(app.Close)

	return app
}

// TestTheRowKnowsWhatAPersonChangedAndWhatTheGatewaySaw is the number's own
// bookkeeping, and each part of it is a claim a mock would only restate:
//
//   - the fields a person edits are written together and keep the ones they do not
//     name, so a rename cannot silently reset a ceiling;
//   - a status carries the reason a person has to read;
//   - the heartbeat moves `last_seen_at` forward only, so a late report from the
//     old gateway cannot walk "is it alive" backwards;
//   - accepting the warning twice keeps the first moment, because that is when the
//     risk was taken.
func TestTheRowKnowsWhatAPersonChangedAndWhatTheGatewaySaw(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	// The fields a person edits: the label and the ceiling change, the Bolu does not.
	renamed, err := fixture.repository.Update(ctx, fixture.scope, whatsappdomain.Number{
		ID:          fixture.number.ID,
		Label:       "Kasir",
		AgentID:     fixture.number.AgentID,
		HourlyLimit: 12,
	})
	require.NoError(t, err)
	require.Equal(t, "Kasir", renamed.Label)
	require.Equal(t, 12, renamed.HourlyLimit)
	require.Equal(t, fixture.number.AgentID, renamed.AgentID)
	require.Equal(t, fixture.number.Phone, renamed.Phone, "the number WhatsApp reports is not a field a person edits")

	// A number in another workspace is not updated, and the caller is told the row
	// is not there rather than that the write failed.
	their := newWhatsAppFixture(t, appDatabase(t))
	_, err = their.repository.Update(ctx, their.scope, whatsappdomain.Number{
		ID: fixture.number.ID, Label: "bukan milikku", AgentID: fixture.number.AgentID, HourlyLimit: 3,
	})
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)

	still, err := fixture.repository.Get(ctx, fixture.scope, fixture.number.ID)
	require.NoError(t, err)
	require.Equal(t, "Kasir", still.Label, "and the row it named is untouched")

	// A status carries why, because a person acts on the reason rather than on the
	// status word.
	reported, err := fixture.repository.SetStatus(ctx, fixture.scope, fixture.number.ID,
		whatsappdomain.StatusNeedsRelink, "sesi ditolak WhatsApp")
	require.NoError(t, err)
	require.Equal(t, whatsappdomain.StatusNeedsRelink, reported.Status)
	require.Equal(t, "sesi ditolak WhatsApp", reported.StatusReason)

	// And the next report replaces it, which is what makes the later observer's word
	// the one that holds.
	reported, err = fixture.repository.SetStatus(ctx, fixture.scope, fixture.number.ID,
		whatsappdomain.StatusConnected, "")
	require.NoError(t, err)
	require.Equal(t, whatsappdomain.StatusConnected, reported.Status)
	require.Empty(t, reported.StatusReason, "the reason travels with the status it belonged to")

	// The heartbeat: a report from now moves it, and a report from before does not
	// move it back.
	now := time.Now()
	require.NoError(t, fixture.repository.Touch(ctx, fixture.scope, fixture.number.ID, now))

	touched, err := fixture.repository.Get(ctx, fixture.scope, fixture.number.ID)
	require.NoError(t, err)
	require.NotNil(t, touched.LastSeenAt)
	require.WithinDuration(t, now, *touched.LastSeenAt, 2*time.Second)

	require.NoError(t, fixture.repository.Touch(ctx, fixture.scope, fixture.number.ID, now.Add(-time.Hour)))

	touched, err = fixture.repository.Get(ctx, fixture.scope, fixture.number.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, *touched.LastSeenAt, 2*time.Second,
		"a late report cannot walk last_seen_at backwards")
}

// TestAcceptingTheWarningTwiceKeepsTheFirstMoment: the moment is when the risk was
// taken, and it is what the screen shows. A second acceptance from the same person, or
// from a page reload, must not move it.
func TestAcceptingTheWarningTwiceKeepsTheFirstMoment(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	// A number nobody has accepted the warning for yet.
	fresh, err := fixture.repository.Create(ctx, fixture.scope, whatsappdomain.Number{
		IntegrationID: fixture.createConnection(t),
		Phone:         "628" + uuid.NewString()[:9],
		Label:         "belum disetujui",
		AgentID:       fixture.agentID,
		Status:        whatsappdomain.StatusUnlinked,
		HourlyLimit:   40,
	})
	require.NoError(t, err)
	require.Nil(t, fresh.TOSAcceptedAt, "a number starts without the warning accepted")

	first, err := fixture.repository.AcceptTerms(ctx, fixture.scope, fresh.ID)
	require.NoError(t, err)
	require.NotNil(t, first.TOSAcceptedAt)

	time.Sleep(time.Millisecond)
	second, err := fixture.repository.AcceptTerms(ctx, fixture.scope, fresh.ID)
	require.NoError(t, err)
	require.NotNil(t, second.TOSAcceptedAt)
	require.Equal(t, *first.TOSAcceptedAt, *second.TOSAcceptedAt, "the first moment stands")

	// Accepting for a number in another workspace is a row that is not there.
	their := newWhatsAppFixture(t, appDatabase(t))
	_, err = their.repository.AcceptTerms(ctx, their.scope, fresh.ID)
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)
}

// TestAMessageIsFoundByWhatsappsOwnIdAndKeepsItsTask is what a redelivery and the
// task list both read: WhatsApp's id is the lookup key, and the task a message opened
// stays on the row so a person can follow the answer back to the question.
func TestAMessageIsFoundByWhatsappsOwnIdAndKeepsItsTask(t *testing.T) {
	fixture := newWhatsAppFixture(t, appDatabase(t))
	ctx := t.Context()

	stored, created, err := fixture.repository.Record(ctx, fixture.scope, fixture.inbound("msg-by-id"))
	require.NoError(t, err)
	require.True(t, created)

	found, err := fixture.repository.ByWaID(ctx, fixture.scope, "msg-by-id")
	require.NoError(t, err)
	require.Equal(t, stored.ID, found.ID)
	require.Equal(t, stored.Text, found.Text)
	require.Equal(t, stored.ChatJID, found.ChatJID)

	// A message WhatsApp has not delivered is not there, which is what a caller
	// checking a redelivery reads.
	_, err = fixture.repository.ByWaID(ctx, fixture.scope, "msg-never-sent")
	require.ErrorIs(t, err, whatsappdomain.ErrNotFound)

	// The task a message opened is written onto it, and it is what the screen shows
	// next to the message.
	taskID := fixture.createTask(t)
	require.NoError(t, fixture.repository.AttachTask(ctx, fixture.scope, stored.ID, taskID))

	attached, err := fixture.repository.MessageByID(ctx, fixture.scope, stored.ID)
	require.NoError(t, err)
	require.Equal(t, taskID, attached.TaskID)

	// A task that does not exist cannot be named: the row points at the runtime's own
	// table, so a task id nobody created is refused by the database rather than
	// recorded as an answer to a question that was never asked. The refusal is left
	// as the constraint's own, because the only caller attaches the id the reply
	// just returned and there is no product sentence to write for it.
	require.Error(t, fixture.repository.AttachTask(ctx, fixture.scope, stored.ID, uuid.New()))

	// Another workspace cannot attach a task to this message, however it names it —
	// and it is told the row is not there rather than answered as done.
	their := newWhatsAppFixture(t, appDatabase(t))
	require.ErrorIs(t, their.repository.AttachTask(ctx, their.scope, stored.ID, their.createTask(t)),
		whatsappdomain.ErrNotFound)

	// A message that does not exist is refused the same way.
	require.ErrorIs(t, fixture.repository.AttachTask(ctx, fixture.scope, uuid.New(), taskID),
		whatsappdomain.ErrNotFound)

	untouched, err := fixture.repository.MessageByID(ctx, fixture.scope, stored.ID)
	require.NoError(t, err)
	require.Equal(t, taskID, untouched.TaskID, "and the row it named is untouched")
}

// createTask inserts the task a message may point at, which the runtime owns and this
// epic only references.
func (f *whatsappFixture) createTask(t *testing.T) uuid.UUID {
	t.Helper()

	var taskID uuid.UUID
	require.NoError(t, f.pool.InScope(t.Context(), database.Scope{
		UserID:      f.userID,
		WorkspaceID: f.workspace,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			`INSERT INTO tasks (workspace_id, agent_id, trigger, title)
			 VALUES ($1, $2, 'chat', 'Jawab pesanan WhatsApp')
			 RETURNING id`,
			f.workspace, f.agentID).Scan(&taskID)
	}))

	return taskID
}

package container

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	billingdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain/mocks"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	llmmocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain/mocks"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacemocks "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mail"
)

// The billing module's adapters are this file: the four translations between its ports and
// the modules behind them. Only the container can get them wrong — each is the sole place
// two modules meet — and each is small enough that being wrong looks like nothing until a
// workspace is served for free or a person is told they have stopped.

// TestTheQuotaPolicyAnswersWithThePeriodsAllowance: the gateway's pre-flight check and the
// task runtime's round check both read this, so it is what a workspace may actually spend.
func TestTheQuotaPolicyAnswersWithThePeriodsAllowance(t *testing.T) {
	workspaceID := uuid.New()
	svc := mocks.NewService(t)

	svc.EXPECT().Check(mock.Anything, workspaceID).Return(billingdomain.Check{
		Allowed:         true,
		Entitlement:     billingdomain.EntitlementActive,
		Period:          billingdomain.QuotaPeriod{TokensGranted: 2000, TokensToppedUp: 500, TokensUsed: 100},
		DailyCostMicros: 5_000_000,
	}, nil).Twice()

	policy := billingQuotaPolicy{service: svc}

	allowance, err := policy.Allowance(t.Context(), workspaceID)
	require.NoError(t, err)
	require.Equal(t, int64(2500), allowance, "the package plus what was bought on top")

	ceiling, err := policy.DailyCostAllowance(t.Context(), workspaceID)
	require.NoError(t, err)
	require.Equal(t, int64(5_000_000), ceiling)
}

// TestARefusalCrossesThePortsAsHowManyTokensAreLeft is the subtle part.
//
// The gateway's port reads a zero as *unlimited*, so a refusal must not be reported as a
// zero allowance — it would read as "spend freely". It is reported as the error instead,
// which is what makes the gateway refuse the call with the sentence the package wrote.
func TestARefusalCrossesThePortsAsHowManyTokensAreLeft(t *testing.T) {
	workspaceID := uuid.New()

	cases := []struct {
		name string
		err  error
	}{
		{"an allowance that is spent", billingdomain.ErrQuotaExceeded},
		{"a package past its grace window", billingdomain.ErrReadOnly},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := mocks.NewService(t)
			svc.EXPECT().Check(mock.Anything, workspaceID).Return(billingdomain.Check{}, tc.err).Twice()

			policy := billingQuotaPolicy{service: svc}

			allowance, err := policy.Allowance(t.Context(), workspaceID)
			require.ErrorIs(t, err, tc.err, "a refusal is passed through rather than read as unlimited")
			require.Zero(t, allowance)

			_, err = policy.DailyCostAllowance(t.Context(), workspaceID)
			require.ErrorIs(t, err, tc.err)
		})
	}

	// A failure that is neither — the database is down, the workspace has no row — is also
	// an error, because reporting it as an allowance would hand out a month.
	failing := mocks.NewService(t)
	failing.EXPECT().Check(mock.Anything, workspaceID).
		Return(billingdomain.Check{}, errors.New("the subscription store is down")).Once()

	policy := billingQuotaPolicy{service: failing}

	_, err := policy.Allowance(t.Context(), workspaceID)
	require.Error(t, err)
	require.NotErrorIs(t, err, billingdomain.ErrQuotaExceeded)
}

// TestTheUsageAdapterReadsTheLedger: the ledger is the durable truth about what was spent,
// and the day boundary is the gateway's own rule — asking for it rather than recomputing it
// is what keeps the two from drifting.
func TestTheUsageAdapterReadsTheLedger(t *testing.T) {
	workspaceID := uuid.New()
	reader := llmmocks.NewUsageReader(t)

	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reader.EXPECT().SumSince(mock.Anything, workspaceID, since).Return(int64(1500), nil).Once()
	reader.EXPECT().CostSince(mock.Anything, workspaceID, mock.Anything).Return(int64(2_500_000), nil).Once()

	usage := billingUsage{reader: reader}

	tokens, err := usage.PeriodTokens(t.Context(), workspaceID, since)
	require.NoError(t, err)
	require.Equal(t, int64(1500), tokens)

	cost, err := usage.DailyCost(t.Context(), workspaceID)
	require.NoError(t, err)
	require.Equal(t, int64(2_500_000), cost)

	// An unwired gateway answers the sentinel rather than zero, which is what tells the
	// service to read its other source instead of believing nothing was spent.
	_, err = billingUsage{}.PeriodTokens(t.Context(), workspaceID, since)
	require.ErrorIs(t, err, errNoUsageReader)

	_, err = billingUsage{}.DailyCost(t.Context(), workspaceID)
	require.ErrorIs(t, err, errNoUsageReader)
}

// TestTheCounterAdapterReadsTheLiveWindow: the counter is the fast path the hot check reads,
// and without Redis it answers the sentinel so the ledger answers instead.
func TestTheCounterAdapterReadsTheLiveWindow(t *testing.T) {
	workspaceID := uuid.New()

	// Without Redis the adapter answers the sentinel rather than zero: a zero would read as
	// "nothing spent", which is the one thing an outage must not say.
	_, err := billingCounter{}.PeriodTokens(t.Context(), workspaceID)
	require.ErrorIs(t, err, errNoCounter)

	_, err = billingCounter{}.DailyCost(t.Context(), workspaceID)
	require.ErrorIs(t, err, errNoCounter)
}

// TestTheNotifierTellsThePeopleWhoCanAct is the adapter onto the mailer and the stream.
//
// Who hears about a notice is not the same for every notice, and the difference is the
// point: an allowance about to run out concerns everybody who works in the workspace, and
// the rest ask for money or report that it arrived, which is the owner's and the admin's
// business.
func TestTheNotifierTellsThePeopleWhoCanAct(t *testing.T) {
	owner := workspacedomain.Member{Email: "owner@example.com", Role: workspacedomain.RoleOwner}
	admin := workspacedomain.Member{Email: "admin@example.com", Role: workspacedomain.RoleAdmin}
	member := workspacedomain.Member{Email: "member@example.com", Role: workspacedomain.RoleMember}

	workspaces := workspacemocks.NewRepository(t)
	workspaces.EXPECT().Members(mock.Anything, mock.Anything).
		Return([]workspacedomain.Member{owner, admin, member}, nil)

	mailer := &recordingMailer{}
	activity := &recordingActivity{}

	notifier := billingNotifier{
		workspaces: workspaces,
		mailer:     mailer,
		activity:   activity,
		logger:     zap.NewNop(),
	}

	scope := billingdomain.Scope{WorkspaceID: uuid.New()}

	// A warning reaches everybody who works in the workspace.
	require.NoError(t, notifier.Notify(t.Context(), scope, billingdomain.Notice{
		Kind: billingdomain.NoticeWarning, Threshold: 80, Reason: "kuota hampir habis",
	}))
	require.Len(t, mailer.sent, 3)
	require.Equal(t, 1, activity.emitted)

	// The rest reach the people who can act on them.
	mailer.sent = nil
	require.NoError(t, notifier.Notify(t.Context(), scope, billingdomain.Notice{
		Kind: billingdomain.NoticePastDue, PlanName: "Paket Keluarga", Reason: "tagihan menunggu",
	}))
	require.Len(t, mailer.sent, 2)
	for _, message := range mailer.sent {
		require.NotEqual(t, "member@example.com", message.To, "a plain member cannot pay the invoice")
	}

	require.Equal(t, 2, activity.emitted, "both notices reached the stream")

	// And a notice reaches the stream even when nothing can send email, because the two are
	// one decision reaching two places rather than two decisions.
	streamOnly := billingNotifier{activity: activity, logger: zap.NewNop()}
	require.NoError(t, streamOnly.Notify(t.Context(), scope, billingdomain.Notice{Kind: billingdomain.NoticePaid}))
	require.Equal(t, 3, activity.emitted)

	// And a mailer that refuses does not fail the caller's work: an allowance that was spent
	// is spent whether or not the email arrived.
	refusing := billingNotifier{
		workspaces: workspaces,
		mailer:     failingMailer{},
		activity:   activity,
		logger:     zap.NewNop(),
	}
	workspaces.EXPECT().Members(mock.Anything, mock.Anything).
		Return([]workspacedomain.Member{owner}, nil).Maybe()

	require.NoError(t, refusing.Notify(t.Context(), scope, billingdomain.Notice{Kind: billingdomain.NoticeReadOnly}))
}

// TestTheNotifierSaysTheRightThingForEachNotice: the subject is what a person reads in an
// inbox, and a warning at 80% and one at 100% are different sentences.
func TestTheNotifierSaysTheRightThingForEachNotice(t *testing.T) {
	cases := []struct {
		notice  billingdomain.Notice
		subject string
	}{
		{billingdomain.Notice{Kind: billingdomain.NoticeWarning, Threshold: 80}, "hampir habis"},
		{billingdomain.Notice{Kind: billingdomain.NoticeWarning, Threshold: 100}, "Kuota token workspace habis"},
		{billingdomain.Notice{Kind: billingdomain.NoticePastDue}, "Tagihan"},
		{billingdomain.Notice{Kind: billingdomain.NoticeReadOnly}, "mode baca saja"},
		{billingdomain.Notice{Kind: billingdomain.NoticePaid}, "Pembayaran"},
		{billingdomain.Notice{Kind: billingdomain.NoticeTopUp}, "Paket tambahan"},
		{billingdomain.Notice{Kind: "something-new"}, "Langganan Bolu"},
	}

	for _, tc := range cases {
		subject, body := billingMessage(tc.notice)
		require.Contains(t, subject, tc.subject)
		require.Equal(t, tc.notice.Reason, body, "the body is the sentence the package wrote")
	}
}

// TestWhoPaysIsReadFromTheWorkspace: a payment is addressed to the workspace and the owner,
// and a lookup that fails is a receipt with no address rather than a failed payment.
func TestWhoPaysIsReadFromTheWorkspace(t *testing.T) {
	workspaceID, userID := uuid.New(), uuid.New()
	workspaces := workspacemocks.NewRepository(t)

	workspaces.EXPECT().Workspace(mock.Anything, mock.Anything).
		Return(workspacedomain.Workspace{ID: workspaceID, Name: "Toko Sinar"}, nil).Once()
	workspaces.EXPECT().Members(mock.Anything, mock.Anything).
		Return([]workspacedomain.Member{
			{Email: "member@example.com", Role: workspacedomain.RoleMember},
			{Email: "owner@example.com", Role: workspacedomain.RoleOwner},
		}, nil).Once()

	customer, err := billingCustomers{workspaces: workspaces}.
		Customer(t.Context(), billingdomain.Scope{UserID: userID, WorkspaceID: workspaceID})

	require.NoError(t, err)
	require.Equal(t, workspaceID, customer.WorkspaceID)
	require.Equal(t, "Toko Sinar", customer.Workspace)
	require.Equal(t, "owner@example.com", customer.Email, "the receipt goes to the person who pays")

	// A deployment with no workspace repository answers with an address-less customer
	// rather than failing the charge.
	empty, err := billingCustomers{}.Customer(t.Context(), billingdomain.Scope{WorkspaceID: workspaceID})
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, empty.WorkspaceID)
}

// TestTheBudgetPortIsTheGatewaysOwnCheck: the task module asks this before opening a task,
// and it must be the same policy the gateway asks — one implementation, so the two cannot
// disagree about whether a workspace may spend.
func TestTheBudgetPortIsTheGatewaysOwnCheck(t *testing.T) {
	workspaceID := uuid.New()
	gateway := llmmocks.NewQuotaChecker(t)

	gateway.EXPECT().Check(mock.Anything, workspaceID).Return(nil).Once()
	require.NoError(t, quotaBudget{checker: gateway}.Allow(t.Context(), workspaceID))

	gateway.EXPECT().Check(mock.Anything, workspaceID).
		Return(billingdomain.ErrQuotaExceeded).Once()
	require.ErrorIs(t, quotaBudget{checker: gateway}.Allow(t.Context(), workspaceID), billingdomain.ErrQuotaExceeded)
}

// TestTheBudgetRefusalBecomesTheChatModulesOwn: the container is where two modules meet, so
// it is where one module's refusal becomes the other's. A person gets a sentence they can
// act on rather than an internal failure.
func TestTheBudgetRefusalBecomesTheChatModulesOwn(t *testing.T) {
	reason, refused := budgetRefusal(billingdomain.ErrQuotaExceeded)
	require.True(t, refused)
	require.Contains(t, reason, "kuota token habis")

	// The sentence the package wrote is what survives, without the module's own prefix: a
	// person reads why the work cannot be opened, not which package noticed.
	sentence, refused := budgetRefusal(fmt.Errorf(
		"%w: 800000 dari 200000 token terpakai periode ini", billingdomain.ErrQuotaExceeded))
	require.True(t, refused)
	require.Contains(t, sentence, "200000 token terpakai")
	require.NotContains(t, sentence, "billing:")

	reason, refused = budgetRefusal(billingdomain.ErrReadOnly)
	require.True(t, refused)
	require.Contains(t, reason, "masa tenggang")

	// A failure that is neither is not translated, because it is not something a person
	// fixes by buying tokens.
	_, refused = budgetRefusal(errors.New("the subscription store is down"))
	require.False(t, refused)

	// The sentence is the package's own words with the module's prefix removed, so nothing
	// reads as "billing: ... : billing: ...".
	require.Equal(t, "pilih dulu paketnya", userSentence(
		errors.New("billing: invalid input: pilih dulu paketnya"), billingdomain.ErrInvalidInput))

	// A refusal that carries the sentinel in the middle — wrapped by every layer it passed
	// through — still yields the sentence rather than the list of packages that noticed.
	require.Equal(t, "kuota habis", userSentence(
		errors.New("llm: read quota allowance: kuota token habis: kuota habis"), billingdomain.ErrQuotaExceeded))
}

// recordingMailer keeps what was sent.
type recordingMailer struct {
	sent []mail.Message
}

func (m *recordingMailer) Send(_ context.Context, message mail.Message) error {
	m.sent = append(m.sent, message)
	return nil
}

// failingMailer refuses everything, which is what an SMTP server having a bad day looks
// like.
type failingMailer struct{}

func (failingMailer) Send(context.Context, mail.Message) error {
	return errors.New("smtp is down")
}

// recordingActivity counts what reached the workspace's stream.
type recordingActivity struct{ emitted int }

func (a *recordingActivity) Emit(context.Context, billingdomain.Event) error {
	a.emitted++
	return nil
}

// compile-time checks that the test doubles satisfy the ports they stand in for.
var (
	_ billingdomain.UsageReader  = billingUsage{}
	_ billingdomain.SpendCounter = billingCounter{}
	_ llmdomain.QuotaPolicy      = billingQuotaPolicy{}
	_ billingdomain.Activity     = (*recordingActivity)(nil)
)

// Package service implements the WhatsApp use cases: linking a number, who
// answers it, what a customer's message becomes, and how a reply is paced on its
// way out.
//
// Four rules run through the package:
//
//   - a number is linked once, and the session behind it is the number's rather
//     than the tenant's. A second workspace cannot take a number that is already
//     linked, because WhatsApp has one session per number and a second one would
//     take the first over.
//   - nothing leaves without an approved draft. This package carries a decision
//     out; it never makes one. The one exception is the pace, and a pace is not a
//     decision.
//   - a message is recorded before it is acted on. The record is what makes a
//     redelivery harmless, and it is what a person reads when they ask what a
//     customer said.
//   - a hold is not a failure. A number that has spent its hour keeps its queue;
//     a number whose session was refused says so and asks for a person.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/domain"
)

// App is the application key a linked number appears under in the hub.
const App = "whatsapp"

// DefaultHourlyLimit is the ceiling a number gets when the workspace did not
// choose one. It is a pace a person can keep up with rather than a rate limit:
// the point is that a batch reads as somebody writing.
const DefaultHourlyLimit = 40

// holdDuration is how long a message waits when the hourly ceiling stopped it.
// It is deliberately not the full hour: the ceiling is counted over a sliding
// hour, so a message that waits the whole hour would be the last to go rather
// than the next.
const holdDuration = 5 * time.Minute

// linkStateTTL is how long a finished link attempt is remembered, so a screen
// that polls one step behind sees "paired" rather than "gone".
const linkStateTTL = 2 * time.Minute

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Store
	// Sessions persists the sealed device state. The repository implements it;
	// it is a separate field because the engine's port is narrower than the
	// repository's.
	Sessions domain.SessionStore
	Sealer   domain.Sealer
	// Transport is the WhatsApp side. Optional in the sense that the module
	// still serves its reads without one: a deployment with no gateway shows the
	// numbers it has and refuses to link new ones rather than failing when
	// somebody clicks.
	Transport domain.Transport
	// Connections records a number as a connection in the hub. Optional for the
	// same reason: without it a number can be linked and read, and nothing can be
	// sent as it.
	Connections domain.Connections
	// Activity publishes what happened into the workspace's live stream.
	Activity domain.Activity
	// Alerter tells the owners when a number needs them.
	Alerter domain.Alerter
	// Replies opens the task an inbound message asks for.
	Replies domain.Replies
	// Watchers starts the routines watching the message event.
	Watchers domain.Watchers
	// Media keeps the bytes of what a customer sent, for the task to read.
	Media  domain.MediaStore
	Logger *zap.Logger
	// Now is the time source, so the pacing and the ceilings are testable without
	// waiting.
	Now func() time.Time
	// HourlyLimit is the deployment's default ceiling.
	HourlyLimit int
}

// Service is the WhatsApp implementation.
type Service struct {
	deps Deps
	now  func() time.Time
	// transportMu guards the transport, which is the one dependency that can be
	// installed after construction: the gateway process builds the engine that is
	// its transport once the service it calls back into exists.
	transportMu sync.RWMutex
	// finished remembers how a link attempt ended, so a screen polling for it
	// gets the answer rather than "gone".
	finished map[string]finishedLink
}

// finishedLink is one attempt's outcome, kept briefly after the transport forgot
// it.
type finishedLink struct {
	link     domain.Link
	forgetAt time.Time
}

// New builds the service.
func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = zap.NewNop()
	}
	if deps.HourlyLimit <= 0 {
		deps.HourlyLimit = DefaultHourlyLimit
	}

	return &Service{deps: deps, now: deps.Now, finished: map[string]finishedLink{}}
}

// Numbers returns the workspace's numbers.
func (s *Service) Numbers(ctx context.Context, scope domain.Scope) ([]domain.Number, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.List(ctx, scope)
}

// Number returns one number.
func (s *Service) Number(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.Get(ctx, scope, id)
}

// Prepare records one number and who answers it, before anything is linked.
//
// It is separate from BeginLink because two things have to happen in order: the
// warning is shown and accepted against a row, and only then does the link start.
// Doing both at once would mean a person could start a pairing they had not yet
// been told the risk of.
func (s *Service) Prepare(ctx context.Context, scope domain.Scope, request domain.LinkRequest) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if request.AgentID == uuid.Nil {
		return domain.Number{}, fmt.Errorf("%w: pilih dulu Bolu yang menjawab nomor ini", domain.ErrInvalidInput)
	}

	number, err := s.numberForLink(ctx, scope, request)
	if err != nil {
		return domain.Number{}, err
	}

	s.audit(ctx, scope, "whatsapp.prepared", number)
	return number, nil
}

// MessageByID returns one message.
func (s *Service) MessageByID(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Inbound, error) {
	if scope.IsZero() {
		return domain.Inbound{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	message, err := s.deps.Repository.MessageByID(ctx, scope, id)
	if err != nil {
		return domain.Inbound{}, err
	}

	return message, nil
}

// MediaBytes reads one stored attachment.
func (s *Service) MediaBytes(ctx context.Context, key string) ([]byte, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("%w: tidak ada lampiran", domain.ErrInvalidInput)
	}
	if s.deps.Media == nil {
		return nil, fmt.Errorf("%w: no media store is configured", domain.ErrNotConfigured)
	}

	return s.deps.Media.Get(ctx, key)
}

// BeginLink starts linking one number.
//
// It refuses until the workspace has accepted the warning and a Bolu has been
// chosen, and neither is a formality: linking a number over an unsupported route
// is a risk somebody accepts, and a number with no Bolu has nobody to answer the
// customers who write to it.
func (s *Service) BeginLink(ctx context.Context, scope domain.Scope, request domain.LinkRequest) (domain.Link, error) {
	if scope.IsZero() {
		return domain.Link{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if s.transport() == nil {
		return domain.Link{}, fmt.Errorf("%w: no gateway is reachable", domain.ErrNotConfigured)
	}
	if request.AgentID == uuid.Nil {
		return domain.Link{}, fmt.Errorf("%w: pilih dulu Bolu yang menjawab nomor ini", domain.ErrInvalidInput)
	}

	number, err := s.numberForLink(ctx, scope, request)
	if err != nil {
		return domain.Link{}, err
	}
	if number.Linked() {
		return domain.Link{}, fmt.Errorf("%w: nomor ini sudah tertaut", domain.ErrAlreadyLinked)
	}
	if number.TOSAcceptedAt == nil {
		return domain.Link{}, domain.ErrTermsNotAccepted
	}

	link, err := s.transport().Link(ctx, scope, number.ID, number.Phone)
	if err != nil {
		return domain.Link{}, err
	}

	// The row says a link is in progress, which is what the screen reads before
	// the first code arrives.
	if _, err := s.deps.Repository.SetStatus(ctx, scope, number.ID, domain.StatusLinking, ""); err != nil {
		return domain.Link{}, err
	}

	s.remind(link, number)
	return link, nil
}

// numberForLink finds or prepares the row this attempt works on.
//
// A number is created once, before it is linked, because the warning has to be
// accepted against something and because the Bolu that answers it is chosen at
// that moment rather than after the phone paired.
func (s *Service) numberForLink(ctx context.Context, scope domain.Scope, request domain.LinkRequest) (domain.Number, error) {
	phone := normalizePhone(request.Phone)
	if len(phone) < 8 {
		return domain.Number{}, fmt.Errorf("%w: %q bukan nomor telepon yang masuk akal", domain.ErrInvalidInput, request.Phone)
	}
	// A number written the local way starts with a zero, and WhatsApp needs the
	// country-coded form: pairing with "0812…" is refused by the server with an
	// error nobody can act on. Guessing the country would be worse than saying so —
	// this deployment serves one country today and would silently misread the next.
	if strings.HasPrefix(phone, "0") {
		return domain.Number{}, fmt.Errorf(
			"%w: tulis nomornya dengan kode negara, contoh 62812… bukan 0812…", domain.ErrInvalidInput)
	}

	existing, err := s.deps.Repository.ByPhone(ctx, scope, phone)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, domain.ErrNotFound):
		return domain.Number{}, err
	}

	label := strings.TrimSpace(request.Label)
	if label == "" {
		label = "WhatsApp " + maskPhone(phone)
	}

	number, err := s.deps.Repository.Create(ctx, scope, domain.Number{
		Phone:       phone,
		Label:       label,
		AgentID:     request.AgentID,
		Status:      domain.StatusUnlinked,
		HourlyLimit: s.deps.HourlyLimit,
	})
	if err != nil {
		return domain.Number{}, err
	}

	return number, nil
}

// AcceptTerms records that the workspace accepted the warning.
//
// It is what unblocks a link, so it is separate from linking rather than a flag
// on the same request: a screen shows the warning, a person reads it, and only
// then does the button do anything.
func (s *Service) AcceptTerms(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.AcceptTerms(ctx, scope, id)
	if err != nil {
		return domain.Number{}, err
	}

	s.audit(ctx, scope, "whatsapp.terms_accepted", number)
	return number, nil
}

// LinkState reports where an attempt stands, and finishes the link when the
// transport says the phone paired.
func (s *Service) LinkState(ctx context.Context, scope domain.Scope, linkID string) (domain.Link, error) {
	if scope.IsZero() {
		return domain.Link{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if s.transport() == nil {
		return domain.Link{}, fmt.Errorf("%w: no gateway is reachable", domain.ErrNotConfigured)
	}

	// An attempt whose *outcome* is already known answers from here: the transport
	// forgets an attempt the moment it ends, and a screen that polls one step behind
	// must still be told what happened rather than asked to start over.
	//
	// Only a terminal state is answered from memory. An attempt that is still
	// waiting or showing a code has to be asked about, or this shortcut would freeze
	// it at the state it was started in — which is exactly what it did, and it looked
	// like WhatsApp never sending a code.
	if remembered, ok := s.finished[linkID]; ok && terminalLinkState(remembered.link.State) {
		return remembered.link, nil
	}

	link, err := s.transport().LinkState(ctx, linkID)
	if err != nil {
		if errors.Is(err, domain.ErrLinkExpired) {
			return s.expired(linkID, "penautan ini sudah kedaluwarsa; minta kode baru")
		}
		return domain.Link{}, err
	}

	// The transport answers with the workspace it was started for, so an attempt
	// that belongs to another tenant is refused rather than reported: a person
	// must not watch, or finish, somebody else's link.
	if link.WorkspaceID != uuid.Nil && link.WorkspaceID != scope.WorkspaceID {
		return domain.Link{}, fmt.Errorf("%w: penautan itu milik workspace lain", domain.ErrNotFound)
	}

	switch link.State {
	case domain.LinkExpired, domain.LinkFailed:
		// The row goes back to what it was: a link that did not happen is not a
		// number that is broken.
		if link.NumberID != uuid.Nil {
			if _, err := s.deps.Repository.SetStatus(ctx, scope, link.NumberID, domain.StatusUnlinked, link.Reason); err != nil {
				s.deps.Logger.Warn("could not clear a failed link's status", zap.Error(err))
			}
		}
		s.remember(domain.Link{ID: linkID, State: link.State, Reason: link.Reason})
	case domain.LinkPaired:
		s.remember(link)
	default:
		s.remember(link)
	}

	return link, nil
}

// PairCode asks for the phone-code alternative to scanning.
func (s *Service) PairCode(ctx context.Context, scope domain.Scope, linkID string, phone string) (domain.Link, error) {
	if scope.IsZero() {
		return domain.Link{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if s.transport() == nil {
		return domain.Link{}, fmt.Errorf("%w: no gateway is reachable", domain.ErrNotConfigured)
	}

	link, err := s.transport().PairCode(ctx, linkID, phone)
	if err != nil {
		return domain.Link{}, err
	}

	s.remember(link)
	return link, nil
}

// CancelLink abandons an attempt.
func (s *Service) CancelLink(ctx context.Context, scope domain.Scope, linkID string) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if s.transport() == nil {
		return nil
	}

	if err := s.transport().Cancel(ctx, linkID); err != nil {
		return err
	}

	// The row is left unlinked rather than failed: cancelling is a person
	// changing their mind, and the next attempt starts from the same row.
	if remembered, ok := s.forget(linkID); ok && remembered.NumberID != uuid.Nil {
		if _, err := s.deps.Repository.SetStatus(ctx, scope, remembered.NumberID, domain.StatusUnlinked, ""); err != nil {
			return err
		}
	}

	return nil
}

// Paired is what the transport calls when a phone finished pairing.
//
// It is the moment an attempt becomes a number that works: the connection the
// hub shows is created, the number is marked connected, and the fleet directory
// is told to serve it.
func (s *Service) Paired(ctx context.Context, scope domain.Scope, paired domain.PairedLink) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.Get(ctx, scope, paired.NumberID)
	if err != nil {
		return err
	}

	phone := normalizePhone(paired.Phone)
	if phone == "" {
		phone = number.Phone
	}

	// The number WhatsApp reported is written onto the row, because it is the
	// number this session answers as. What the person typed was a hint: somebody
	// writes "0812…" and WhatsApp says "62812…", and the row has to be the second
	// or every later match against it is off by a country code.
	if phone != number.Phone {
		if err := s.deps.Repository.SetPhone(ctx, scope, number.ID, phone); err != nil {
			return err
		}
		number.Phone = phone
	}

	number, err = s.linkConnection(ctx, scope, number, phone, paired.JID)
	if err != nil {
		return err
	}

	number, err = s.deps.Repository.SetStatus(ctx, scope, number.ID, domain.StatusConnected, "")
	if err != nil {
		return err
	}

	if err := s.deps.Repository.Publish(ctx, scope, number.ID, true); err != nil {
		return err
	}

	s.audit(ctx, scope, "whatsapp.linked", number)
	s.deps.Logger.Info("a whatsapp number is linked",
		zap.String("number", number.ID.String()),
		zap.String("phone", maskPhone(number.Phone)),
	)

	return nil
}

// linkConnection records the number as a connection in the hub, creating it the
// first time and re-labelling it afterwards.
//
// The handle is the number's own id, which is what the tool call carries back:
// the credential a send needs is "speak as this number", and the hub seals it
// like any other.
func (s *Service) linkConnection(ctx context.Context, scope domain.Scope, number domain.Number, phone, _ string) (domain.Number, error) {
	if s.deps.Connections == nil {
		return number, nil
	}

	integrationID, err := s.deps.Connections.Link(ctx, scope, domain.ConnectionLink{
		App:          App,
		AccountLabel: maskPhone(phone),
		Handle:       number.ID.String(),
	})
	if err != nil {
		return domain.Number{}, err
	}

	// The integration id goes in through its own statement, because it is a link
	// the hub owns rather than a field a person edits: rewriting the label and
	// the ceiling here would be a second write of values that did not change.
	if err := s.deps.Repository.SetIntegration(ctx, scope, number.ID, integrationID); err != nil {
		return domain.Number{}, err
	}

	number.IntegrationID = integrationID
	return number, nil
}

// AssignAgent chooses which Bolu answers this number.
func (s *Service) AssignAgent(ctx context.Context, scope domain.Scope, id uuid.UUID, agentID uuid.UUID) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.Number{}, err
	}

	updated, err := s.deps.Repository.Update(ctx, scope, domain.Number{
		ID:          number.ID,
		Label:       number.Label,
		AgentID:     agentID,
		HourlyLimit: number.HourlyLimit,
	})
	if err != nil {
		return domain.Number{}, err
	}

	s.audit(ctx, scope, "whatsapp.agent_assigned", updated)
	return updated, nil
}

// SetLimit changes the number's hourly ceiling.
func (s *Service) SetLimit(ctx context.Context, scope domain.Scope, id uuid.UUID, limit int) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if limit < 0 || limit > 500 {
		return domain.Number{}, fmt.Errorf("%w: batas per jam harus antara 0 dan 500", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.Number{}, err
	}

	updated, err := s.deps.Repository.Update(ctx, scope, domain.Number{
		ID:          number.ID,
		Label:       number.Label,
		AgentID:     number.AgentID,
		HourlyLimit: limit,
	})
	if err != nil {
		return domain.Number{}, err
	}

	s.audit(ctx, scope, "whatsapp.limit_changed", updated)
	return updated, nil
}

// SetLabel renames the number as the workspace refers to it.
func (s *Service) SetLabel(ctx context.Context, scope domain.Scope, id uuid.UUID, label string) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	label = strings.TrimSpace(label)
	if label == "" {
		return domain.Number{}, fmt.Errorf("%w: nama nomor tidak boleh kosong", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.Number{}, err
	}

	updated, err := s.deps.Repository.Update(ctx, scope, domain.Number{
		ID:          number.ID,
		Label:       label,
		AgentID:     number.AgentID,
		HourlyLimit: number.HourlyLimit,
	})
	if err != nil {
		return domain.Number{}, err
	}

	s.audit(ctx, scope, "whatsapp.renamed", updated)
	return updated, nil
}

// Unlink ends the session and clears what was stored.
//
// The message history stays: what a customer wrote is not the session's, and
// losing it because a phone was replaced would erase the record of what the
// workspace was told.
func (s *Service) Unlink(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.Get(ctx, scope, id)
	if err != nil {
		return domain.Number{}, err
	}

	if s.transport() != nil {
		if err := s.transport().Unlink(ctx, domain.Snapshot{
			NumberID:    number.ID,
			WorkspaceID: scope.WorkspaceID,
			Phone:       number.Phone,
		}); err != nil && !errors.Is(err, domain.ErrNotConnected) {
			return domain.Number{}, err
		}
	}

	if err := s.deps.Sessions.ForgetSession(ctx, scope, number.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Number{}, err
	}
	if err := s.deps.Repository.Publish(ctx, scope, number.ID, false); err != nil {
		return domain.Number{}, err
	}

	if s.deps.Connections != nil && number.IntegrationID != uuid.Nil {
		if err := s.deps.Connections.Disconnect(ctx, scope, number.IntegrationID); err != nil {
			return domain.Number{}, err
		}
	}

	updated, err := s.deps.Repository.SetStatus(ctx, scope, number.ID, domain.StatusDisconnected, "dinonaktifkan")
	if err != nil {
		return domain.Number{}, err
	}

	s.audit(ctx, scope, "whatsapp.unlinked", updated)
	return updated, nil
}

// Report records what the transport knows about a number.
func (s *Service) Report(ctx context.Context, scope domain.Scope, request domain.ReportRequest) (domain.Number, error) {
	if scope.IsZero() {
		return domain.Number{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if !knownStatus(request.Status) {
		return domain.Number{}, fmt.Errorf("%w: unknown status %q", domain.ErrInvalidInput, request.Status)
	}

	number, err := s.deps.Repository.Get(ctx, scope, request.NumberID)
	if err != nil {
		return domain.Number{}, err
	}

	// A report that says nothing new does not move the row: the transport
	// reports on a timer, and rewriting the status every tick would be a write
	// per number per interval for no change.
	if number.Status == request.Status && number.StatusReason == request.Reason {
		if request.Status == domain.StatusConnected {
			if err := s.deps.Repository.Touch(ctx, scope, number.ID, request.At); err != nil {
				return domain.Number{}, err
			}
			number.LastSeenAt = &request.At
		}
		return number, nil
	}

	updated, err := s.deps.Repository.SetStatus(ctx, scope, number.ID, request.Status, request.Reason)
	if err != nil {
		return domain.Number{}, err
	}
	if request.Status == domain.StatusConnected {
		if err := s.deps.Repository.Touch(ctx, scope, number.ID, request.At); err != nil {
			return domain.Number{}, err
		}
		updated.LastSeenAt = &request.At
	}

	// Only a status a person has to act on alerts them. A number that dropped
	// and reconnected by itself is worth the live stream and not an email.
	switch request.Status {
	case domain.StatusNeedsRelink, domain.StatusDisconnected:
		s.alert(ctx, scope, updated, request.Status)
	case domain.StatusConnected:
		s.emit(ctx, scope, "whatsapp.status", updated)
	}

	if err := s.deps.Repository.Publish(ctx, scope, updated.ID, updated.Linked()); err != nil {
		s.deps.Logger.Warn("could not publish a number's status to the fleet", zap.Error(err))
	}

	return updated, nil
}

// alert tells the workspace's owners that a number needs them.
func (s *Service) alert(ctx context.Context, scope domain.Scope, number domain.Number, status string) {
	kind := domain.AlertDisconnected
	if status == domain.StatusNeedsRelink {
		kind = domain.AlertNeedsRelink
	}

	s.emit(ctx, scope, "whatsapp."+status, number)

	if s.deps.Alerter == nil {
		return
	}
	if err := s.deps.Alerter.Alert(ctx, scope, domain.Alert{
		Kind:        kind,
		NumberID:    number.ID,
		Phone:       number.Phone,
		Status:      status,
		Reason:      number.StatusReason,
		WorkspaceID: scope.WorkspaceID,
	}); err != nil {
		s.deps.Logger.Warn("could not alert the owners about a number", zap.Error(err))
	}
}

// audit records what a person did to a number, for the trail the product answers
// "who turned this off" with.
func (s *Service) audit(ctx context.Context, scope domain.Scope, action string, number domain.Number) {
	s.emit(ctx, scope, action, number)
}

// emit publishes one event into the workspace's live stream.
func (s *Service) emit(ctx context.Context, scope domain.Scope, kind string, number domain.Number) {
	if s.deps.Activity == nil {
		return
	}

	payload := map[string]any{
		"number_id": number.ID.String(),
		"phone":     maskPhone(number.Phone),
		"label":     number.Label,
		"status":    number.Status,
	}
	if number.StatusReason != "" {
		payload["reason"] = number.StatusReason
	}
	if number.AgentID != uuid.Nil {
		payload["agent_id"] = number.AgentID.String()
	}

	if err := s.deps.Activity.Emit(ctx, domain.Event{
		WorkspaceID: scope.WorkspaceID,
		Type:        kind,
		Payload:     payload,
	}); err != nil {
		s.deps.Logger.Warn("could not publish a whatsapp event", zap.Error(err))
	}
}

// remember keeps a link attempt's state briefly, so a screen polling one step
// behind sees the outcome rather than an expired attempt.
func (s *Service) remember(link domain.Link) {
	if link.ID == "" {
		return
	}

	// A sweep on every write keeps the map from growing with attempts nobody
	// asked about again. It is O(len(finished)) and len is bounded by how many
	// people are linking at once.
	now := s.now()
	for id, entry := range s.finished {
		if entry.forgetAt.Before(now) {
			delete(s.finished, id)
		}
	}

	s.finished[link.ID] = finishedLink{link: link, forgetAt: now.Add(linkStateTTL)}
}

// forget removes a remembered attempt and answers it.
func (s *Service) forget(linkID string) (domain.Link, bool) {
	entry, ok := s.finished[linkID]
	if !ok {
		return domain.Link{}, false
	}
	delete(s.finished, linkID)
	return entry.link, true
}

// expired answers with a reason a person can act on: the answer is a new code, not
// a failure to read.
func (s *Service) expired(linkID, reason string) (domain.Link, error) {
	link := domain.Link{ID: linkID, State: domain.LinkExpired, Reason: reason}
	s.remember(link)
	return link, nil
}

// remind records an attempt's first state so a poll before the first code
// arrives answers with the attempt rather than with "gone".
func (s *Service) remind(link domain.Link, number domain.Number) {
	link.NumberID = number.ID
	link.WorkspaceID = number.WorkspaceID
	link.Phone = number.Phone
	s.remember(link)
}

// terminalLinkState reports whether an attempt has ended, either way.
func terminalLinkState(state string) bool {
	switch state {
	case domain.LinkPaired, domain.LinkExpired, domain.LinkFailed:
		return true
	default:
		return false
	}
}

// knownStatus reports whether a status is one this module uses.
func knownStatus(status string) bool {
	switch status {
	case domain.StatusUnlinked, domain.StatusLinking, domain.StatusConnected,
		domain.StatusNeedsRelink, domain.StatusDisconnected:
		return true
	default:
		return false
	}
}

// normalizePhone reduces a typed number to the digits WhatsApp wants.
func normalizePhone(phone string) string {
	var out strings.Builder
	for _, char := range phone {
		if char >= '0' && char <= '9' {
			out.WriteRune(char)
		}
	}
	return out.String()
}

// maskPhone hides all but the last four digits.
func maskPhone(phone string) string {
	trimmed := strings.TrimSpace(phone)
	if len(trimmed) <= 4 {
		return strings.Repeat("•", len(trimmed))
	}
	return strings.Repeat("•", len(trimmed)-4) + trimmed[len(trimmed)-4:]
}

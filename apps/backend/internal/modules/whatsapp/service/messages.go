package service

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// The message half: what arrived, what is waiting to leave, and how the two are
// paced.

// Receive records one inbound message and answers whether it was new.
//
// The order is what makes the path idempotent rather than merely careful: the
// message is recorded first, and only a message this call created opens anything.
// A redelivery — which WhatsApp does on purpose when an acknowledgement is
// slow — finds its own row and stops there.
func (s *Service) Receive(ctx context.Context, scope domain.Scope, numberID uuid.UUID, message domain.Inbound) (domain.Inbound, bool, error) {
	if scope.IsZero() {
		return domain.Inbound{}, false, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if strings.TrimSpace(message.WaMessageID) == "" {
		return domain.Inbound{}, false, fmt.Errorf("%w: a message needs whatsapp's own id", domain.ErrInvalidInput)
	}
	if message.ReceivedAt.IsZero() {
		message.ReceivedAt = s.now()
	}
	message.NumberID = numberID
	message.Kind = kindOr(message.Kind)

	number, err := s.deps.Repository.Get(ctx, scope, numberID)
	if err != nil {
		return domain.Inbound{}, false, err
	}

	stored, created, err := s.deps.Repository.Record(ctx, scope, message)
	if err != nil {
		return domain.Inbound{}, false, err
	}
	if !created {
		return stored, false, nil
	}

	s.emit(ctx, scope, "whatsapp.message_received", number)

	// The event is announced before anything is opened, so a routine watching it
	// gets the message whether or not a Bolu is assigned to the number. A routine
	// names the Bolu that should act, which is a better answer than a rule
	// invented here.
	started := 0
	if s.deps.Watchers != nil {
		started, err = s.deps.Watchers.Fire(ctx, scope, domain.EventMessageReceived)
		if err != nil {
			// A routine that could not start is reported and not fatal: the
			// message is recorded either way, and the fallback below still gives
			// it a reader.
			s.deps.Logger.Warn("could not start the routines watching whatsapp messages", zap.Error(err))
		}
	}
	if started > 0 {
		return stored, true, nil
	}

	// Nothing is watching, so the Bolu assigned to this number answers.
	if stored.IsFromMe || stored.IsGroup {
		// A group is not a customer writing to the business, and a message this
		// number sent is not one to answer. Both are recorded — what was said is
		// what a person reads when they ask — and neither opens work.
		return stored, true, nil
	}
	if number.AgentID == uuid.Nil {
		return stored, true, fmt.Errorf("%w: nomor ini belum punya Bolu yang menjawab", domain.ErrNoAgent)
	}
	if s.deps.Replies == nil {
		return stored, true, nil
	}

	taskID, err := s.openTask(ctx, scope, number, stored)
	if err != nil {
		// A task that could not be opened is reported rather than hidden, and the
		// message keeps its place: a person reading the history still sees what
		// the customer wrote.
		s.deps.Logger.Warn("could not open a task for an inbound message",
			zap.String("message", stored.WaMessageID), zap.Error(err))
		return stored, true, err
	}

	if err := s.deps.Repository.AttachTask(ctx, scope, stored.ID, taskID); err != nil {
		s.deps.Logger.Warn("could not link a message to its task", zap.Error(err))
		return stored, true, err
	}
	stored.TaskID = taskID

	return stored, true, nil
}

// openTask hands one customer message to the Bolu that answers the number.
func (s *Service) openTask(ctx context.Context, scope domain.Scope, number domain.Number, message domain.Inbound) (uuid.UUID, error) {
	title := "Pesan WhatsApp dari " + maskPhone(number.Phone)
	if message.SenderName != "" {
		title = "Pesan WhatsApp dari " + message.SenderName
	}

	return s.deps.Replies.Answer(ctx, scope, domain.AnswerRequest{
		AgentID:  number.AgentID,
		NumberID: number.ID,
		Phone:    number.Phone,
		Name:     message.SenderName,
		Title:    title,
		Prompt:   promptFor(number, message),
	})
}

// promptFor is what the Bolu is told.
//
// The customer's own words are the instruction, and the number it came from is
// the reply address. Both are stated plainly because the task has no other way
// to know them: it runs out of process with an agent, a title, and this text.
func promptFor(number domain.Number, message domain.Inbound) string {
	var out strings.Builder

	out.WriteString("Seorang pelanggan menulis ke nomor WhatsApp ")
	out.WriteString(maskPhone(number.Phone))
	out.WriteString(".\n\n")

	if message.SenderName != "" {
		out.WriteString("Nama yang dipakai pengirim: ")
		out.WriteString(message.SenderName)
		out.WriteString("\n")
	}
	out.WriteString("Nomor pengirim: +")
	out.WriteString(strings.TrimPrefix(message.ChatJID, ""))
	out.WriteString("\n\n")

	switch message.Kind {
	case domain.KindText:
		out.WriteString("Isi pesannya:\n\n")
		out.WriteString(message.Text)
	case domain.KindOther:
		out.WriteString("Pesan ini bukan teks dan tidak bisa dibaca di sini.\n")
	default:
		out.WriteString("Pelanggan mengirim sebuah ")
		out.WriteString(message.Kind)
		if message.Text != "" {
			out.WriteString(" dengan keterangan:\n\n")
			out.WriteString(message.Text)
		} else {
			out.WriteString(".\n")
		}
		switch message.Media.Status {
		case domain.MediaReady:
			out.WriteString("\nLampirannya sudah tersimpan dan bisa dibaca lewat alat yang tersedia.")
		case domain.MediaFailed:
			out.WriteString("\nLampirannya gagal diunduh: ")
			out.WriteString(message.Media.Error)
		case domain.MediaPending:
			out.WriteString("\nLampirannya sedang diunduh.")
		}
	}

	out.WriteString("\n\nBalas pelanggan ini dengan alat `whatsapp.send_message`, ke nomor pengirim di atas. ")
	out.WriteString("Balasan itu keluar lewat gerbang persetujuan: tulis drafnya, jangan mengirim sendiri.")

	return out.String()
}

// Messages returns what customers wrote, newest first.
func (s *Service) Messages(ctx context.Context, scope domain.Scope, filter domain.MessageFilter) ([]domain.Inbound, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.Messages(ctx, scope, filter)
}

// Outbox returns what the workspace asked one number to send, newest first.
func (s *Service) Outbox(ctx context.Context, scope domain.Scope, filter domain.OutboxFilter) ([]domain.Outbox, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.Outbox(ctx, scope, filter)
}

// NumberByIntegration answers which number a connection speaks as.
//
// It is what a tool call needs: the model is given a connection to act on, and
// the module is the only thing that knows which number that connection is.
func (s *Service) NumberByIntegration(ctx context.Context, scope domain.Scope, integrationID uuid.UUID) (uuid.UUID, error) {
	if scope.IsZero() {
		return uuid.Nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if integrationID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: no connection was named", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.ByIntegration(ctx, scope, integrationID)
	if err != nil {
		return uuid.Nil, err
	}

	return number.ID, nil
}

// History returns one conversation, oldest first.
//
// The repository reads newest first because that is what a screen wants; a
// conversation handed to a model reads the other way round, so it is reversed
// here rather than in the query. Two orderings of one read would be two places
// to get the answer wrong.
func (s *Service) History(ctx context.Context, scope domain.Scope, integrationID uuid.UUID, chatJID string, limit int) ([]domain.Inbound, error) {
	numberID, err := s.NumberByIntegration(ctx, scope, integrationID)
	if err != nil {
		return nil, err
	}

	if limit <= 0 || limit > 100 {
		limit = 20
	}

	chat := strings.TrimSpace(chatJID)
	if chat != "" {
		chat = normalizeJID(chat)
	}

	newest, err := s.deps.Repository.Messages(ctx, scope, domain.MessageFilter{
		NumberID: numberID,
		ChatJID:  chat,
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}

	oldest := make([]domain.Inbound, 0, len(newest))
	for index := len(newest) - 1; index >= 0; index-- {
		oldest = append(oldest, newest[index])
	}
	return oldest, nil
}

// Enqueue records one message to send.
//
// It is what the integration adapter calls when an approved action reaches
// WhatsApp, so nothing in this module decides that a message should go out.
func (s *Service) Enqueue(ctx context.Context, scope domain.Scope, request domain.EnqueueRequest) (domain.Outbox, error) {
	if scope.IsZero() {
		return domain.Outbox{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	number, err := s.deps.Repository.Get(ctx, scope, request.NumberID)
	if err != nil {
		return domain.Outbox{}, err
	}
	if !number.Linked() {
		return domain.Outbox{}, fmt.Errorf("%w: nomor %s belum tertaut", domain.ErrNotConnected, maskPhone(number.Phone))
	}

	to := normalizeJID(request.ToJID)
	if to == "" {
		return domain.Outbox{}, fmt.Errorf("%w: pesan tidak menyebut nomor tujuan", domain.ErrInvalidInput)
	}

	kind := request.Kind
	if kind == "" {
		kind = domain.KindText
	}
	if !knownOutgoingKind(kind) {
		return domain.Outbox{}, fmt.Errorf("%w: jenis pesan %q tidak dikenal", domain.ErrInvalidInput, kind)
	}
	if strings.TrimSpace(request.Text) == "" && strings.TrimSpace(request.MediaKey) == "" {
		return domain.Outbox{}, fmt.Errorf("%w: pesan kosong", domain.ErrInvalidInput)
	}

	stored, err := s.deps.Repository.Enqueue(ctx, scope, domain.Outbox{
		NumberID: number.ID,
		ToJID:    to,
		Kind:     kind,
		Text:     request.Text,
		MediaKey: request.MediaKey,
		DraftID:  request.DraftID,
		Status:   domain.OutboxQueued,
		// Nothing leaves immediately: the gap before the first message of a batch
		// is what keeps a burst from leaving in one second.
		NotBefore: s.now().Add(s.firstGap()),
	})
	if err != nil {
		return domain.Outbox{}, err
	}

	s.deps.Logger.Info("a whatsapp message is queued",
		zap.String("number", number.ID.String()),
		zap.String("message", stored.ID.String()),
	)

	return stored, nil
}

// Claim returns the messages of one number that may go out now.
//
// The hourly ceiling is checked here rather than by the sender, and what it does
// when it is reached is the point: the due messages are pushed to later rather
// than parked as failed, because a limit is a pace and a customer's answer is
// still owed.
func (s *Service) Claim(ctx context.Context, scope domain.Scope, numberID uuid.UUID, limit int) (domain.Claimed, error) {
	if scope.IsZero() {
		return domain.Claimed{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if limit <= 0 {
		limit = 5
	}

	number, err := s.deps.Repository.Get(ctx, scope, numberID)
	if err != nil {
		return domain.Claimed{}, err
	}

	ceiling := number.HourlyLimit
	if ceiling <= 0 {
		ceiling = s.deps.HourlyLimit
	}

	sent, err := s.deps.Repository.SentSince(ctx, scope, numberID, s.now().Add(-time.Hour))
	if err != nil {
		return domain.Claimed{}, err
	}

	remaining := ceiling - sent
	if remaining <= 0 {
		// Everything due waits. It is reported so a person can be told their
		// message is paced rather than lost, and the alert fires once per hold
		// rather than once per message.
		held, err := s.holdDue(ctx, scope, numberID, limit)
		if err != nil {
			return domain.Claimed{}, err
		}
		if held > 0 {
			s.alertLimit(ctx, scope, number, ceiling, held)
		}
		return domain.Claimed{Held: held}, nil
	}

	if limit > remaining {
		limit = remaining
	}

	ready, err := s.deps.Repository.Due(ctx, scope, numberID, limit)
	if err != nil {
		return domain.Claimed{}, err
	}

	return domain.Claimed{Ready: ready}, nil
}

// holdDue pushes one number's due messages to later and answers how many.
func (s *Service) holdDue(ctx context.Context, scope domain.Scope, numberID uuid.UUID, limit int) (int, error) {
	due, err := s.deps.Repository.Due(ctx, scope, numberID, limit)
	if err != nil {
		return 0, err
	}

	held := 0
	for _, item := range due {
		if err := s.deps.Repository.MarkHeld(ctx, scope, item.ID,
			"batas kirim per jam tercapai", s.now().Add(holdDuration)); err != nil {
			return held, err
		}
		held++
	}
	return held, nil
}

// alertLimit tells the workspace that a number is paced.
func (s *Service) alertLimit(ctx context.Context, scope domain.Scope, number domain.Number, ceiling, held int) {
	s.deps.Logger.Info("a whatsapp number reached its hourly ceiling",
		zap.String("number", number.ID.String()),
		zap.Int("ceiling", ceiling),
		zap.Int("held", held),
	)

	s.emit(ctx, scope, "whatsapp.limit_reached", number)

	if s.deps.Alerter == nil {
		return
	}
	if err := s.deps.Alerter.Alert(ctx, scope, domain.Alert{
		Kind:        domain.AlertLimit,
		NumberID:    number.ID,
		Phone:       number.Phone,
		Status:      number.Status,
		Reason:      fmt.Sprintf("batas %d pesan per jam tercapai; %d pesan menunggu", ceiling, held),
		WorkspaceID: scope.WorkspaceID,
	}); err != nil {
		s.deps.Logger.Warn("could not report a paced number", zap.Error(err))
	}
}

// Sent records that a message left.
func (s *Service) Sent(ctx context.Context, scope domain.Scope, id uuid.UUID, waMessageID string, at time.Time) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.MarkSent(ctx, scope, id, waMessageID, at)
}

// Retry puts a message back in the queue with a later time.
func (s *Service) Retry(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string, at time.Time) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.MarkRetry(ctx, scope, id, reason, at)
}

// Failed parks a message that will not be retried, so it stays visible.
func (s *Service) Failed(ctx context.Context, scope domain.Scope, id uuid.UUID, reason string, attempts int) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	if err := s.deps.Repository.MarkFailed(ctx, scope, id, reason, attempts); err != nil {
		return err
	}

	s.deps.Logger.Warn("a whatsapp message was not delivered",
		zap.String("message", id.String()),
		zap.String("reason", reason),
	)
	return nil
}

// Reclaim puts messages a dead gateway left mid-send back in the queue.
func (s *Service) Reclaim(ctx context.Context, scope domain.Scope, olderThan time.Time) (int, error) {
	if scope.IsZero() {
		return 0, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.Reclaim(ctx, scope, olderThan)
}

// PendingMedia returns the messages whose bytes have not been fetched.
func (s *Service) PendingMedia(ctx context.Context, scope domain.Scope, limit int) ([]domain.Inbound, error) {
	if scope.IsZero() {
		return nil, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return s.deps.Repository.PendingMedia(ctx, scope, limit)
}

// StoreMedia records where a message's bytes were stored, or that they could
// not be.
func (s *Service) StoreMedia(ctx context.Context, scope domain.Scope, id uuid.UUID, media domain.Media) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if !knownMediaStatus(media.Status) {
		return fmt.Errorf("%w: unknown media status %q", domain.ErrInvalidInput, media.Status)
	}

	return s.deps.Repository.SetMedia(ctx, scope, id, media)
}

// firstGap is the pause before the first message of a batch.
//
// It is short and randomised for the same reason the gap between two messages
// is: a batch that leaves in the same instant it was approved reads as a machine
// posting, and a number that reads that way is the one that gets restricted.
func (s *Service) firstGap() time.Duration {
	return 2*time.Second + randomGap(3*time.Second)
}

// randomGap draws a pause below a ceiling, so the time a message waits is not the
// same every time. A fixed interval is a pattern, and a pattern is what makes a
// batch of answers look automated.
func randomGap(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling)))
}

// knownOutgoingKind reports whether this deployment can send that kind.
func knownOutgoingKind(kind string) bool {
	switch kind {
	case domain.KindText, domain.KindImage, domain.KindDocument:
		return true
	default:
		return false
	}
}

// kindOr defaults a message's kind.
func kindOr(kind string) string {
	if strings.TrimSpace(kind) == "" {
		return domain.KindOther
	}
	return kind
}

// knownMediaStatus reports whether a media status is one this module uses.
func knownMediaStatus(status string) bool {
	switch status {
	case domain.MediaPending, domain.MediaReady, domain.MediaFailed, "":
		return true
	default:
		return false
	}
}

// normalizeJID turns a phone number or an address into a WhatsApp address.
//
// A person writing a reply says "+62812…", and WhatsApp addresses are
// "62812…@s.whatsapp.net". The conversion is here rather than in the adapter
// because it is a product convenience: what the model writes is a number, and
// what the protocol needs is an address.
func normalizeJID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "@") {
		return trimmed
	}

	digits := normalizePhone(trimmed)
	if digits == "" {
		return ""
	}
	return digits + "@s.whatsapp.net"
}

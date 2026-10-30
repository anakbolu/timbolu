package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/zap"

	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The transport half: what the product asks WhatsApp to do.
//
// Everything here is a thin translation. The decisions — who may link a number,
// who answers it, whether a message may leave — are the module's, and this file
// only carries them out. What it does own is the pairing flow's own mechanics:
// the QR code rotates on a timer, the code expires, and both have to become a
// state a screen can show rather than an error.

// Compile-time check that the engine is the Transport the module declares.
var _ whatsappdomain.Transport = (*Engine)(nil)

// pairCodeLength is how long a pairing code is. WhatsApp's own is eight
// characters; a shorter one is refused by the server.
const pairCodeLength = 8

// Link starts an attempt to link one number.
//
// The pair attempt is held in memory rather than in the database because it is
// not state: it is a socket, a QR code, and a few minutes of a person's
// attention. What survives is the session, and that is written when the phone
// pairs.
func (e *Engine) Link(_ context.Context, scope whatsappdomain.Scope, numberID uuid.UUID, phone string) (whatsappdomain.Link, error) {
	if scope.IsZero() {
		return whatsappdomain.Link{}, fmt.Errorf("%w: workspace is required", whatsappdomain.ErrInvalidInput)
	}
	if e.deps.Links == nil {
		return whatsappdomain.Link{}, fmt.Errorf("%w: no link sink is configured", whatsappdomain.ErrNotConfigured)
	}

	// A number that is already served here must not be re-linked underneath its
	// running client, which would leave two devices competing for one session.
	if runtime := e.runtimeFor(numberID); runtime != nil {
		return whatsappdomain.Link{}, fmt.Errorf("%w: the number is already connected", whatsappdomain.ErrAlreadyLinked)
	}

	attempt, err := e.newAttempt(scope, numberID, phone)
	if err != nil {
		return whatsappdomain.Link{}, err
	}

	// The channel is bound to the attempt's own lifetime, not the request's, and
	// that is not a detail: the request that starts a pairing returns as soon as the
	// attempt exists — the QR arrives later, on its own socket — so a channel tied to
	// the request would be cancelled before WhatsApp ever sent a code, and the
	// attempt would sit waiting for something that could no longer arrive.
	qrc, err := attempt.client.GetQRChannel(attempt.ctx)
	if err != nil {
		attempt.cancel()
		return whatsappdomain.Link{}, fmt.Errorf("whatsapp: start pairing: %w", err)
	}
	if err := attempt.client.Connect(); err != nil {
		attempt.cancel()
		// A socket that cannot be dialled is the service being unreachable, which
		// is a different answer from a request that was wrong: a person should be
		// told to try again rather than to check what they typed.
		return whatsappdomain.Link{}, fmt.Errorf("%w: %w", whatsappdomain.ErrUnavailable, err)
	}

	go e.watchPairing(scope, attempt, qrc)

	return attempt.snapshot(), nil
}

// newAttempt builds a device and a client for one link.
func (e *Engine) newAttempt(scope whatsappdomain.Scope, numberID uuid.UUID, phone string) (*linkAttempt, error) {
	attemptCtx, cancel := context.WithCancel(context.Background())

	device, inner := newDevice(numberID, scope.WorkspaceID, e.waLog, e.persister(scope, numberID))
	client := whatsmeow.NewClient(device, e.waLog)

	attempt := &linkAttempt{
		id:          "link-" + uuid.NewString(),
		numberID:    numberID,
		workspaceID: scope.WorkspaceID,
		phone:       phone,
		device:      device,
		store:       inner,
		client:      client,
		cancel:      cancel,
		state:       whatsappdomain.LinkWaiting,
	}

	client.AddEventHandler(func(raw any) {
		switch event := raw.(type) {
		case *events.LoggedOut:
			reason := "WhatsApp menolak penautan ini"
			if event.OnConnect {
				reason = "WhatsApp menolak penautan ini saat menyambung"
			}
			attempt.fail(reason)
		}
	})

	e.mu.Lock()
	e.links[attempt.id] = attempt
	e.mu.Unlock()

	attempt.ctx = attemptCtx

	return attempt, nil
}

// watchPairing follows one pairing to its end.
//
// The first code has a deadline of its own, and it is the reason this is a select
// rather than a range: a connection that stalls during the handshake emits nothing
// at all, and a screen waiting on it would show "menyiapkan kode…" forever. Waiting
// is not an answer a person can act on, so a stalled attempt ends the way an expired
// code does — and the screen offers a new one.
func (e *Engine) watchPairing(scope whatsappdomain.Scope, attempt *linkAttempt, qrc <-chan whatsmeow.QRChannelItem) {
	defer e.forgetLink(attempt.id)

	// The deadline is longer than WhatsApp's own handshake takes and short enough
	// that somebody watching the screen is not left guessing.
	deadline := time.NewTimer(attemptDeadline)
	defer deadline.Stop()

	for {
		select {
		case <-deadline.C:
			if attempt.snapshot().State == whatsappdomain.LinkWaiting {
				attempt.fail("WhatsApp tidak mengirim kode; coba lagi")
				if attempt.client != nil {
					attempt.client.Disconnect()
				}
				return
			}
			// A code arrived, and WhatsApp rotates its own from here: the next
			// deadline is the server's, delivered with each item.
		case <-attempt.ctx.Done():
			return
		case item, ok := <-qrc:
			if !ok {
				// The channel closed without a verdict. That happens when the
				// emitter ran out of codes or the socket went away, and the state
				// says what a person should do either way.
				if attempt.snapshot().State == whatsappdomain.LinkWaiting {
					attempt.fail("WhatsApp menutup penautan ini; coba lagi")
				}
				return
			}
			if e.handleQRItem(scope, attempt, item) {
				return
			}
		}
	}
}

// attemptDeadline bounds how long a pairing may sit without producing a code.
const attemptDeadline = 20 * time.Second

// handleQRItem applies one item and answers whether the attempt is over.
func (e *Engine) handleQRItem(scope whatsappdomain.Scope, attempt *linkAttempt, item whatsmeow.QRChannelItem) bool {
	// The end-of-stream markers are whole items rather than event names, so the
	// comparison is against the item and the code path against its event.
	switch item {
	case whatsmeow.QRChannelSuccess:
		e.finishPairing(scope, attempt)
		return true
	case whatsmeow.QRChannelTimeout:
		attempt.fail("kode kedaluwarsa sebelum dipindai; minta kode baru")
		return true
	case whatsmeow.QRChannelClientOutdated:
		attempt.fail("versi WhatsApp web ini sudah tidak didukung; perbarui wa-gateway")
		return true
	case whatsmeow.QRChannelScannedWithoutMultidevice:
		attempt.fail("nomor itu belum mengaktifkan perangkat tertaut")
		return true
	}

	switch item.Event {
	case whatsmeow.QRChannelEventCode:
		// A new code replaces the previous one, which is what makes an expired QR
		// a new QR rather than a dead end: WhatsApp rotates them by itself, and the
		// screen follows.
		attempt.setCode(item.Code, item.Timeout)
	case whatsmeow.QRChannelEventError:
		if item.Error != nil {
			attempt.fail("penautan gagal: " + item.Error.Error())
		} else {
			attempt.fail("penautan gagal")
		}
		return true
	case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
		// A passkey prompt is a flow this deployment does not offer, and leaving
		// the attempt open would show a QR nobody can use.
		attempt.fail("WhatsApp meminta passkey, yang belum didukung di sini")
		return true
	}

	return false
}

// finishPairing stores the session and tells the module.
func (e *Engine) finishPairing(scope whatsappdomain.Scope, attempt *linkAttempt) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	attempt.client.Disconnect()

	if attempt.device.ID == nil {
		attempt.fail("WhatsApp menyelesaikan penautan tanpa memberi identitas nomor")
		return
	}

	// The session is written before the module is told, so a crash between the
	// two leaves a stored session the fleet can serve rather than a number that
	// claims to be linked with nothing behind it.
	if err := attempt.store.flush(ctx, true); err != nil {
		attempt.fail("sesi tidak bisa disimpan: " + err.Error())
		return
	}

	jid := attempt.device.ID.String()
	phone := phoneFromJID(*attempt.device.ID)
	if phone == "" {
		phone = attempt.phone
	}

	if err := e.deps.Links.Paired(ctx, scope, whatsappdomain.PairedLink{
		NumberID: attempt.numberID,
		Phone:    phone,
		JID:      jid,
	}); err != nil {
		e.log.Warn("a pairing finished but the workspace was not told",
			zap.String("number", shortID(attempt.numberID)), zap.Error(err))
		attempt.fail("penautan berhasil tetapi nomornya tidak bisa dicatat: " + err.Error())
		return
	}

	attempt.succeed(phone)
	e.log.Info("a number finished pairing",
		zap.String("number", shortID(attempt.numberID)),
		zap.String("phone", maskPhone(phone)),
	)
}

// linkFailure classifies a pairing failure into something a person can act on.
//
// Pairing asks WhatsApp for the number's identity before it asks for a code, so the
// failures that reach here are mostly about the number rather than about the request.
// A number nobody registered comes back as a bad request, and reporting that as an
// internal failure tells a person to try again when what they need to do is check the
// number they typed.
func linkFailure(err error, phone string) error {
	// The classification reads the code rather than matching one sentinel, because
	// the library wraps its own errors: a bad request arrives wrapped, and matching
	// the sentinel alone is what made the first version report an unknown number as
	// an internal failure.
	var iqErr *whatsmeow.IQError
	if errors.As(err, &iqErr) {
		switch {
		case iqErr.Code == 400 || iqErr.Code == 404:
			// WhatsApp asks for the number's identity before it asks for a code, and
			// a number nobody registered comes back as a bad request. What a person
			// needs to do is check the number, not try again.
			return fmt.Errorf("%w: WhatsApp tidak mengenal nomor %s; periksa nomornya",
				whatsappdomain.ErrNotFound, maskPhone(phone))
		case iqErr.Code == 429:
			// WhatsApp throttles pairing attempts, and it says so with its own status.
			// Reporting that as an internal failure tells a person something is broken
			// when the honest answer is that they tried too often and should wait.
			return fmt.Errorf("%w: WhatsApp membatasi percobaan penautan; tunggu beberapa menit lalu coba lagi",
				whatsappdomain.ErrUnavailable)
		case iqErr.Code >= 500:
			// The service is refusing to answer, which is not about the request.
			return fmt.Errorf("%w: %w", whatsappdomain.ErrUnavailable, err)
		}
	}

	if errors.Is(err, whatsmeow.ErrIQDisconnected) {
		return fmt.Errorf("%w: %w", whatsappdomain.ErrUnavailable, err)
	}

	return fmt.Errorf("whatsapp: pair by phone: %w", err)
}

// LinkState reports where an attempt stands.
func (e *Engine) LinkState(_ context.Context, id string) (whatsappdomain.Link, error) {
	attempt := e.linkFor(id)
	if attempt == nil {
		// An attempt that is gone is one that ended, and a screen polls this
		// until it stops asking. Answering "expired" is what makes the screen
		// offer a new code instead of an error nobody can act on.
		return whatsappdomain.Link{
			ID:     id,
			State:  whatsappdomain.LinkExpired,
			Reason: "penautan ini sudah selesai atau kedaluwarsa",
		}, nil
	}
	return attempt.snapshot(), nil
}

// PairCode asks for the phone-code alternative to scanning.
func (e *Engine) PairCode(_ context.Context, id string, phone string) (whatsappdomain.Link, error) {
	attempt := e.linkFor(id)
	if attempt == nil {
		return whatsappdomain.Link{}, fmt.Errorf("%w: the link attempt is gone", whatsappdomain.ErrLinkExpired)
	}

	normalized := normalizePhone(phone)
	if len(normalized) < 8 {
		return whatsappdomain.Link{}, fmt.Errorf("%w: %q is not a phone number", whatsappdomain.ErrInvalidInput, phone)
	}

	code, err := attempt.client.PairPhone(attempt.ctx, normalized, true, whatsmeow.PairClientChrome, "Bolu")
	if err != nil {
		return whatsappdomain.Link{}, linkFailure(err, normalized)
	}
	if len(code) != pairCodeLength {
		// The server's own code is what the phone expects; a rewrite would be a
		// code the phone refuses.
		attempt.fail(fmt.Sprintf("WhatsApp memberi kode %q yang tidak bisa ditampilkan", code))
		return whatsappdomain.Link{}, fmt.Errorf("whatsapp: pair code %q is not %d characters", code, pairCodeLength)
	}

	attempt.setPairCode(code)
	return attempt.snapshot(), nil
}

// Cancel abandons an attempt.
func (e *Engine) Cancel(_ context.Context, id string) error {
	attempt := e.linkFor(id)
	if attempt == nil {
		return nil
	}

	e.forgetLink(id)
	attempt.cancel()
	if attempt.client != nil {
		attempt.client.Disconnect()
	}
	return nil
}

// Send delivers one message.
func (e *Engine) Send(ctx context.Context, snapshot whatsappdomain.Snapshot, message whatsappdomain.ToSend) (string, error) {
	runtime := e.runtimeFor(snapshot.NumberID)
	if runtime == nil {
		return "", fmt.Errorf("%w: this instance does not serve that number", whatsappdomain.ErrNotConnected)
	}
	if !runtime.client.IsConnected() {
		return "", fmt.Errorf("%w: the number is offline", whatsappdomain.ErrNotConnected)
	}

	to, err := types.ParseJID(message.ToJID)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a whatsapp address", whatsappdomain.ErrInvalidInput, message.ToJID)
	}

	outgoing, err := e.buildMessage(ctx, runtime, snapshot, message)
	if err != nil {
		return "", err
	}

	// The typing indicator is shown before every send, not only the first: a
	// batch that arrived at the number's pace should still read as somebody
	// writing one message at a time.
	if e.cfg.TypingDelay > 0 {
		if err := e.Typing(ctx, snapshot, message.ToJID); err != nil && ctx.Err() == nil {
			// A typing indicator that does not show is not a reason to lose the
			// message.
			e.log.Debug("could not show the typing indicator", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(e.cfg.TypingDelay):
		}
	}

	response, err := runtime.client.SendMessage(ctx, to, outgoing)
	if err != nil {
		return "", fmt.Errorf("whatsapp: send: %w", err)
	}

	// The indicator is cleared after the message, so the chat does not keep
	// showing "typing" in a conversation nobody is writing in.
	if e.cfg.TypingDelay > 0 {
		_ = runtime.client.SendChatPresence(ctx, to, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}

	return response.ID, nil
}

// buildMessage renders one queued item as the protobuf WhatsApp expects.
func (e *Engine) buildMessage(ctx context.Context, runtime *numberRuntime, snapshot whatsappdomain.Snapshot, message whatsappdomain.ToSend) (*waE2E.Message, error) {
	if message.Kind == whatsappdomain.KindText || message.MediaKey == "" {
		text := strings.TrimSpace(message.Text)
		if text == "" {
			return nil, fmt.Errorf("%w: a message with no body and no attachment", whatsappdomain.ErrInvalidInput)
		}
		return textMessage(text), nil
	}

	if e.deps.Media == nil {
		return nil, fmt.Errorf("%w: no media store is configured", whatsappdomain.ErrNotConfigured)
	}

	content, err := e.deps.Media.Get(ctx, message.MediaKey)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: read attachment: %w", err)
	}

	mediaType := whatsmeow.MediaDocument
	if message.Kind == whatsappdomain.KindImage {
		mediaType = whatsmeow.MediaImage
	}

	upload, err := runtime.client.Upload(ctx, content, mediaType)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: upload attachment: %w", err)
	}

	filename := filenameFromKey(message.MediaKey)
	return mediaMessage(message.Kind, upload, filename, message.Text), nil
}

// Typing shows the typing indicator in one chat.
func (e *Engine) Typing(ctx context.Context, snapshot whatsappdomain.Snapshot, toJID string) error {
	runtime := e.runtimeFor(snapshot.NumberID)
	if runtime == nil || !runtime.client.IsConnected() {
		return fmt.Errorf("%w: the number is offline", whatsappdomain.ErrNotConnected)
	}

	to, err := types.ParseJID(toJID)
	if err != nil {
		return fmt.Errorf("%w: %q is not a whatsapp address", whatsappdomain.ErrInvalidInput, toJID)
	}

	if err := runtime.client.SendChatPresence(ctx, to, presenceFor(true), types.ChatPresenceMediaText); err != nil {
		return fmt.Errorf("whatsapp: show typing: %w", err)
	}
	return nil
}

// Unlink ends the session at WhatsApp and forgets it locally.
//
// The order matters: WhatsApp is told first. A session forgotten locally while
// still live at WhatsApp's end would keep sending the phone's messages to a
// device nobody can see.
func (e *Engine) Unlink(ctx context.Context, snapshot whatsappdomain.Snapshot) error {
	runtime := e.runtimeFor(snapshot.NumberID)
	if runtime != nil {
		if runtime.client != nil && runtime.client.IsConnected() {
			if err := runtime.client.Logout(ctx); err != nil {
				// A logout that fails because the session is already gone is the
				// caller's goal already met.
				e.log.Debug("logout did not complete", zap.Error(err))
			}
		}
		e.stopNumber(runtime, "dinonaktifkan oleh workspace")
	}

	if err := e.deps.Sessions.ForgetSession(ctx, whatsappdomain.Scope{
		WorkspaceID: snapshot.WorkspaceID,
	}, snapshot.NumberID); err != nil && !errors.Is(err, whatsappdomain.ErrNotFound) {
		return err
	}

	return nil
}

// Online reports whether this instance holds the number.
func (e *Engine) Online(_ context.Context, numberID uuid.UUID) (bool, error) {
	runtime := e.runtimeFor(numberID)
	if runtime == nil {
		return false, nil
	}
	return runtime.online.Load() && runtime.client.IsConnected(), nil
}

// runtimeFor returns the runtime serving a number, if this instance has one.
func (e *Engine) runtimeFor(numberID uuid.UUID) *numberRuntime {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.numbers[numberID]
}

// linkFor returns one attempt.
func (e *Engine) linkFor(id string) *linkAttempt {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.links[id]
}

// forgetLink drops an attempt from the register.
func (e *Engine) forgetLink(id string) {
	e.mu.Lock()
	delete(e.links, id)
	e.mu.Unlock()
}

// snapshot renders an attempt's state for a screen.
func (a *linkAttempt) snapshot() whatsappdomain.Link {
	a.mu.Lock()
	defer a.mu.Unlock()

	return whatsappdomain.Link{
		ID:          a.id,
		NumberID:    a.numberID,
		WorkspaceID: a.workspaceID,
		State:       a.state,
		Code:        a.code,
		PairCode:    a.pairCode,
		Phone:       a.phone,
		ExpiresAt:   a.expiresAt,
		Reason:      a.reason,
	}
}

// setCode records a fresh QR code.
func (a *linkAttempt) setCode(code string, timeout time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.state = whatsappdomain.LinkCode
	a.code = code
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	a.expiresAt = time.Now().Add(timeout)
}

// setPairCode records the phone-code alternative.
func (a *linkAttempt) setPairCode(code string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.state = whatsappdomain.LinkCode
	a.pairCode = code
	a.expiresAt = time.Now().Add(60 * time.Second)
}

// succeed records that the number paired.
func (a *linkAttempt) succeed(phone string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.state = whatsappdomain.LinkPaired
	a.phone = phone
	a.code = ""
	a.pairCode = ""
}

// fail records why an attempt ended.
func (a *linkAttempt) fail(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// A finished attempt keeps its state: a late failure event must not turn a
	// successful pairing into a refusal on a screen that is still polling.
	if a.state == whatsappdomain.LinkPaired {
		return
	}

	a.state = whatsappdomain.LinkFailed
	a.reason = reason
	a.code = ""
	a.pairCode = ""
}

// phoneFromJID reads the number out of a JID's user part.
//
// The user part is either the number itself or `number:device`, which is the
// multi-device form, so the device suffix is dropped. With a LID-addressed account
// there is no number at all, which is why the caller falls back to what the person
// typed.
//
// The server part is what tells the two apart, and it has to be checked: a LID is
// a plain number too, so a digits-only test would read one as a phone number and
// the product would show — and try to send to — a number that is not anybody's.
func phoneFromJID(jid types.JID) string {
	if jid.Server != types.DefaultUserServer {
		return ""
	}

	user, _, _ := strings.Cut(jid.User, ":")
	if user == "" {
		return ""
	}
	for _, digit := range user {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	return user
}

// normalizePhone reduces a typed number to the digits WhatsApp wants, dropping a
// leading plus and any grouping a person typed.
func normalizePhone(phone string) string {
	var out strings.Builder
	for _, char := range phone {
		if char >= '0' && char <= '9' {
			out.WriteRune(char)
		}
	}
	return out.String()
}

// filenameFromKey is the name an attachment had when it was stored, which is the
// last segment of its key.
func filenameFromKey(key string) string {
	name := key
	if index := strings.LastIndex(key, "/"); index >= 0 {
		name = key[index+1:]
	}
	if strings.TrimSpace(name) == "" {
		return "lampiran"
	}
	return name
}

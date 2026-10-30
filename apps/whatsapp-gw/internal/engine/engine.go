package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	whatsappdomain "github.com/anakbolu/timbolu/packages/whatsapp/domain"
)

// The engine owns the live connections.
//
// It is the process that stays up: one whatsmeow client per number it holds the
// lease for, each reporting what arrived and pulling what is due. Everything it
// does with the product goes through the five small ports the module's domain
// declares, and everything the product does with WhatsApp goes through the
// Transport interface this type implements. Neither knows the other's types
// beyond those seams.
//
// Three rules it keeps:
//
//   - a number it does not hold the lease for is not connected, at all. Two
//     clients on one session send everything twice and get the number banned.
//   - the session is written through the sealer, bound to its workspace, and to
//     no other.
//   - nothing is sent that the module did not hand it. The engine has no rule
//     about when a message may leave.

// EngineConfig is how the engine behaves.
type EngineConfig struct {
	// Owner identifies this instance in the lease. It must differ between
	// instances and be stable for the life of one, or a takeover looks like a
	// renewal.
	Owner string
	// LeaseTTL is how long a lease survives without a renewal, which is therefore
	// the upper bound on how long a takeover takes.
	LeaseTTL time.Duration
	// LeaseRenew is how often a held lease is extended. It must be well under the
	// TTL, so one slow renewal does not cost the number.
	LeaseRenew time.Duration
	// DirectoryPoll is how often the fleet re-reads which numbers exist. It is
	// what makes a newly linked number come online without a restart.
	DirectoryPoll time.Duration
	// FlushInterval is how often a dirty session is written. Long enough that a
	// busy number is not a write loop, short enough that a restart loses little.
	FlushInterval time.Duration
	// DrainInterval is how often the outbox is asked for work.
	DrainInterval time.Duration
	// MediaInterval is how often undownloaded attachments are fetched.
	MediaInterval time.Duration
	// TypingDelay is how long the typing indicator shows before a message is
	// sent. It is what makes a reply read as somebody writing.
	TypingDelay time.Duration
	// SendLimit is how many messages one drain takes per number.
	SendLimit int
	// MediaLimit is how many attachments one media pass fetches.
	MediaLimit int
	// MaxMediaBytes refuses an attachment larger than this. A number that pulls a
	// video nobody will read spends its bandwidth and the workspace's storage on
	// it.
	MaxMediaBytes int
	// MaxAttempts is how many times a message is attempted before it is parked as
	// failed.
	MaxAttempts int
}

// DefaultEngineConfig is what a deployment gets when it configures nothing.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		LeaseTTL:      30 * time.Second,
		LeaseRenew:    10 * time.Second,
		DirectoryPoll: 15 * time.Second,
		FlushInterval: 5 * time.Second,
		DrainInterval: 3 * time.Second,
		MediaInterval: 20 * time.Second,
		TypingDelay:   1200 * time.Millisecond,
		SendLimit:     5,
		MediaLimit:    3,
		MaxMediaBytes: 25 << 20,
		MaxAttempts:   3,
	}
}

// EngineDeps is what the engine is wired with.
type EngineDeps struct {
	Sessions  whatsappdomain.SessionStore
	Sealer    whatsappdomain.Sealer
	Directory whatsappdomain.Directory
	Lease     whatsappdomain.Lease
	Inbox     whatsappdomain.Inbox
	Drain     whatsappdomain.Drain
	Status    whatsappdomain.Status
	Links     whatsappdomain.LinkSink
	Media     whatsappdomain.MediaStore
}

// Engine is the whatsmeow driver.
type Engine struct {
	cfg  EngineConfig
	deps EngineDeps
	log  *zap.Logger
	now  func() time.Time
	// waLog is whatsmeow's own logger, which is where it reports protocol-level
	// trouble. It is kept separate from the structured logger because whatsmeow
	// writes a line per decrypted message and those lines are not product events.
	waLog waLog.Logger

	mu      sync.Mutex
	numbers map[uuid.UUID]*numberRuntime
	links   map[string]*linkAttempt
}

// numberRuntime is one connected number.
type numberRuntime struct {
	numberID    uuid.UUID
	workspaceID uuid.UUID
	phone       string

	// ctx is the runtime's own lifetime. A takeover and an unlink both end it.
	ctx    context.Context
	cancel context.CancelFunc

	device *store.Device
	store  *deviceStore
	client *whatsmeow.Client

	online atomic.Bool

	// media holds the raw protobuf of messages whose attachment has not been
	// fetched yet. WhatsApp sends the media descriptor with the message and does
	// not keep it for us, so the download has to happen against what it sent —
	// and the download is deliberately deferred, so the descriptor is kept here
	// in the meantime.
	//
	// It is bounded and evicted oldest-first. An evicted descriptor is a media
	// error on a message that was already delivered, not a lost message.
	mediaMu  sync.Mutex
	media    map[string]*waE2E.Message
	mediaSeq []string
}

// maxPendingMedia bounds how many message protobufs are held for download.
const maxPendingMedia = 200

// rememberMedia keeps one message's descriptor for the media pass.
func (r *numberRuntime) rememberMedia(waMessageID string, message *waE2E.Message) {
	r.mediaMu.Lock()
	defer r.mediaMu.Unlock()

	if _, exists := r.media[waMessageID]; !exists {
		r.mediaSeq = append(r.mediaSeq, waMessageID)
	}
	r.media[waMessageID] = message

	for len(r.mediaSeq) > maxPendingMedia {
		oldest := r.mediaSeq[0]
		r.mediaSeq = r.mediaSeq[1:]
		delete(r.media, oldest)
	}
}

// takeRawMedia returns and forgets one message's descriptor.
func (r *numberRuntime) takeRawMedia(waMessageID string) *waE2E.Message {
	r.mediaMu.Lock()
	defer r.mediaMu.Unlock()

	message, ok := r.media[waMessageID]
	if !ok {
		return nil
	}
	delete(r.media, waMessageID)
	for index, id := range r.mediaSeq {
		if id == waMessageID {
			r.mediaSeq = append(r.mediaSeq[:index], r.mediaSeq[index+1:]...)
			break
		}
	}
	return message
}

// scope is the tenant this number belongs to.
func (r *numberRuntime) scope() whatsappdomain.Scope {
	return whatsappdomain.Scope{WorkspaceID: r.workspaceID}
}

// linkAttempt is one link in progress.
type linkAttempt struct {
	id          string
	numberID    uuid.UUID
	workspaceID uuid.UUID
	phone       string

	device *store.Device
	store  *deviceStore
	client *whatsmeow.Client

	// ctx is the attempt's lifetime. Cancel ends it, and the pairing loop ends
	// with it whether the phone paired or the code ran out.
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	state     string
	code      string
	pairCode  string
	expiresAt time.Time
	reason    string
}

// NewEngine builds the engine.
func NewEngine(cfg EngineConfig, deps EngineDeps, log *zap.Logger) *Engine {
	// Defaults are filled per field rather than by replacing the configuration.
	// A caller that names its instance and sets nothing else is the common case —
	// that is what a compose service does — and replacing the whole struct would
	// silently drop the name, which is exactly the value a fleet must control.
	defaults := DefaultEngineConfig()
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaults.LeaseTTL
	}
	if cfg.LeaseRenew <= 0 {
		cfg.LeaseRenew = defaults.LeaseRenew
	}
	if cfg.DirectoryPoll <= 0 {
		cfg.DirectoryPoll = defaults.DirectoryPoll
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaults.FlushInterval
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = defaults.DrainInterval
	}
	if cfg.MediaInterval <= 0 {
		cfg.MediaInterval = defaults.MediaInterval
	}
	if cfg.TypingDelay < 0 {
		cfg.TypingDelay = defaults.TypingDelay
	}
	if cfg.SendLimit <= 0 {
		cfg.SendLimit = defaults.SendLimit
	}
	if cfg.MediaLimit <= 0 {
		cfg.MediaLimit = defaults.MediaLimit
	}
	if cfg.MaxMediaBytes <= 0 {
		cfg.MaxMediaBytes = defaults.MaxMediaBytes
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaults.MaxAttempts
	}
	if log == nil {
		log = zap.NewNop()
	}
	if cfg.Owner == "" {
		cfg.Owner = "wa-gateway-" + uuid.NewString()[:8]
	}

	return &Engine{
		cfg:     cfg,
		deps:    deps,
		log:     log,
		now:     time.Now,
		waLog:   waLog.Noop,
		numbers: map[uuid.UUID]*numberRuntime{},
		links:   map[string]*linkAttempt{},
	}
}

// WithLogger sets whatsmeow's own logger, which a deployment turns on when it
// needs to see the protocol rather than the product.
func (e *Engine) WithLogger(log waLog.Logger) *Engine {
	if log != nil {
		e.waLog = log
	}
	return e
}

// Run reconciles the fleet until the context is cancelled.
//
// The loops are separate so that a slow outbox never delays a lease renewal:
// losing a lease because the drain was busy would take a number offline for no
// reason.
func (e *Engine) Run(ctx context.Context) error {
	if e.deps.Sessions == nil || e.deps.Sealer == nil || e.deps.Directory == nil {
		return fmt.Errorf("%w: the engine needs a session store, a sealer, and a directory", whatsappdomain.ErrNotConfigured)
	}

	var wg sync.WaitGroup

	e.reconcile(ctx)

	wg.Add(4)
	go func() { defer wg.Done(); e.loop(ctx, e.cfg.DirectoryPoll, e.reconcileAndRenew) }()
	go func() { defer wg.Done(); e.loop(ctx, e.cfg.DrainInterval, e.drainAll) }()
	go func() { defer wg.Done(); e.loop(ctx, e.cfg.MediaInterval, e.fetchMediaAll) }()
	go func() { defer wg.Done(); e.loop(ctx, e.cfg.FlushInterval, e.flushAll) }()

	<-ctx.Done()
	wg.Wait()

	e.shutdown()
	return nil
}

// loop runs one engine pass on an interval until the context ends.
func (e *Engine) loop(ctx context.Context, every time.Duration, pass func(context.Context)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass(ctx)
		}
	}
}

// reconcileAndRenew re-reads the fleet and extends what this instance holds.
func (e *Engine) reconcileAndRenew(ctx context.Context) {
	e.reconcile(ctx)
	e.renew(ctx)
}

// reconcile brings the running numbers in line with what this instance may
// serve.
func (e *Engine) reconcile(ctx context.Context) {
	assignments, err := e.deps.Directory.Serving(ctx)
	if err != nil {
		e.log.Warn("could not read which numbers to serve", zap.Error(err))
		return
	}

	wanted := make(map[uuid.UUID]whatsappdomain.Assignment, len(assignments))
	for _, assignment := range assignments {
		wanted[assignment.NumberID] = assignment
	}

	// A number that is no longer served must let go, so another instance — or
	// nobody — has it rather than a stale client holding it silently.
	e.mu.Lock()
	var drop []*numberRuntime
	for numberID, runtime := range e.numbers {
		if _, ok := wanted[numberID]; !ok {
			drop = append(drop, runtime)
			delete(e.numbers, numberID)
		}
	}
	e.mu.Unlock()
	for _, runtime := range drop {
		e.stopNumber(runtime, "it is no longer served")
	}

	for _, assignment := range wanted {
		if e.running(assignment.NumberID) {
			continue
		}

		held, err := e.deps.Lease.Acquire(ctx, assignment.NumberID, e.cfg.Owner, e.cfg.LeaseTTL)
		if err != nil {
			e.log.Warn("could not claim a number", zap.Error(err))
			continue
		}
		if !held {
			// Somebody else is serving it, which is the healthy answer.
			continue
		}

		served, err := e.startNumber(ctx, assignment)
		if err != nil {
			e.log.Warn("could not serve a number",
				zap.String("number", shortID(assignment.NumberID)), zap.Error(err))
		}
		if served {
			continue
		}

		// The lease is given back rather than held. A number with no session yet —
		// a row a person created and did not finish linking — would otherwise be
		// claimed for the length of the TTL by an instance that is not serving it,
		// and the next pass would find it already held and never retry.
		if err := e.deps.Lease.Release(ctx, assignment.NumberID, e.cfg.Owner); err != nil {
			e.log.Warn("could not release the lease of a number nothing serves", zap.Error(err))
		}
	}
}

// renew extends every held lease and gives up the ones this instance lost.
func (e *Engine) renew(ctx context.Context) {
	e.mu.Lock()
	runtimes := make([]*numberRuntime, 0, len(e.numbers))
	for _, runtime := range e.numbers {
		runtimes = append(runtimes, runtime)
	}
	e.mu.Unlock()

	for _, runtime := range runtimes {
		held, err := e.deps.Lease.Renew(ctx, runtime.numberID, e.cfg.Owner, e.cfg.LeaseTTL)
		if err != nil {
			// A Redis that cannot answer is not a reason to drop a connection: the
			// lease still exists with its original deadline, and dropping the
			// number on a transient error would take it offline for no reason.
			e.log.Warn("could not renew a lease", zap.String("number", shortID(runtime.numberID)), zap.Error(err))
			continue
		}
		if held {
			continue
		}

		// Somebody took it while we held it, which means our lease expired and a
		// successor is connecting. Two clients on one session is the one thing
		// this must never do, so this one stands down immediately.
		e.log.Warn("a lease was taken over; standing down", zap.String("number", shortID(runtime.numberID)))
		e.drop(runtime, "another instance took the number over")
	}
}

// startNumber connects one number and reports whether anything was served.
//
// The two answers are different and both are needed: a number with no session is a
// state rather than a failure, so it is not an error — and it is also not served, so
// the caller must not keep its lease.
func (e *Engine) startNumber(ctx context.Context, assignment whatsappdomain.Assignment) (bool, error) {
	scope := whatsappdomain.Scope{WorkspaceID: assignment.WorkspaceID}

	number, err := e.deps.Status.Number(ctx, scope, assignment.NumberID)
	if err != nil {
		return false, err
	}
	if !number.Linked() {
		// A number with no session cannot be connected; it waits for a person to
		// link it.
		return false, nil
	}

	session, err := e.deps.Sessions.Load(ctx, scope, assignment.NumberID)
	if err != nil {
		if errors.Is(err, whatsappdomain.ErrNotFound) {
			return false, nil
		}
		return false, err
	}

	plaintext, err := e.deps.Sealer.Open(ctx, assignment.WorkspaceID, session.Sealed)
	if err != nil {
		// A blob that will not open under its own workspace's key is either a
		// rotation that dropped the key or a row that was tampered with. Both are
		// a person's problem, so the number is reported as needing a relink
		// rather than retried forever.
		_, reportErr := e.deps.Status.Report(ctx, scope, whatsappdomain.ReportRequest{
			NumberID: assignment.NumberID,
			Status:   whatsappdomain.StatusNeedsRelink,
			Reason:   "sesi tidak bisa dibuka dengan kunci workspace ini",
			At:       e.now(),
		})
		if reportErr != nil {
			e.log.Warn("could not report an unopenable session", zap.Error(reportErr))
		}
		return false, fmt.Errorf("whatsapp: open session: %w", err)
	}

	device, inner, err := restoreDevice(plaintext, assignment.NumberID, assignment.WorkspaceID, e.waLog)
	if err != nil {
		return false, err
	}
	inner.put = e.persister(scope, assignment.NumberID)

	runtime := e.newRuntime(assignment, number.Phone, device, inner)
	if err := e.connect(ctx, runtime); err != nil {
		runtime.cancel()
		return false, err
	}

	e.mu.Lock()
	e.numbers[assignment.NumberID] = runtime
	e.mu.Unlock()

	e.log.Info("a number is being served",
		zap.String("number", shortID(assignment.NumberID)),
		zap.String("phone", maskPhone(number.Phone)),
	)

	// Stray messages are reclaimed the moment this instance takes a number on,
	// which is exactly when they exist: a gateway replaced mid-send leaves rows in
	// `sending`, and the successor is the only thing that can return them to the
	// queue. Doing it per pass instead would be a write per number per interval for
	// a case that happens once.
	e.reclaimNumber(ctx, scope, assignment.NumberID)

	go e.reportPresence(ctx, runtime)
	return true, nil
}

// newRuntime builds and connects one client.
func (e *Engine) newRuntime(assignment whatsappdomain.Assignment, phone string, device *store.Device, inner *deviceStore) *numberRuntime {
	runtimeCtx, cancel := context.WithCancel(context.Background())
	runtime := &numberRuntime{
		numberID:    assignment.NumberID,
		workspaceID: assignment.WorkspaceID,
		phone:       phone,
		ctx:         runtimeCtx,
		cancel:      cancel,
		device:      device,
		store:       inner,
		media:       map[string]*waE2E.Message{},
	}
	runtime.client = whatsmeow.NewClient(device, e.waLog)
	runtime.client.AddEventHandler(e.handler(runtimeCtx, runtime))

	return runtime
}

// connect brings a client online.
func (e *Engine) connect(_ context.Context, runtime *numberRuntime) error {
	if err := runtime.client.Connect(); err != nil {
		return fmt.Errorf("whatsapp: connect: %w", err)
	}
	return nil
}

// reportPresence keeps the number's status in step with the connection, and
// tells the workspace when it needs a person.
func (e *Engine) reportPresence(ctx context.Context, runtime *numberRuntime) {
	ticker := time.NewTicker(e.cfg.LeaseRenew)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-runtime.ctx.Done():
			return
		case <-ticker.C:
		}

		if !runtime.client.IsConnected() {
			continue
		}

		// The report carries a timestamp, and the store only moves last_seen_at
		// forward, so a repeated report is what keeps "is it alive" answerable
		// from the database without asking this process.
		if _, err := e.deps.Status.Report(ctx, runtime.scope(), whatsappdomain.ReportRequest{
			NumberID: runtime.numberID,
			Status:   whatsappdomain.StatusConnected,
			At:       e.now(),
		}); err != nil && ctx.Err() == nil {
			e.log.Debug("could not report a number online", zap.Error(err))
		}
	}
}

// stopNumber disconnects one number and gives its lease back.
func (e *Engine) stopNumber(runtime *numberRuntime, why string) {
	e.log.Info("a number is no longer served",
		zap.String("number", shortID(runtime.numberID)), zap.String("reason", why))

	e.drop(runtime, why)
	releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.deps.Lease.Release(releaseCtx, runtime.numberID, e.cfg.Owner); err != nil {
		e.log.Warn("could not release a lease", zap.Error(err))
	}
}

// drop disconnects a client and forgets it, without touching the lease. It is
// what a takeover calls: the successor already holds the lease, so releasing it
// here would delete the successor's claim.
func (e *Engine) drop(runtime *numberRuntime, why string) {
	e.mu.Lock()
	delete(e.numbers, runtime.numberID)
	e.mu.Unlock()

	runtime.cancel()
	if runtime.client != nil {
		runtime.client.Disconnect()
	}
	if err := runtime.store.flush(context.Background(), true); err != nil {
		// The client is already gone, so a session that cannot be written is
		// worth saying and not worth failing the shutdown over: the number is
		// reported as disconnected either way, and the next start re-reads what
		// was last stored.
		e.log.Warn("could not store a session while dropping a number", zap.Error(err))
	}

	status := whatsappdomain.StatusDisconnected
	if strings.Contains(why, "relink") {
		status = whatsappdomain.StatusNeedsRelink
	}
	// The report is best effort: the client is already gone, and a status that
	// cannot be written must not keep the process from moving on.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.deps.Status.Report(ctx, runtime.scope(), whatsappdomain.ReportRequest{
		NumberID: runtime.numberID,
		Status:   status,
		Reason:   why,
		At:       e.now(),
	}); err != nil {
		e.log.Warn("could not report a number offline", zap.Error(err))
	}
}

// shutdown lets go of everything cleanly.
func (e *Engine) shutdown() {
	e.mu.Lock()
	runtimes := make([]*numberRuntime, 0, len(e.numbers))
	for _, runtime := range e.numbers {
		runtimes = append(runtimes, runtime)
	}
	e.numbers = map[uuid.UUID]*numberRuntime{}
	links := make([]*linkAttempt, 0, len(e.links))
	for _, attempt := range e.links {
		links = append(links, attempt)
	}
	e.links = map[string]*linkAttempt{}
	e.mu.Unlock()

	for _, attempt := range links {
		attempt.cancel()
		if attempt.client != nil {
			attempt.client.Disconnect()
		}
	}

	for _, runtime := range runtimes {
		runtime.cancel()
		if runtime.client != nil {
			runtime.client.Disconnect()
		}
		// Forced, so a shutdown writes even a session that was already clean.
		if err := runtime.store.flush(context.Background(), true); err != nil {
			e.log.Warn("could not store a session during shutdown",
				zap.String("number", shortID(runtime.numberID)), zap.Error(err))
		}
		// Released rather than left to expire: a successor takes over in
		// milliseconds after a clean shutdown instead of waiting for the TTL.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = e.deps.Lease.Release(ctx, runtime.numberID, e.cfg.Owner)
		cancel()
	}
}

// running reports whether this instance already serves a number.
func (e *Engine) running(numberID uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.numbers[numberID]
	return ok
}

// persister is how one number's device state is written: sealed with its
// workspace's own key, and stored under the scope that owns it.
func (e *Engine) persister(scope whatsappdomain.Scope, numberID uuid.UUID) putFunc {
	return func(ctx context.Context, blob sealed) error {
		encoded, err := json.Marshal(blob)
		if err != nil {
			return fmt.Errorf("whatsapp: encode session: %w", err)
		}

		sealedBytes, err := e.deps.Sealer.Seal(ctx, scope.WorkspaceID, encoded)
		if err != nil {
			return fmt.Errorf("whatsapp: seal session: %w", err)
		}

		if err := e.deps.Sessions.Save(ctx, scope, whatsappdomain.Session{
			NumberID:    numberID,
			WorkspaceID: scope.WorkspaceID,
			Sealed:      sealedBytes,
			JID:         blob.JID,
		}); err != nil {
			return fmt.Errorf("whatsapp: store session: %w", err)
		}
		return nil
	}
}

// shortID renders an id for a log line.
func shortID(id uuid.UUID) string {
	text := id.String()
	if len(text) < 8 {
		return text
	}
	return text[:8]
}

// maskPhone hides all but the last four digits of a number, which is what the
// logs and the screens both read: enough to recognise, not enough to publish.
func maskPhone(phone string) string {
	digits := strings.TrimSpace(phone)
	if len(digits) <= 4 {
		return strings.Repeat("•", len(digits))
	}
	return strings.Repeat("•", len(digits)-4) + digits[len(digits)-4:]
}

// messageKind names what a WhatsApp message is, in this module's own words.
func messageKind(message *waE2E.Message) string {
	switch {
	case message.GetConversation() != "" || message.GetExtendedTextMessage() != nil:
		return whatsappdomain.KindText
	case message.GetImageMessage() != nil:
		return whatsappdomain.KindImage
	case message.GetVideoMessage() != nil:
		return whatsappdomain.KindVideo
	case message.GetAudioMessage() != nil:
		return whatsappdomain.KindAudio
	case message.GetDocumentMessage() != nil:
		return whatsappdomain.KindDocument
	case message.GetStickerMessage() != nil:
		return whatsappdomain.KindSticker
	default:
		return whatsappdomain.KindOther
	}
}

// messageText is the body a person reads.
func messageText(message *waE2E.Message) string {
	if text := message.GetConversation(); text != "" {
		return text
	}
	if extended := message.GetExtendedTextMessage(); extended != nil {
		return extended.GetText()
	}
	if image := message.GetImageMessage(); image != nil {
		return image.GetCaption()
	}
	if video := message.GetVideoMessage(); video != nil {
		return video.GetCaption()
	}
	if document := message.GetDocumentMessage(); document != nil {
		return document.GetCaption()
	}
	return ""
}

// messageMedia describes the attachment a message carries, if any.
func messageMedia(message *waE2E.Message) (whatsappdomain.Media, bool) {
	switch {
	case message.GetImageMessage() != nil:
		image := message.GetImageMessage()
		return whatsappdomain.Media{
			Ref:    image.GetDirectPath(),
			Kind:   whatsappdomain.KindImage,
			Size:   int64(image.GetFileLength()),
			Status: whatsappdomain.MediaPending,
		}, true
	case message.GetVideoMessage() != nil:
		video := message.GetVideoMessage()
		return whatsappdomain.Media{
			Ref:    video.GetDirectPath(),
			Kind:   whatsappdomain.KindVideo,
			Size:   int64(video.GetFileLength()),
			Status: whatsappdomain.MediaPending,
		}, true
	case message.GetAudioMessage() != nil:
		audio := message.GetAudioMessage()
		return whatsappdomain.Media{
			Ref:    audio.GetDirectPath(),
			Kind:   whatsappdomain.KindAudio,
			Size:   int64(audio.GetFileLength()),
			Status: whatsappdomain.MediaPending,
		}, true
	case message.GetDocumentMessage() != nil:
		document := message.GetDocumentMessage()
		return whatsappdomain.Media{
			Ref:    document.GetDirectPath(),
			Kind:   whatsappdomain.KindDocument,
			Size:   int64(document.GetFileLength()),
			Status: whatsappdomain.MediaPending,
		}, true
	case message.GetStickerMessage() != nil:
		sticker := message.GetStickerMessage()
		return whatsappdomain.Media{
			Ref:    sticker.GetDirectPath(),
			Kind:   whatsappdomain.KindSticker,
			Size:   int64(sticker.GetFileLength()),
			Status: whatsappdomain.MediaPending,
		}, true
	default:
		return whatsappdomain.Media{}, false
	}
}

// mediaContentType is what a provider sends for each kind.
func mediaContentType(kind string) string {
	return map[string]string{
		whatsappdomain.KindImage:    "image/jpeg",
		whatsappdomain.KindVideo:    "video/mp4",
		whatsappdomain.KindAudio:    "audio/ogg",
		whatsappdomain.KindDocument: "application/octet-stream",
		whatsappdomain.KindSticker:  "image/webp",
	}[kind]
}

// downloadInfo is what whatsmeow needs to fetch an attachment.
func downloadInfo(message *waE2E.Message) whatsmeow.DownloadableMessage {
	switch {
	case message.GetImageMessage() != nil:
		return message.GetImageMessage()
	case message.GetVideoMessage() != nil:
		return message.GetVideoMessage()
	case message.GetAudioMessage() != nil:
		return message.GetAudioMessage()
	case message.GetDocumentMessage() != nil:
		return message.GetDocumentMessage()
	case message.GetStickerMessage() != nil:
		return message.GetStickerMessage()
	default:
		return nil
	}
}

// textMessage builds the protobuf a text send carries.
func textMessage(text string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(text)}
}

// mediaMessage builds the protobuf a media send carries.
func mediaMessage(kind string, upload whatsmeow.UploadResponse, filename, caption string) *waE2E.Message {
	switch kind {
	case whatsappdomain.KindImage:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption:       proto.String(caption),
			URL:           proto.String(upload.URL),
			DirectPath:    proto.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    proto.Uint64(upload.FileLength),
			Mimetype:      proto.String(mediaContentType(kind)),
		}}
	default:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Caption:       proto.String(caption),
			FileName:      proto.String(filename),
			URL:           proto.String(upload.URL),
			DirectPath:    proto.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    proto.Uint64(upload.FileLength),
			Mimetype:      proto.String(mediaContentType(whatsappdomain.KindDocument)),
		}}
	}
}

// presenceFor turns this module's chat state into whatsmeow's.
func presenceFor(typing bool) types.ChatPresence {
	if typing {
		return types.ChatPresenceComposing
	}
	return types.ChatPresencePaused
}

// handler builds the whatsmeow event handler for one number.
func (e *Engine) handler(ctx context.Context, runtime *numberRuntime) func(any) {
	return func(raw any) {
		switch event := raw.(type) {
		case *events.Message:
			e.onMessage(ctx, runtime, event)
		case *events.Connected:
			runtime.online.Store(true)
			e.onConnected(ctx, runtime)
		case *events.Disconnected:
			runtime.online.Store(false)
			// A disconnect is not necessarily the number's fault, so it is
			// reported without claiming the session is gone. whatsmeow reconnects
			// on its own.
			e.reportStatus(ctx, runtime, whatsappdomain.StatusDisconnected, "koneksi terputus, mencoba menyambung lagi")
		case *events.LoggedOut:
			runtime.online.Store(false)
			e.onLoggedOut(ctx, runtime, event)
		}
	}
}

// onConnected records that the number came online.
func (e *Engine) onConnected(ctx context.Context, runtime *numberRuntime) {
	// The session is written on a successful connection because that is the
	// moment the ADV identity is known to be the one WhatsApp accepts.
	if err := runtime.store.flush(ctx, true); err != nil {
		e.log.Warn("could not store a session after connecting", zap.Error(err))
	}

	e.reportStatus(ctx, runtime, whatsappdomain.StatusConnected, "")
}

// onLoggedOut handles WhatsApp refusing the session.
func (e *Engine) onLoggedOut(ctx context.Context, runtime *numberRuntime, event *events.LoggedOut) {
	reason := "WhatsApp memutus sesi ini"
	if event.OnConnect {
		reason = "WhatsApp menolak sesi ini saat menyambung"
	}

	// The session is gone at WhatsApp's end, so keeping it would only produce a
	// client that reconnects into the same refusal.
	if err := e.deps.Sessions.ForgetSession(ctx, runtime.scope(), runtime.numberID); err != nil {
		e.log.Warn("could not forget a refused session", zap.Error(err))
	}

	e.stopNumber(runtime, reason+" (relink)")
}

// reportStatus writes a status, logging a failure rather than failing a pass.
func (e *Engine) reportStatus(ctx context.Context, runtime *numberRuntime, status, reason string) {
	if _, err := e.deps.Status.Report(ctx, runtime.scope(), whatsappdomain.ReportRequest{
		NumberID: runtime.numberID,
		Status:   status,
		Reason:   reason,
		At:       e.now(),
	}); err != nil && ctx.Err() == nil {
		e.log.Warn("could not report a number's status", zap.Error(err))
	}
}

// onMessage turns one WhatsApp message into the module's own and records it.
func (e *Engine) onMessage(ctx context.Context, runtime *numberRuntime, event *events.Message) {
	if event.Message == nil {
		return
	}

	media, hasMedia := messageMedia(event.Message)

	inbound := whatsappdomain.Inbound{
		NumberID:    runtime.numberID,
		WaMessageID: event.Info.ID,
		ChatJID:     event.Info.Chat.String(),
		IsGroup:     event.Info.IsGroup,
		IsFromMe:    event.Info.IsFromMe,
		Kind:        messageKind(event.Message),
		Text:        messageText(event.Message),
		ReceivedAt:  event.Info.Timestamp,
	}
	if event.Info.Sender != (types.JID{}) {
		inbound.SenderJID = event.Info.Sender.String()
	}
	if event.Info.PushName != "" {
		inbound.SenderName = event.Info.PushName
	}
	if inbound.ReceivedAt.IsZero() {
		inbound.ReceivedAt = e.now()
	}
	if hasMedia {
		inbound.Media = media
		runtime.rememberMedia(inbound.WaMessageID, event.Message)
	}

	// The bytes are fetched after the message is recorded, so a large download
	// never holds up the listener: this call returns as soon as the row exists,
	// and the media pass picks it up.
	if _, _, err := e.deps.Inbox.Receive(ctx, runtime.scope(), runtime.numberID, inbound); err != nil {
		if ctx.Err() != nil {
			return
		}
		e.log.Warn("could not record an inbound message",
			zap.String("number", shortID(runtime.numberID)),
			zap.String("message", inbound.WaMessageID),
			zap.Error(err),
		)
	}
}

// drainAll pulls each online number's due work and sends it.
func (e *Engine) drainAll(ctx context.Context) {
	// The messages a dead gateway left mid-send are returned to the queue first,
	// so a takeover does not leave them stuck in `sending` forever.
	for _, runtime := range e.snapshot() {
		if !runtime.client.IsConnected() {
			continue
		}
		e.drainNumber(ctx, runtime)
	}
}

// drainNumber sends what one number may send now.
func (e *Engine) drainNumber(ctx context.Context, runtime *numberRuntime) {
	scope := runtime.scope()

	claimed, err := e.deps.Drain.Claim(ctx, scope, runtime.numberID, e.cfg.SendLimit)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("could not read the outbox", zap.String("number", shortID(runtime.numberID)), zap.Error(err))
		}
		return
	}
	if claimed.Held > 0 {
		e.log.Info("a number's messages are paced by its hourly ceiling",
			zap.String("number", shortID(runtime.numberID)),
			zap.Int("held", claimed.Held),
		)
	}

	for _, item := range claimed.Ready {
		if ctx.Err() != nil {
			return
		}

		snapshot := whatsappdomain.Snapshot{
			NumberID:    runtime.numberID,
			WorkspaceID: runtime.workspaceID,
			Phone:       runtime.phone,
			Session:     &whatsappdomain.Session{NumberID: runtime.numberID, WorkspaceID: runtime.workspaceID},
		}

		waMessageID, err := e.Send(ctx, snapshot, whatsappdomain.ToSend{
			ID:       item.ID,
			ToJID:    item.ToJID,
			Kind:     item.Kind,
			Text:     item.Text,
			MediaKey: item.MediaKey,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attempts := item.Attempts
			terminal := attempts >= e.cfg.MaxAttempts
			if terminal {
				if failErr := e.deps.Drain.Failed(ctx, scope, item.ID, err.Error(), attempts); failErr != nil {
					e.log.Warn("could not park a failed message", zap.Error(failErr))
				}
				e.log.Warn("a message was parked as failed",
					zap.String("number", shortID(runtime.numberID)),
					zap.Int("attempts", attempts),
					zap.Error(err),
				)
				continue
			}

			// Not terminal: the message goes back to the queue with a later time,
			// which is what a transient failure deserves. It is given back rather
			// than left in `sending`, so a restart does not strand it.
			if retryErr := e.deps.Drain.Retry(ctx, scope, item.ID, err.Error(),
				e.now().Add(sendBackoff(attempts))); retryErr != nil {
				e.log.Warn("could not return a message to the queue", zap.Error(retryErr))
			}
			continue
		}

		if err := e.deps.Drain.Sent(ctx, scope, item.ID, waMessageID, e.now()); err != nil {
			e.log.Warn("could not record a sent message", zap.Error(err))
		}
	}
}

// sendBackoff is how long a message waits after a failed attempt. It grows with
// the attempt so a number that is refusing messages is not hammered, and it is
// bounded so a customer's answer is delayed rather than lost.
func sendBackoff(attempts int) time.Duration {
	backoff := time.Duration(attempts) * 5 * time.Second
	if backoff > time.Minute {
		return time.Minute
	}
	if backoff < 5*time.Second {
		return 5 * time.Second
	}
	return backoff
}

// reclaimNumber returns the messages a dead gateway left mid-send to the queue.
func (e *Engine) reclaimNumber(ctx context.Context, scope whatsappdomain.Scope, numberID uuid.UUID) {
	if e.deps.Drain == nil {
		return
	}

	reclaimed, err := e.deps.Drain.Reclaim(ctx, scope, e.now().Add(-2*e.cfg.LeaseTTL))
	if err != nil {
		e.log.Warn("could not reclaim stranded messages",
			zap.String("number", shortID(numberID)), zap.Error(err))
		return
	}
	if reclaimed > 0 {
		e.log.Info("stranded messages were returned to the queue",
			zap.String("number", shortID(numberID)), zap.Int("count", reclaimed))
	}
}

// fetchMediaAll downloads the attachments that are still pending.
func (e *Engine) fetchMediaAll(ctx context.Context) {
	for _, runtime := range e.snapshot() {
		e.fetchMedia(ctx, runtime)
	}
}

// fetchMedia downloads one number's pending attachments.
func (e *Engine) fetchMedia(ctx context.Context, runtime *numberRuntime) {
	pending, err := e.deps.Inbox.PendingMedia(ctx, runtime.scope(), e.cfg.MediaLimit)
	if err != nil {
		e.log.Debug("could not read pending attachments", zap.Error(err))
		return
	}

	for _, message := range pending {
		if ctx.Err() != nil {
			return
		}
		if message.Media.Status != whatsappdomain.MediaPending {
			continue
		}
		if e.cfg.MaxMediaBytes > 0 && message.Media.Size > int64(e.cfg.MaxMediaBytes) {
			e.log.Info("an attachment is larger than this deployment stores",
				zap.String("message", message.WaMessageID),
				zap.Int64("size", message.Media.Size),
			)
			e.storeMediaError(ctx, runtime, message, fmt.Sprintf(
				"lampiran %d MB melebihi batas %d MB", message.Media.Size>>20, e.cfg.MaxMediaBytes>>20))
			continue
		}

		content, err := e.download(ctx, runtime, message)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A failed download is recorded rather than retried forever: the
			// message itself is already in front of a person, and the bytes are
			// the least of it.
			e.log.Warn("could not fetch an attachment",
				zap.String("message", message.WaMessageID), zap.Error(err))
			e.storeMediaError(ctx, runtime, message, err.Error())
			continue
		}

		if e.cfg.MaxMediaBytes > 0 && len(content) > e.cfg.MaxMediaBytes {
			e.storeMediaError(ctx, runtime, message, "lampiran melebihi batas ukuran")
			continue
		}

		stored, err := e.deps.Media.Put(ctx, runtime.scope(), message.ID, message.Media, content)
		if err != nil {
			e.log.Warn("could not store an attachment", zap.Error(err))
			e.storeMediaError(ctx, runtime, message, err.Error())
			continue
		}

		if err := e.deps.Inbox.StoreMedia(ctx, runtime.scope(), message.ID, whatsappdomain.Media{
			Kind:   message.Media.Kind,
			Size:   stored.Size,
			Status: whatsappdomain.MediaReady,
			Key:    stored.Key,
		}); err != nil {
			e.log.Warn("could not record where an attachment went", zap.Error(err))
		}
	}
}

// download fetches one message's bytes.
//
// The media descriptor WhatsApp sends is not stored with the message, so the
// bytes are re-requested from the provider by the message's own record: the
// engine keeps the raw protobuf of the message it saw, keyed by WhatsApp's id,
// until the download happens.
func (e *Engine) download(ctx context.Context, runtime *numberRuntime, message whatsappdomain.Inbound) ([]byte, error) {
	raw := runtime.takeRawMedia(message.WaMessageID)
	if raw == nil {
		return nil, errors.New("whatsapp: the message's media is no longer available")
	}
	downloadable := downloadInfo(raw)
	if downloadable == nil {
		return nil, errors.New("whatsapp: the message carries no attachment this deployment fetches")
	}
	return runtime.client.Download(ctx, downloadable)
}

// storeMediaError records that an attachment will not arrive.
func (e *Engine) storeMediaError(ctx context.Context, runtime *numberRuntime, message whatsappdomain.Inbound, reason string) {
	if err := e.deps.Inbox.StoreMedia(ctx, runtime.scope(), message.ID, whatsappdomain.Media{
		Kind:   message.Media.Kind,
		Size:   message.Media.Size,
		Status: whatsappdomain.MediaFailed,
		Error:  reason,
	}); err != nil {
		e.log.Warn("could not record a failed attachment", zap.Error(err))
	}
}

// flushAll writes every dirty session.
func (e *Engine) flushAll(ctx context.Context) {
	for _, runtime := range e.snapshot() {
		if err := runtime.store.flush(ctx, false); err != nil && ctx.Err() == nil {
			e.log.Warn("could not store a session", zap.String("number", shortID(runtime.numberID)), zap.Error(err))
		}
	}
}

// snapshot returns the runtimes this instance is serving.
func (e *Engine) snapshot() []*numberRuntime {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]*numberRuntime, 0, len(e.numbers))
	for _, runtime := range e.numbers {
		out = append(out, runtime)
	}
	return out
}

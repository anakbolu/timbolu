package container

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	routinedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	whatsappadapter "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/adapter"
	whatsappservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/service"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/mail"
	"github.com/robzlabz/employeebot/packages/platform/config"
	"github.com/robzlabz/employeebot/packages/platform/storage"
	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
	"github.com/robzlabz/employeebot/packages/whatsapp/seam"
)

// openWhatsApp builds the WhatsApp module: the numbers a workspace has linked,
// who answers them, and the seam to the process that owns the connections.
//
// It runs after the integrations, the task runtime, and the routines: a linked
// number is a connection the hub shows, an inbound message opens a task, and a
// routine may be watching the message event.
func (c *Container) openWhatsApp(_ context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.Repositories.WhatsApp == nil {
		c.Logger.Warn("whatsapp is disabled: no database connection")
		return nil
	}

	// How this process talks to WhatsApp: the gateway process owns the
	// connections, and every other process reaches it over HTTP. Nothing is
	// wired when neither is configured, and the module then serves its reads and
	// refuses to link — which is a deployment without a gateway rather than a
	// broken one.
	transport := c.whatsAppTransport(cfg)
	if transport == nil {
		c.Logger.Warn("whatsapp cannot link or send: no gateway address and token are configured")
	}

	service := whatsappservice.New(whatsappservice.Deps{
		Repository: c.Repositories.WhatsApp,
		Sessions:   c.Repositories.WhatsApp,
		Transport:  transport,
		Connections: whatsAppConnections{
			integrations: c.Services.Integration,
		},
		Activity:    c.whatsAppActivity(),
		Alerter:     c.whatsAppAlerter(),
		Replies:     whatsAppReplies{service: c.Services.Task},
		Watchers:    whatsAppWatchers{routines: c.Routine},
		Media:       c.whatsAppMedia(),
		Logger:      c.Logger,
		HourlyLimit: cfg.WhatsApp.HourlyLimit,
	})

	c.WhatsApp = service
	c.Services.WhatsApp = service

	return nil
}

// whatsAppTransport picks which side of the seam this process is on.
//
// The choice follows from which binary is running and nothing else: the gateway
// replaces its transport with the engine it owns, and every other process gets a
// client that reaches that engine. A flag the caller passed would be a second
// place for the same fact.
func (c *Container) whatsAppTransport(cfg *config.Config) whatsappdomain.Transport {
	if cfg.WhatsApp.GatewayURL == "" || cfg.WhatsApp.GatewayToken == "" {
		return nil
	}

	return seam.NewClient(cfg.WhatsApp.GatewayURL, cfg.WhatsApp.GatewayToken, 30*time.Second)
}

// whatsAppConnections records a number as a connection the hub shows.
type whatsAppConnections struct {
	integrations integrationdomain.Service
}

// Link implements whatsappdomain.Connections.
func (c whatsAppConnections) Link(ctx context.Context, scope whatsappdomain.Scope, link whatsappdomain.ConnectionLink) (uuid.UUID, error) {
	if c.integrations == nil {
		return uuid.Nil, fmt.Errorf("whatsapp: the integration hub is not configured")
	}

	connection, err := c.integrations.Link(ctx, integrationdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, integrationdomain.LinkInput{
		App:          link.App,
		AccountLabel: link.AccountLabel,
	})
	if err != nil {
		return uuid.Nil, err
	}

	return connection.ID, nil
}

// Disconnect implements whatsappdomain.Connections.
func (c whatsAppConnections) Disconnect(ctx context.Context, scope whatsappdomain.Scope, integrationID uuid.UUID) error {
	if c.integrations == nil {
		return nil
	}

	return c.integrations.Disconnect(ctx, integrationdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, integrationID)
}

// whatsAppActivity publishes a number's own events into the workspace's stream.
//
// The activity table belongs to the chat module, so that module is the one that
// writes it — the same split the memory and routine modules use.
func (c *Container) whatsAppActivity() whatsappdomain.Activity {
	if c.Repositories == nil || c.Repositories.Chat == nil {
		return nil
	}

	return whatsAppActivity{store: c.Repositories.Chat, logger: c.Logger}
}

type whatsAppActivity struct {
	store  chatdomain.EventStore
	logger *zap.Logger
}

// Emit implements whatsappdomain.Activity.
func (a whatsAppActivity) Emit(ctx context.Context, event whatsappdomain.Event) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("whatsapp: render event: %w", err)
	}

	if _, err := a.store.AppendEvent(ctx, chatdomain.Event{
		WorkspaceID: event.WorkspaceID,
		Type:        event.Type,
		Payload:     payload,
	}); err != nil {
		return err
	}

	return nil
}

// whatsAppAlerter tells a workspace's owners that a number needs them.
//
// It is one implementation for two effects — a line in the live stream and an
// email — because they are one decision. The email is the part that reaches
// somebody who is not looking at the screen, which is the point of the alert.
func (c *Container) whatsAppAlerter() whatsappdomain.Alerter {
	if c.Repositories == nil || c.Repositories.Workspace == nil {
		return nil
	}

	return whatsAppAlerter{
		workspaces: c.Repositories.Workspace,
		mailer:     c.Mailer,
		activity:   c.whatsAppActivity(),
		logger:     c.Logger,
	}
}

type whatsAppAlerter struct {
	workspaces *workspacerepo.Repository
	mailer     mail.Sender
	activity   whatsappdomain.Activity
	logger     *zap.Logger
}

// Alert implements whatsappdomain.Alerter.
func (a whatsAppAlerter) Alert(ctx context.Context, scope whatsappdomain.Scope, alert whatsappdomain.Alert) error {
	if a.activity != nil {
		if err := a.activity.Emit(ctx, whatsappdomain.Event{
			WorkspaceID: scope.WorkspaceID,
			Type:        alert.Kind,
			Payload: map[string]any{
				"number_id": alert.NumberID.String(),
				"status":    alert.Status,
				"reason":    alert.Reason,
			},
		}); err != nil && a.logger != nil {
			a.logger.Warn("could not publish a whatsapp alert", zap.Error(err))
		}
	}

	if a.mailer == nil {
		return nil
	}

	members, err := a.workspaces.Members(ctx, workspacedomain.Scope{WorkspaceID: scope.WorkspaceID})
	if err != nil {
		return err
	}

	subject, body := alertMessage(alert)
	for _, member := range members {
		// Only the roles that can act get the mail. Telling everybody trains
		// people to ignore the message that matters.
		if member.Role != workspacedomain.RoleOwner && member.Role != workspacedomain.RoleAdmin {
			continue
		}
		if err := a.mailer.Send(ctx, mail.Message{
			To:      member.Email,
			Subject: subject,
			Text:    body,
		}); err != nil && a.logger != nil {
			a.logger.Warn("could not email a whatsapp alert",
				zap.String("email", member.Email), zap.Error(err))
		}
	}

	return nil
}

// alertMessage writes what a person reads.
func alertMessage(alert whatsappdomain.Alert) (string, string) {
	switch alert.Kind {
	case whatsappdomain.AlertNeedsRelink:
		return "Nomor WhatsApp perlu ditautkan ulang",
			"Nomor WhatsApp " + maskNumber(alert.Phone) + " tidak bisa dipakai lagi:\n\n" +
				alert.Reason + "\n\nBuka halaman Integrasi untuk menautkannya kembali."
	case whatsappdomain.AlertLimit:
		return "Nomor WhatsApp menahan kiriman",
			"Nomor WhatsApp " + maskNumber(alert.Phone) + " sudah mencapai batas kirim:\n\n" +
				alert.Reason + "\n\nPesannya tetap menunggu dan akan dikirim setelah batasnya longgar."
	default:
		return "Nomor WhatsApp terputus",
			"Nomor WhatsApp " + maskNumber(alert.Phone) + " sedang terputus:\n\n" +
				alert.Reason + "\n\nKalau tidak tersambung sendiri, tautkan ulang dari halaman Integrasi."
	}
}

// whatsAppReplies opens the task a customer's message asks for.
type whatsAppReplies struct {
	service taskdomain.Service
}

// Answer implements whatsappdomain.Replies.
func (r whatsAppReplies) Answer(ctx context.Context, scope whatsappdomain.Scope, request whatsappdomain.AnswerRequest) (uuid.UUID, error) {
	if r.service == nil {
		return uuid.Nil, fmt.Errorf("whatsapp: the task runtime is not configured")
	}

	// The trigger is `webhook`: the message came from outside the product, which
	// is what the trigger column means.
	dispatched, err := r.service.Dispatch(ctx, taskdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, taskdomain.DispatchRequest{
		AgentID: request.AgentID,
		Trigger: taskdomain.TriggerWebhook,
		Title:   request.Title,
		Prompt:  request.Prompt,
	})
	if err != nil {
		return uuid.Nil, err
	}

	return dispatched.ID, nil
}

// whatsAppWatchers starts the routines watching the message event.
//
// It is wired straight to the routine runtime, the way the integration hub's
// trigger adapter is: an event is one fact, and a routine watching it must start
// the same way whichever channel delivered it.
type whatsAppWatchers struct {
	routines routinedomain.Service
}

// Fire implements whatsappdomain.Watchers.
func (w whatsAppWatchers) Fire(ctx context.Context, scope whatsappdomain.Scope, event string) (int, error) {
	if w.routines == nil {
		return 0, nil
	}

	return w.routines.Fire(ctx, routinedomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, event)
}

// whatsAppMedia stores what a customer sent.
func (c *Container) whatsAppMedia() whatsappdomain.MediaStore {
	driver, err := openStorage(c.Config)
	if err != nil || driver == nil {
		return nil
	}

	return whatsAppMedia{store: driver}
}

type whatsAppMedia struct {
	store storage.Store
}

// Put implements whatsappdomain.MediaStore.
//
// The key names the workspace, so the bytes of one tenant are never reachable by
// guessing another's, and it names the message, so a stored object can be traced
// back to what it was.
func (m whatsAppMedia) Put(ctx context.Context, scope whatsappdomain.Scope, messageID uuid.UUID, media whatsappdomain.Media, content []byte) (whatsappdomain.StoredObject, error) {
	name := filenameForMedia(media.Kind, messageID)
	key := path.Join("whatsapp", scope.WorkspaceID.String(), messageID.String(), name)

	object, err := m.store.Put(ctx, key, contentTypeForMedia(media.Kind), content)
	if err != nil {
		return whatsappdomain.StoredObject{}, err
	}

	return whatsappdomain.StoredObject{
		Key:         object.Key,
		ContentType: object.ContentType,
		Filename:    name,
		Size:        object.ByteSize,
	}, nil
}

// Get implements whatsappdomain.MediaStore.
func (m whatsAppMedia) Get(ctx context.Context, key string) ([]byte, error) {
	return m.store.Get(ctx, key)
}

// filenameForMedia names a stored attachment.
func filenameForMedia(kind string, messageID uuid.UUID) string {
	extension := map[string]string{
		whatsappdomain.KindImage:   ".jpg",
		whatsappdomain.KindVideo:   ".mp4",
		whatsappdomain.KindAudio:   ".ogg",
		whatsappdomain.KindSticker: ".webp",
	}[kind]

	return kind + "-" + messageID.String() + extension
}

// contentTypeForMedia names what an attachment is.
func contentTypeForMedia(kind string) string {
	switch kind {
	case whatsappdomain.KindImage:
		return "image/jpeg"
	case whatsappdomain.KindVideo:
		return "video/mp4"
	case whatsappdomain.KindAudio:
		return "audio/ogg"
	case whatsappdomain.KindSticker:
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// maskNumber hides all but the last four digits, which is what an email and a
// log line both read.
func maskNumber(phone string) string {
	trimmed := strings.TrimSpace(phone)
	if len(trimmed) <= 4 {
		return strings.Repeat("•", len(trimmed))
	}
	return strings.Repeat("•", len(trimmed)-4) + trimmed[len(trimmed)-4:]
}

// adWattsApp exposes a linked number to the hub, so the model sees tools for it
// and a routine can watch its messages.
//
// The adapter is given a resolver rather than the service itself because the hub
// is assembled first: a linked number is a connection the hub shows, so the module
// that owns numbers cannot exist yet. An adapter holding a service captured here
// would hold nil forever, and every WhatsApp tool would answer "not configured"
// while the tools were still offered to the model — which teaches it to call them.
func adWattsApp(c *Container) integrationdomain.Adapter {
	return whatsappadapter.New(whatsAppNumbers{container: c})
}

// whatsAppNumbers resolves the module when it is called.
type whatsAppNumbers struct {
	container *Container
}

// live is the service this process has, or nil while it is still starting.
func (n whatsAppNumbers) live() whatsappdomain.Service {
	if n.container == nil || n.container.Services == nil {
		return nil
	}
	return n.container.Services.WhatsApp
}

// Enqueue implements the adapter's port.
func (n whatsAppNumbers) Enqueue(ctx context.Context, scope whatsappdomain.Scope, request whatsappdomain.EnqueueRequest) (whatsappdomain.Outbox, error) {
	service := n.live()
	if service == nil {
		return whatsappdomain.Outbox{}, fmt.Errorf("%w: the whatsapp module is not configured", whatsappdomain.ErrNotConfigured)
	}

	return service.Enqueue(ctx, scope, request)
}

// History implements the adapter's port.
func (n whatsAppNumbers) History(ctx context.Context, scope whatsappdomain.Scope, integrationID uuid.UUID, chatJID string, limit int) ([]whatsappdomain.Inbound, error) {
	service := n.live()
	if service == nil {
		return nil, fmt.Errorf("%w: the whatsapp module is not configured", whatsappdomain.ErrNotConfigured)
	}

	return service.History(ctx, scope, integrationID, chatJID, limit)
}

// NumberByIntegration implements the adapter's port.
func (n whatsAppNumbers) NumberByIntegration(ctx context.Context, scope whatsappdomain.Scope, integrationID uuid.UUID) (uuid.UUID, error) {
	service := n.live()
	if service == nil {
		return uuid.Nil, fmt.Errorf("%w: the whatsapp module is not configured", whatsappdomain.ErrNotConfigured)
	}

	return service.NumberByIntegration(ctx, scope, integrationID)
}

// Compile-time check that the resolver is the adapter's port.
var _ whatsappadapter.Numbers = whatsAppNumbers{}

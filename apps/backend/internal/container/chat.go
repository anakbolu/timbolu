package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	chatservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/pubsub"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/storage"
)

// openChat builds the conversation module: the repository, the object storage,
// the event publisher, and the responder that answers a message.
//
// The responder is the model-backed one for now. EPIC 6 replaces it with the
// durable agent loop by handing the service a different implementation; nothing
// else in the module changes when it does.
func (c *Container) openChat(_ context.Context, cfg *config.Config) error {
	if c.Repositories == nil || c.Repositories.Chat == nil {
		c.Logger.Warn("chat is disabled: no database connection")
		return nil
	}

	driver, err := openStorage(cfg)
	if err != nil {
		return err
	}
	if driver == nil {
		c.Logger.Warn("object storage is not configured: attachments and sandboxed content are unavailable")
	}

	var objectStore chatdomain.ObjectStore
	if driver != nil {
		objectStore = objectStorage{store: driver}
	}

	var publisher chatdomain.EventPublisher
	if c.Redis != nil {
		publisher = pubsub.New(c.Redis.Raw())
	} else {
		c.Logger.Warn("redis is not configured: live events reach the store but not connected clients")
	}

	gateway := c.Services.LLM

	deps := chatservice.Deps{
		Conversations: c.Repositories.Chat,
		Messages:      c.Repositories.Chat,
		Attachments:   c.Repositories.Chat,
		Events:        c.Repositories.Chat,
		Publisher:     publisher,
		Storage:       objectStore,
		Logger:        c.Logger,
	}

	if gateway != nil {
		deps.Responder = chatservice.NewModelResponder(chatservice.ResponderDeps{
			Gateway: gateway,
			Storage: objectStore,
		})
		if c.Services.Agent != nil {
			directory := agentDirectory{service: c.Services.Agent}
			deps.Agents = directory
			deps.Router = chatservice.NewGroupRouter(chatservice.RouterDeps{
				Gateway: gateway,
				Agents:  directory,
				Logger:  c.Logger,
			})
		}
	}

	// A message becomes a durable task when the runtime is configured, and is
	// answered in process otherwise. The task runtime needs the registry to know
	// which Bolu the message addresses, so both must be present.
	if c.Services.Task != nil && deps.Agents != nil {
		deps.Tasks = chatTaskStarter{service: c.Services.Task}
	} else {
		c.Logger.Warn("chat replies run in process: the task runtime is not configured, " +
			"so an answer does not survive a restart")
	}

	if deps.Responder == nil || deps.Agents == nil {
		// Without a gateway or a registry a message can be stored and read, but
		// nothing can answer it. Saying so is better than a Bolu that never
		// replies for no visible reason.
		c.Logger.Warn("chat replies are disabled: the model gateway or the agent registry is not configured")
	}

	c.Services.Chat = chatservice.New(deps)

	return nil
}

// openStorage builds the configured object storage driver.
//
// A missing configuration is not an error: the API runs, and only the endpoints
// that need storage report it as unavailable. That is what lets a developer run
// the API without a bucket.
func openStorage(cfg *config.Config) (storage.Store, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Storage.Driver)) {
	case "":
		return nil, nil

	case "local":
		store, err := storage.NewLocal(cfg.Storage.LocalRoot)
		if err != nil {
			return nil, fmt.Errorf("build local storage: %w", err)
		}
		return store, nil

	case "s3":
		store, err := storage.NewS3(context.Background(), storage.S3Config{
			Endpoint:  cfg.Storage.Endpoint,
			Region:    cfg.Storage.Region,
			Bucket:    cfg.Storage.Bucket,
			AccessKey: cfg.Storage.AccessKey,
			SecretKey: cfg.Storage.SecretKey,
			UseSSL:    cfg.Storage.UseSSL,
			PathStyle: cfg.Storage.PathStyle,
		})
		if err != nil {
			return nil, fmt.Errorf("build s3 storage: %w", err)
		}
		return store, nil

	default:
		return nil, fmt.Errorf("unknown storage driver %q", cfg.Storage.Driver)
	}
}

// objectStorage adapts the storage driver to the port the chat module declares.
//
// The adapter is a few lines because the two shapes differ in one thing: the
// module's port speaks in its own value type, so a driver change never reaches
// the module.
type objectStorage struct {
	store storage.Store
}

func (o objectStorage) Put(ctx context.Context, key, contentType string, content []byte) (chatdomain.StoredObject, error) {
	object, err := o.store.Put(ctx, key, contentType, content)
	if err != nil {
		return chatdomain.StoredObject{}, err
	}
	return chatdomain.StoredObject{
		Key:            object.Key,
		ContentType:    object.ContentType,
		ByteSize:       object.ByteSize,
		ChecksumSHA256: object.ChecksumSHA256,
	}, nil
}

func (o objectStorage) Get(ctx context.Context, key string) ([]byte, error) {
	return o.store.Get(ctx, key)
}

func (o objectStorage) Delete(ctx context.Context, key string) error {
	return o.store.Delete(ctx, key)
}

func (o objectStorage) Driver() string { return o.store.Driver() }

// agentDirectory adapts the agent registry to the port the chat module declares.
//
// The adapter lives here because the container is the only package allowed to
// know both sides: the chat module asks for "the Bolu of this workspace" and
// never learns which module answers.
type agentDirectory struct {
	service agentdomain.Service
}

// Agent implements chatdomain.AgentDirectory.
func (d agentDirectory) Agent(ctx context.Context, scope chatdomain.Scope, id uuid.UUID) (chatdomain.AgentRef, error) {
	agent, err := d.service.Get(ctx, agentScope(scope), id)
	if err != nil {
		return chatdomain.AgentRef{}, err
	}
	return toAgentRef(agent), nil
}

// Agents implements chatdomain.AgentDirectory.
func (d agentDirectory) Agents(ctx context.Context, scope chatdomain.Scope, ids []uuid.UUID) ([]chatdomain.AgentRef, error) {
	refs := make([]chatdomain.AgentRef, 0, len(ids))
	for _, id := range ids {
		agent, err := d.service.Get(ctx, agentScope(scope), id)
		if err != nil {
			return nil, err
		}
		refs = append(refs, toAgentRef(agent))
	}
	return refs, nil
}

// Active implements chatdomain.AgentDirectory. A resting Bolu takes no new work,
// which is the same rule the registry applies to its own routes.
func (d agentDirectory) Active(ref chatdomain.AgentRef) bool {
	return agentdomain.AcceptsNewTasks(ref.Stored)
}

func agentScope(scope chatdomain.Scope) agentdomain.Scope {
	return agentdomain.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

func toAgentRef(agent agentdomain.Agent) chatdomain.AgentRef {
	return chatdomain.AgentRef{
		ID:           agent.ID,
		Name:         agent.Name,
		Role:         agent.Role,
		Persona:      agent.Persona,
		Tone:         agent.Tone,
		Stored:       agent.Status,
		Display:      agent.Display,
		Tools:        agent.Tools,
		DefaultModel: agent.DefaultModel,
	}
}

// compile-time checks that the adapters satisfy the ports they are wired to.
var (
	_ chatdomain.AgentDirectory = agentDirectory{}
	_ chatdomain.ObjectStore    = objectStorage{}
)

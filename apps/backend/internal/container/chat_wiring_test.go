package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	agentdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/agent/domain/mocks"
	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	"github.com/anakbolu/timbolu/packages/platform/config"
	"github.com/anakbolu/timbolu/packages/platform/storage"
)

// TestOpenStorageSelectsTheDriver covers the three configurations a deployment
// can be in, and the one that is a mistake.
func TestOpenStorageSelectsTheDriver(t *testing.T) {
	t.Run("no driver means no storage", func(t *testing.T) {
		store, err := openStorage(&config.Config{})
		require.NoError(t, err)
		require.Nil(t, store, "a deployment without storage still serves the rest of the API")
	})

	t.Run("local writes to a directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "objects")
		store, err := openStorage(&config.Config{Storage: config.StorageConfig{
			Driver:    "local",
			LocalRoot: root,
		}})
		require.NoError(t, err)
		require.Equal(t, "local", store.Driver())

		info, err := os.Stat(root)
		require.NoError(t, err)
		require.True(t, info.IsDir(), "the root is created, so a fresh checkout works")
	})

	t.Run("local without a root is an error", func(t *testing.T) {
		_, err := openStorage(&config.Config{Storage: config.StorageConfig{Driver: "local"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "local storage")
	})

	t.Run("s3 without an endpoint is an error", func(t *testing.T) {
		_, err := openStorage(&config.Config{Storage: config.StorageConfig{Driver: "s3"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "s3 storage")
	})

	t.Run("an unknown driver is refused", func(t *testing.T) {
		_, err := openStorage(&config.Config{Storage: config.StorageConfig{Driver: "ftp"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "ftp")
	})
}

// TestObjectStorageAdapterIsAThinTranslation keeps the adapter's job narrow: it
// speaks the module's port and nothing else.
func TestObjectStorageAdapterIsAThinTranslation(t *testing.T) {
	local, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)

	adapter := objectStorage{store: local}
	ctx := context.Background()

	object, err := adapter.Put(ctx, "content/a/index.html", "text/html", []byte("<h1>halo</h1>"))
	require.NoError(t, err)
	require.Equal(t, "content/a/index.html", object.Key)
	require.Equal(t, int64(13), object.ByteSize)
	require.Len(t, object.ChecksumSHA256, 64)
	require.Equal(t, "local", adapter.Driver())

	content, err := adapter.Get(ctx, object.Key)
	require.NoError(t, err)
	require.Equal(t, "<h1>halo</h1>", string(content))

	require.NoError(t, adapter.Delete(ctx, object.Key))
	_, err = adapter.Get(ctx, object.Key)
	require.Error(t, err)
}

// TestAgentDirectoryIsolatesTheModules is the point of the adapter: the chat
// module asks for a Bolu and never learns which module answers.
func TestAgentDirectoryIsolatesTheModules(t *testing.T) {
	service := mocks.NewService(t)
	directory := agentDirectory{service: service}

	agentID := uuid.New()
	agent := agentdomain.Agent{
		ID:          agentID,
		WorkspaceID: uuid.New(),
		Name:        "Oren",
		Role:        "Penjualan",
		Persona:     "Ramah.",
		Tone:        "hangat",
		Status:      agentdomain.StoredActive,
		Display:     agentdomain.DisplayWorking,
		Tools:       []string{"gmail.search"},
		DefaultModel: map[string]any{
			"model": "gpt-4o-mini",
		},
	}

	scope := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: agent.WorkspaceID}
	service.EXPECT().Get(mock.Anything, mock.Anything, agentID).Return(agent, nil).Once()

	ref, err := directory.Agent(context.Background(), scope, agentID)
	require.NoError(t, err)
	require.Equal(t, agentID, ref.ID)
	require.Equal(t, "Oren", ref.Name)
	require.Equal(t, "Penjualan", ref.Role)
	require.Equal(t, agentdomain.StoredActive, ref.Stored)
	require.Equal(t, agentdomain.DisplayWorking, ref.Display)
	require.Equal(t, []string{"gmail.search"}, ref.Tools)
	require.Equal(t, "gpt-4o-mini", ref.DefaultModel["model"])

	// The registry's own error travels through unchanged, so the handler maps it
	// with the same code it uses for its own routes.
	service.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).Return(agentdomain.Agent{}, agentdomain.ErrAgentNotFound).Once()
	_, err = directory.Agent(context.Background(), scope, uuid.New())
	require.ErrorIs(t, err, agentdomain.ErrAgentNotFound)
}

// TestAgentDirectoryReadsSeveralBolu keeps the order the caller asked for, which
// is the order a group lists its participants in.
func TestAgentDirectoryReadsSeveralBolu(t *testing.T) {
	service := mocks.NewService(t)
	directory := agentDirectory{service: service}

	first, second := uuid.New(), uuid.New()
	scope := chatdomain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()}

	service.EXPECT().Get(mock.Anything, mock.Anything, first).
		Return(agentdomain.Agent{ID: first, Name: "Oren", Status: agentdomain.StoredActive}, nil).Once()
	service.EXPECT().Get(mock.Anything, mock.Anything, second).
		Return(agentdomain.Agent{ID: second, Name: "Biru", Status: agentdomain.StoredActive}, nil).Once()

	refs, err := directory.Agents(context.Background(), scope, []uuid.UUID{first, second})
	require.NoError(t, err)
	require.Len(t, refs, 2)
	require.Equal(t, "Oren", refs[0].Name)
	require.Equal(t, "Biru", refs[1].Name)

	// One missing Bolu fails the whole read rather than silently shortening the
	// list, which would make a group appear to have fewer participants.
	service.EXPECT().Get(mock.Anything, mock.Anything, mock.Anything).
		Return(agentdomain.Agent{}, errors.New("registry is down")).Once()
	_, err = directory.Agents(context.Background(), scope, []uuid.UUID{uuid.New()})
	require.Error(t, err)
}

// TestAgentDirectoryAppliesTheRestSwitch: a resting Bolu takes no new work, which
// is the same rule the registry applies to its own routes.
func TestAgentDirectoryAppliesTheRestSwitch(t *testing.T) {
	directory := agentDirectory{}

	require.True(t, directory.Active(chatdomain.AgentRef{Stored: agentdomain.StoredActive}))
	require.False(t, directory.Active(chatdomain.AgentRef{Stored: agentdomain.StoredResting}))
	require.False(t, directory.Active(chatdomain.AgentRef{Stored: ""}), "an unknown switch is not a yes")
}

// TestOpenChatWithoutADatabaseLeavesTheModuleUnwired keeps the container's
// "running without Postgres" contract: the routes answer 503 rather than
// panicking.
func TestOpenChatWithoutADatabaseLeavesTheModuleUnwired(t *testing.T) {
	c := &Container{
		Config: testConfig(),
		Logger: zap.NewNop(),
		// The aggregates are built by New; a container under test gets the ones
		// this path touches.
		Repositories: &Repositories{},
		Services:     &Services{},
	}

	require.NoError(t, c.openChat(context.Background(), c.Config))
	require.Nil(t, c.Services.Chat, "the chat service stays nil, so its routes report not configured")
}

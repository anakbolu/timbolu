package handler

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// newTestApp mounts the memory routes behind the identity the container's
// middlewares would have put on the request.
func newTestApp(t *testing.T, service domain.Service, workspaceID uuid.UUID) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			var fiberErr *fiber.Error
			code := fiber.StatusInternalServerError
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return response.Error(c, code, err.Error(), "http_error", nil)
		},
	})

	group := app.Group("/api", func(c *fiber.Ctx) error {
		userID := uuid.New()
		middleware.SetIdentity(c, userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: userID, WorkspaceID: workspaceID})
		return c.Next()
	})

	Routes(group, New(service, zap.NewNop()))
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	recorder := httptest.NewRecorder()
	recorder.Code = resp.StatusCode
	if content, err := io.ReadAll(resp.Body); err == nil {
		recorder.Body.Write(content)
	}
	return recorder
}

func memoryFixture() domain.Memory {
	return domain.Memory{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Content:     "Jam buka 08.00-21.00",
		Kind:        domain.KindFact,
		CreatedBy:   domain.SourceUser,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
}

// TestListMemories returns the layer the caller asked for, and answers with the
// count so the screen can say how many there are.
func TestListMemories(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Memories(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.MemoryFilter) bool {
			return filter.Main && filter.Query == "buka"
		})).
		Return([]domain.Memory{memoryFixture()}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/memories?layer=main&q=buka", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "Jam buka 08.00-21.00")
	require.Contains(t, resp.Body.String(), `"count":1`)
}

// TestListMemoriesWithLayerMainById: "main" is what the screen sends when the
// reader picked the workspace layer, so it must not be handed to a uuid parser.
func TestListMemoriesMainLayerByWord(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Memories(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.MemoryFilter) bool {
			return filter.Main && filter.AgentID == uuid.Nil
		})).
		Return(nil, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/memories?agent_id=main", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
}

// TestAddMemoryStoresWhatWasSent: the endpoint returns the stored row, which is
// what the screen shows after a write.
func TestAddMemory(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Add(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.AddRequest) bool {
			return request.Content == "Ongkir Bandung 15 ribu" && request.Kind == domain.KindFact
		})).
		Return(memoryFixture(), nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/memories",
		`{"content":"Ongkir Bandung 15 ribu","kind":"fact"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.Contains(t, resp.Body.String(), "Jam buka 08.00-21.00")
}

// TestAddMemoryRefusesASecret is the E10.7 gate at the HTTP boundary: the
// refusal is a 422 with a code the screen can explain, not a 500.
func TestAddMemoryRefusesASecret(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Add(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Memory{}, domain.ErrSensitive).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/memories",
		`{"content":"Nomor kartu 4111 1111 1111 1111"}`)

	require.Equal(t, fiber.StatusUnprocessableEntity, resp.Code)
	require.Contains(t, resp.Body.String(), "sensitive_content")

	// And the message reads as a sentence about what was typed, not as this
	// module's own error string: the package name and the rule's internals are not
	// something a person can act on.
	message := strings.SplitN(resp.Body.String(), `"message":"`, 2)[1]
	require.NotContains(t, message, "memory:")
	require.NotContains(t, message, "is sensitive")
}

// TestAddMemoryReportsAMissingEmbeddingProvider: the request was fine and the
// platform is not ready, which is a 503 rather than a 400.
func TestAddMemoryReportsAMissingEmbeddingProvider(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Add(mock.Anything, mock.Anything, mock.Anything).
		Return(domain.Memory{}, domain.ErrNoEmbedding).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/memories", `{"content":"Jam buka 08.00"}`)

	require.Equal(t, fiber.StatusServiceUnavailable, resp.Code)
	require.Contains(t, resp.Body.String(), "memory_unavailable")
}

// TestEditMemoryMovesALayer: the screen's move action has two shapes — a Bolu id
// and the word "main" — and both have to reach the service as a move.
func TestEditMemoryMovesALayer(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	id := uuid.New()

	service.EXPECT().
		Edit(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.EditRequest) bool {
			return request.ID == id && request.MoveToMain
		})).
		Return(memoryFixture(), nil).
		Once()

	resp := do(t, app, fiber.MethodPatch, "/api/memories/"+id.String(), `{"agent_id":"main"}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
}

// TestEditMemoryPinsWithoutTouchingTheText: an absent field must mean "leave it
// alone", which is why the request carries pointers.
func TestEditMemoryPinsWithoutTouchingTheText(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	id := uuid.New()

	service.EXPECT().
		Edit(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.EditRequest) bool {
			return request.Content == nil && request.Pinned != nil && *request.Pinned
		})).
		Return(memoryFixture(), nil).
		Once()

	resp := do(t, app, fiber.MethodPatch, "/api/memories/"+id.String(), `{"pinned":true}`)
	require.Equal(t, fiber.StatusOK, resp.Code)
}

// TestForgetMemoryIsAPermanentDelete: the endpoint reports "deleted" and the
// module's own not-found is a 404.
func TestForgetMemory(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	id := uuid.New()

	service.EXPECT().Forget(mock.Anything, mock.Anything, id).Return(nil).Once()
	resp := do(t, app, fiber.MethodDelete, "/api/memories/"+id.String(), "")
	require.Equal(t, fiber.StatusOK, resp.Code)

	service.EXPECT().Forget(mock.Anything, mock.Anything, mock.Anything).Return(domain.ErrNotFound).Once()
	resp = do(t, app, fiber.MethodDelete, "/api/memories/"+uuid.NewString(), "")
	require.Equal(t, fiber.StatusNotFound, resp.Code)
}

// TestIngestDocument: the upload endpoint returns the chunks that were stored
// and embedded.
func TestIngestDocument(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Ingest(mock.Anything, mock.Anything, mock.MatchedBy(func(request domain.IngestRequest) bool {
			return request.Source == "katalog.pdf" && strings.Contains(request.Text, "Ongkir")
		})).
		Return([]domain.KnowledgeItem{{
			ID:         uuid.New(),
			Source:     "katalog.pdf",
			ChunkIndex: 0,
			Content:    "Ongkir Bandung 15 ribu",
			Status:     domain.StatusReady,
			CreatedAt:  time.Now(),
		}}, nil).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/knowledge",
		`{"source":"katalog.pdf","text":"Ongkir Bandung 15 ribu"}`)

	require.Equal(t, fiber.StatusCreated, resp.Code)
	require.Contains(t, resp.Body.String(), `"status":"ready"`)
}

// TestListKnowledgeReportsAFailedChunk: an upload that did not work is something
// a person has to be able to see, so the failed rows are in the list.
func TestListKnowledgeReportsAFailedChunk(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Knowledge(mock.Anything, mock.Anything, mock.MatchedBy(func(filter domain.KnowledgeFilter) bool {
			return filter.Status == domain.StatusFailed
		})).
		Return([]domain.KnowledgeItem{{
			ID:        uuid.New(),
			Source:    "katalog.pdf",
			Content:   "Ongkir Bandung 15 ribu",
			Status:    domain.StatusFailed,
			CreatedAt: time.Now(),
		}}, nil).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/knowledge?status=failed", "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"status":"failed"`)
}

// TestForgetKnowledgeAndItsErrors covers the delete paths the screen offers.
func TestForgetKnowledge(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())
	id := uuid.New()

	service.EXPECT().ForgetKnowledge(mock.Anything, mock.Anything, id).Return(nil).Once()
	require.Equal(t, fiber.StatusOK, do(t, app, fiber.MethodDelete, "/api/knowledge/"+id.String(), "").Code)

	service.EXPECT().ForgetKnowledge(mock.Anything, mock.Anything, mock.Anything).Return(domain.ErrNotFound).Once()
	require.Equal(t, fiber.StatusNotFound, do(t, app, fiber.MethodDelete, "/api/knowledge/"+uuid.NewString(), "").Code)
}

// TestKnowledgeReportsAMissingEmbeddingProvider is the same 503 a memory write
// gets: the request was fine, the platform cannot embed.
func TestKnowledgeReportsAMissingEmbeddingProvider(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Ingest(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, domain.ErrNoEmbedding).
		Once()

	resp := do(t, app, fiber.MethodPost, "/api/knowledge", `{"source":"a.txt","text":"isi"}`)
	require.Equal(t, fiber.StatusServiceUnavailable, resp.Code)
	require.Contains(t, resp.Body.String(), "memory_unavailable")
}

// TestAnUnexpectedFailureIsAnInternalError: an error the handler does not
// recognise is a 500 with a code the client can branch on, and the detail stays
// in the log.
func TestAnUnexpectedFailureIsAnInternalError(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	service.EXPECT().
		Memories(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("the database is on fire")).
		Once()

	resp := do(t, app, fiber.MethodGet, "/api/memories", "")
	require.Equal(t, fiber.StatusInternalServerError, resp.Code)
	require.NotContains(t, resp.Body.String(), "on fire", "the detail must not leave the server")
}

// TestMalformedBodiesAreRejected: a body that is not JSON is a 400 before the
// service is asked anything.
func TestMalformedBodiesAreRejected(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	require.Equal(t, fiber.StatusBadRequest,
		do(t, app, fiber.MethodPost, "/api/memories", "{").Code)
	require.Equal(t, fiber.StatusBadRequest,
		do(t, app, fiber.MethodPost, "/api/knowledge", "{").Code)
	require.Equal(t, fiber.StatusBadRequest,
		do(t, app, fiber.MethodPost, "/api/memories", `{"agent_id":"bukan-uuid","content":"x"}`).Code)
	require.Equal(t, fiber.StatusBadRequest,
		do(t, app, fiber.MethodPatch, "/api/memories/"+uuid.NewString(), `{"agent_id":"bukan-uuid"}`).Code)
}

// TestBadIdentifiersAreRejected: a malformed id is the caller's problem.
func TestBadIdentifiersAreRejected(t *testing.T) {
	service := mocks.NewService(t)
	app := newTestApp(t, service, uuid.New())

	require.Equal(t, fiber.StatusBadRequest,
		do(t, app, fiber.MethodDelete, "/api/memories/bukan-uuid", "").Code)
	require.Equal(t, fiber.StatusBadRequest,
		do(t, app, fiber.MethodGet, "/api/memories?agent_id=bukan-uuid", "").Code)
	require.Equal(t, fiber.StatusBadRequest,
		do(t, app, fiber.MethodPatch, "/api/memories/"+uuid.NewString(), "{").Code)
}

package handler

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/middleware"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// streamIdentity is what the container's stream middleware stores on a request
// that authenticated from the query string.
type streamIdentity struct {
	userID      uuid.UUID
	workspaceID uuid.UUID
}

// newStreamApp mounts the stream routes on an app with no server timeouts,
// which is all the tests that only read the response need.
func newStreamApp(t *testing.T, service chatdomain.Service, id streamIdentity) *fiber.App {
	t.Helper()
	return newStreamAppWith(t, service, id, fiber.Config{}, chatdomain.Heartbeat)
}

// newStreamAppWith builds the same app under a given server configuration and
// heartbeat. Both are inputs because they are what a stream has to outlive, and
// a test cannot wait out the production values.
func newStreamAppWith(
	t *testing.T, service chatdomain.Service, id streamIdentity, config fiber.Config, heartbeat time.Duration,
) *fiber.App {
	t.Helper()

	config.DisableStartupMessage = true
	config.ErrorHandler = func(c *fiber.Ctx, err error) error {
		var fiberErr *fiber.Error
		code := fiber.StatusInternalServerError
		if errors.As(err, &fiberErr) {
			code = fiberErr.Code
		}
		return response.Error(c, code, err.Error(), "http_error", nil)
	}

	app := fiber.New(config)

	handler := New(service, zap.NewNop()).WithHeartbeat(heartbeat)

	group := app.Group("/api", func(c *fiber.Ctx) error {
		middleware.SetIdentity(c, id.userID, "owner@example.com")
		middleware.SetScope(c, database.Scope{UserID: id.userID, WorkspaceID: id.workspaceID})
		return c.Next()
	})

	StreamRoutes(group, handler)
	return app
}

// TestSSEStreamsTheBacklogAndTheLiveEvents is the E5.7 gate on the wire format:
// each frame carries its event id, which is what a reconnect resumes from.
func TestSSEStreamsTheBacklogAndTheLiveEvents(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}
	conversationID := uuid.New()

	events := make(chan chatdomain.Event, 4)
	events <- chatdomain.Event{
		ID:             7,
		WorkspaceID:    id.workspaceID,
		Type:           chatdomain.EventMessageNew,
		ConversationID: conversationID,
		Payload:        []byte(`{"message_id":"a"}`),
		CreatedAt:      time.Now(),
	}
	events <- chatdomain.Event{
		ID:             8,
		WorkspaceID:    id.workspaceID,
		Type:           chatdomain.EventAgentState,
		ConversationID: conversationID,
		Payload:        []byte(`{"agent_id":"b","state":"working"}`),
		CreatedAt:      time.Now(),
	}
	close(events)

	// The resume point arrives in the header the browser sends on its own.
	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(6)).Return(events, nil).Once()

	app := newStreamApp(t, service, id)

	req := httptest.NewRequest(fiber.MethodGet, "/api/events/stream", nil)
	req.Header.Set("Last-Event-ID", "6")
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Contains(t, resp.Header.Get("Cache-Control"), "no-cache")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)

	require.Contains(t, text, "id: 7\n")
	require.Contains(t, text, "event: message.new\n")
	require.Contains(t, text, `"message_id":"a"`)
	require.Contains(t, text, "id: 8\n")
	require.Contains(t, text, "event: agent.state\n")
	require.Contains(t, text, `"state":"working"`)

	// The frames are separated by a blank line, which is what ends an event.
	require.Contains(t, text, "\n\n")
}

// TestSSEOutlivesTheServerWriteTimeout is the issue #255 gate: the server's
// WriteTimeout bounds one write, not the life of the stream. The app runs on a
// real listener with a 300ms timeout, so a stream that kept the server's single
// deadline would be cut at the first keep-alive after that; this one is read
// across three timeouts, and then an event is written and must still arrive.
func TestSSEOutlivesTheServerWriteTimeout(t *testing.T) {
	const (
		writeTimeout = 300 * time.Millisecond
		heartbeat    = 50 * time.Millisecond
	)

	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	events := make(chan chatdomain.Event)
	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(0)).Return(events, nil).Once()

	app := newStreamAppWith(t, service, id, fiber.Config{WriteTimeout: writeTimeout}, heartbeat)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		close(events)
		_ = app.Shutdown()
	})

	go func() { _ = app.Listener(listener) }()

	// A deadline on the request bounds the test even if the stream goes silent:
	// a read of a response whose context is done fails instead of blocking.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, fiber.MethodGet,
		"http://"+listener.Addr().String()+"/api/events/stream", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	reader := bufio.NewReader(resp.Body)
	start := time.Now()
	var keepAlives strings.Builder
	for time.Since(start) < 3*writeTimeout {
		line, err := reader.ReadString('\n')
		require.NoError(t, err, "the stream was cut after %v", time.Since(start))
		keepAlives.WriteString(line)
	}
	require.Contains(t, keepAlives.String(), ": keep-alive",
		"the stream must still be written to after three write timeouts")

	// An event written past the server's timeout still arrives, which is what
	// makes this a live stream rather than a connection held open by silence.
	events <- chatdomain.Event{
		ID:             9,
		WorkspaceID:    id.workspaceID,
		Type:           chatdomain.EventMessageNew,
		ConversationID: uuid.New(),
		Payload:        []byte(`{"message_id":"late"}`),
		CreatedAt:      time.Now(),
	}

	var frame strings.Builder
	for !strings.Contains(frame.String(), "late") {
		line, err := reader.ReadString('\n')
		require.NoError(t, err, "the stream ended after %v without the event", time.Since(start))
		frame.WriteString(line)
	}
	require.Contains(t, frame.String(), "id: 9\n")
	require.Contains(t, frame.String(), "event: message.new\n")
}

// TestAPlainResponseStillHonoursTheServerWriteTimeout guards the other half of
// issue #255: only the stream renews its deadline. A response whose body the
// client stops taking is still cut one WriteTimeout after the server starts
// writing it, so the fix cannot be mistaken for "the API no longer has a write
// timeout".
func TestAPlainResponseStillHonoursTheServerWriteTimeout(t *testing.T) {
	const writeTimeout = 300 * time.Millisecond
	// Larger than any combination of send and receive buffers a test host
	// gives a socket, so the server's write has to block for the deadline to
	// be what ends it.
	body := bytes.Repeat([]byte("x"), 16<<20)

	app := fiber.New(fiber.Config{DisableStartupMessage: true, WriteTimeout: writeTimeout})
	app.Get("/slow-reader", func(c *fiber.Ctx) error { return c.Send(body) })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Shutdown() })

	go func() { _ = app.Listener(listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, fiber.MethodGet,
		"http://"+listener.Addr().String()+"/slow-reader", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	// The client takes nothing while the server writes: the write blocks, the
	// deadline passes, and the response is cut rather than completed.
	time.Sleep(2 * writeTimeout)

	read, err := io.Copy(io.Discard, resp.Body)
	require.Error(t, err, "a stalled reader must lose the response to the write timeout")
	require.Less(t, read, int64(len(body)), "the whole body arrived, so nothing bounded the write")
}

// TestSocketOutlivesTheServerWriteTimeout covers the handler's other long-lived
// stream. A WebSocket is hijacked, so fasthttp's per-response write deadline no
// longer applies to it; this holds the socket open across three write timeouts
// and then reads an event, which is what keeps that true.
func TestSocketOutlivesTheServerWriteTimeout(t *testing.T) {
	const (
		writeTimeout = 300 * time.Millisecond
		heartbeat    = 50 * time.Millisecond
	)

	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	events := make(chan chatdomain.Event)
	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(0)).Return(events, nil).Once()

	app := newStreamAppWith(t, service, id, fiber.Config{WriteTimeout: writeTimeout}, heartbeat)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		close(events)
		_ = app.Shutdown()
	})

	go func() { _ = app.Listener(listener) }()

	conn, resp, err := websocket.DefaultDialer.Dial(
		"ws://"+listener.Addr().String()+"/api/events/socket", nil)
	require.NoError(t, err)
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Nothing but the stream's own keep-alives crosses the socket for three
	// write timeouts.
	time.Sleep(3 * writeTimeout)

	events <- chatdomain.Event{
		ID:             11,
		WorkspaceID:    id.workspaceID,
		Type:           chatdomain.EventMessageNew,
		ConversationID: uuid.New(),
		Payload:        []byte(`{"message_id":"socket"}`),
		CreatedAt:      time.Now(),
	}

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))

	var frame map[string]any
	require.NoError(t, conn.ReadJSON(&frame), "the socket was cut by the write timeout")
	require.Equal(t, float64(11), frame["id"])
	require.Equal(t, chatdomain.EventMessageNew, frame["type"])
}

// TestSSELeavesTheConnectionUsableAfterTheStreamEnds: the stream renews the
// connection's write deadline, and the connection outlives the stream. This
// runs with WriteTimeout unset, which is the case where the server puts no
// deadline of its own on the next request, so a deadline the stream left behind
// would be the only one there — and it is expired by the time the client comes
// back.
func TestSSELeavesTheConnectionUsableAfterTheStreamEnds(t *testing.T) {
	const heartbeat = 50 * time.Millisecond

	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	events := make(chan chatdomain.Event)
	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(0)).Return(events, nil).Once()

	app := newStreamAppWith(t, service, id, fiber.Config{}, heartbeat)
	app.Get("/api/ping", func(c *fiber.Ctx) error { return c.SendString("pong") })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Shutdown() })

	go func() { _ = app.Listener(listener) }()

	base := "http://" + listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(base + "/api/events/stream")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	// The stream ends on its own, which completes the response and leaves the
	// connection idle.
	go func() {
		time.Sleep(3 * heartbeat)
		close(events)
	}()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), ": keep-alive")

	// Long enough that the stream's own deadline has passed.
	time.Sleep(2 * heartbeat)

	var last httptrace.GotConnInfo
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { last = info },
	}
	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(context.Background(), trace), fiber.MethodGet, base+"/api/ping", nil)
	require.NoError(t, err)

	next, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = next.Body.Close() })
	require.Equal(t, fiber.StatusOK, next.StatusCode)

	content, err := io.ReadAll(next.Body)
	require.NoError(t, err)
	require.Equal(t, "pong", string(content))
	require.True(t, last.Reused, "the request must reuse the stream's connection to prove it survived")
}

// TestSSEResumesFromTheQueryParameter covers the client that reconnects
// explicitly rather than through the browser's own header.
func TestSSEResumesFromTheQueryParameter(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	events := make(chan chatdomain.Event)
	close(events)

	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(42)).Return(events, nil).Once()

	app := newStreamApp(t, service, id)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/stream?last_event_id=42", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// TestSSEIgnoresAnUnreadableResumePoint: a malformed id starts from the
// beginning rather than failing the connection.
func TestSSEIgnoresAnUnreadableResumePoint(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	events := make(chan chatdomain.Event)
	close(events)

	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(0)).Return(events, nil).Once()

	app := newStreamApp(t, service, id)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/stream?last_event_id=bukan-angka", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

// TestSSESendsItsHeadBeforeAnyEvent is the gate on the open delay: fasthttp
// writes nothing until the first flush, so a stream with nothing to replay — the
// ordinary reconnect, whose cursor is already the newest event — used to leave
// the client "connecting" until the first heartbeat, 25s later. The response
// head and the opening comment must reach the client on their own.
func TestSSESendsItsHeadBeforeAnyEvent(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	// The stream stays silent: no backlog, no live event, no close.
	events := make(chan chatdomain.Event)
	service.EXPECT().Stream(mock.Anything, mock.Anything, int64(9)).Return(events, nil).Once()

	app := newStreamApp(t, service, id)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(listener) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	// A real connection, not app.Test: the helper waits for the handler to
	// return before it reads the response, which is exactly the delay under
	// test.
	req, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/api/events/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Last-Event-ID", "9")

	client := &http.Client{Timeout: 5 * time.Second}

	start := time.Now()
	resp, err := client.Do(req)
	require.NoError(t, err)
	opened := time.Since(start)

	var once sync.Once
	stop := func() { once.Do(func() { close(events) }) }
	t.Cleanup(stop)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Less(t, opened, time.Second, "the response head waited for an event")

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, ": connected\n", line, "the stream opens with a comment, not an event")

	// Let the handler return before the server shuts down.
	stop()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// TestSocketRefusesAPlainRequest keeps the WebSocket endpoint honest about what
// it speaks, instead of answering with an empty 200.
func TestSocketRefusesAPlainRequest(t *testing.T) {
	service := mocks.NewService(t)

	app := newStreamApp(t, service, streamIdentity{userID: uuid.New(), workspaceID: uuid.New()})
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/socket", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusUpgradeRequired, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upgrade_required")
	require.Contains(t, string(body), "/events/stream", "the refusal names the fallback")
}

// TestStreamFailureIsReported keeps a broken stream a visible error.
func TestStreamFailureIsReported(t *testing.T) {
	service := mocks.NewService(t)
	id := streamIdentity{userID: uuid.New(), workspaceID: uuid.New()}

	service.EXPECT().Stream(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("the live stream is not configured")).Once()

	app := newStreamApp(t, service, id)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/api/events/stream", nil), -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
}

// TestUploadStoresAFile covers the multipart path: the file is read, bounded, and
// handed to the service, and the response carries a URL a client can follow.
func TestUploadStoresAFile(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversationID := uuid.New()
	messageID := uuid.New()
	attachment := chatdomain.Attachment{
		ID:          uuid.New(),
		MessageID:   messageID,
		Filename:    "daftar harga.pdf",
		ContentType: "application/pdf",
		ByteSize:    5,
	}

	service.EXPECT().Attach(mock.Anything, mock.Anything, mock.MatchedBy(func(req chatdomain.AttachRequest) bool {
		return req.MessageID == messageID && req.Filename == "daftar harga.pdf"
	}), []byte("harga")).Return(attachment, nil).Once()

	app := newTestApp(t, service, id)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "daftar harga.pdf")
	require.NoError(t, err)
	_, err = part.Write([]byte("harga"))
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("message_id", messageID.String()))
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(fiber.MethodPost,
		"/api/conversations/"+conversationID.String()+"/attachments", &body)
	req.Header.Set(fiber.HeaderContentType, writer.FormDataContentType())

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusCreated, resp.StatusCode)

	content, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(content), `"filename":"daftar harga.pdf"`)
	require.Contains(t, string(content), `"url":"/api/attachments/`+attachment.ID.String()+`"`)
	require.Contains(t, string(content), `"byte_size":5`)
}

// TestUploadWithoutAMessageOpensOne: an upload may precede the send, so the
// server opens the message the file belongs to.
func TestUploadWithoutAMessageOpensOne(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversationID := uuid.New()
	attachment := chatdomain.Attachment{ID: uuid.New(), MessageID: uuid.New(), Filename: "a.txt"}

	service.EXPECT().Attach(mock.Anything, mock.Anything, mock.MatchedBy(func(req chatdomain.AttachRequest) bool {
		return req.ConversationID == conversationID && req.MessageID == uuid.Nil
	}), mock.Anything).Return(attachment, nil).Once()

	app := newTestApp(t, service, id)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "a.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("a"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(fiber.MethodPost,
		"/api/conversations/"+conversationID.String()+"/attachments", &body)
	req.Header.Set(fiber.HeaderContentType, writer.FormDataContentType())

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusCreated, resp.StatusCode)
}

// TestUploadRequiresAFile keeps the attachment endpoint from accepting an empty
// multipart body.
func TestUploadRequiresAFile(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}

	app := newTestApp(t, service, id)

	req := httptest.NewRequest(fiber.MethodPost, "/api/conversations/"+uuid.NewString()+"/attachments",
		strings.NewReader("--boundary--"))
	req.Header.Set(fiber.HeaderContentType, "multipart/form-data; boundary=boundary")

	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

// TestAddParticipantsValidatesTheBody covers the group management endpoint.
func TestAddParticipantsValidatesTheBody(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversation := conversationFixture()
	conversation.Kind = chatdomain.KindGroup
	conversation.Title = "Grup operasional"
	added := uuid.New()

	service.EXPECT().AddParticipants(mock.Anything, mock.Anything, conversation.ID,
		mock.MatchedBy(func(req chatdomain.AddParticipantsRequest) bool {
			return len(req.AgentIDs) == 1 && req.AgentIDs[0] == added
		})).Return(conversation, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodPost, "/api/conversations/"+conversation.ID.String()+"/participants",
		`{"agent_ids":["`+added.String()+`"]}`)

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), "Grup operasional")

	bad := do(t, app, fiber.MethodPost, "/api/conversations/"+conversation.ID.String()+"/participants",
		`{"agent_ids":["bukan-uuid"]}`)
	require.Equal(t, fiber.StatusBadRequest, bad.Code)
}

// TestGetConversation covers the single read.
func TestGetConversation(t *testing.T) {
	service := mocks.NewService(t)
	id := identity{userID: uuid.New(), workspaceID: uuid.New()}
	conversation := conversationFixture()

	service.EXPECT().Conversation(mock.Anything, mock.Anything, conversation.ID).
		Return(conversation, nil).Once()

	app := newTestApp(t, service, id)
	resp := do(t, app, fiber.MethodGet, "/api/conversations/"+conversation.ID.String(), "")

	require.Equal(t, fiber.StatusOK, resp.Code)
	require.Contains(t, resp.Body.String(), `"kind":"direct"`)

	bad := do(t, app, fiber.MethodGet, "/api/conversations/bukan-uuid", "")
	require.Equal(t, fiber.StatusBadRequest, bad.Code)
}

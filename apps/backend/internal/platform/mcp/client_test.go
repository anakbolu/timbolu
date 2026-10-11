package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests cover the protocol client on its own, which the integration tests
// exercise only through their happy paths. What matters here is what a server can
// answer that is not a result: a refusal, an outage, a body that is not JSON, and
// the event-stream form the protocol also allows.

// server is a stub that answers whatever one test needs.
type server struct {
	t *testing.T
	// handle is the answer for one request.
	handle func(w http.ResponseWriter, r *http.Request, request map[string]any)
	// lastAuth and lastAccept record what the client sent.
	lastAuth   string
	lastAccept string
	lastBody   map[string]any
	server     *httptest.Server
}

func newServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, request map[string]any)) *server {
	t.Helper()

	stub := &server{t: t, handle: handle}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)

		stub.lastAuth = r.Header.Get("Authorization")
		stub.lastAccept = r.Header.Get("Accept")
		stub.lastBody = request

		stub.handle(w, r, request)
	}))
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *server) client(t *testing.T, credential string) *Client {
	t.Helper()

	client, err := New(Config{
		Endpoint:   s.server.URL,
		Credential: credential,
		Timeout:    5 * time.Second,
	})
	require.NoError(t, err)
	return client
}

// result answers one request with a JSON-RPC result.
func result(t *testing.T, w http.ResponseWriter, request map[string]any, payload any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      request["id"],
		"result":  payload,
	}))
}

// TestTheHandshakeReportsWhatTheServerCallsItself covers the first call a client
// makes, and the credential it carries.
func TestTheHandshakeReportsWhatTheServerCallsItself(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{
			"protocolVersion": ProtocolVersion,
			"serverInfo":      map[string]any{"name": "stub-notion", "version": "2"},
		})
	})

	name, err := stub.client(t, "a-server-token").Initialize(context.Background())
	require.NoError(t, err)
	require.Equal(t, "stub-notion", name)

	// The handshake names this client and the revision it speaks, because a server
	// is entitled to refuse a revision it does not know.
	require.Equal(t, "initialize", stub.lastBody["method"])
	params, ok := stub.lastBody["params"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, ProtocolVersion, params["protocolVersion"])

	// The credential travels as a bearer token, and both answer types are
	// accepted because the protocol allows either.
	require.Equal(t, "Bearer a-server-token", stub.lastAuth)
	require.Contains(t, stub.lastAccept, "application/json")
	require.Contains(t, stub.lastAccept, "text/event-stream")
}

// TestAServerWithNoNameIsStillUsable keeps a minimal server from being unusable
// because it did not introduce itself.
func TestAServerWithNoNameIsStillUsable(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{"protocolVersion": ProtocolVersion})
	})

	name, err := stub.client(t, "").Initialize(context.Background())
	require.NoError(t, err)
	require.Equal(t, "mcp", name)
}

// TestAToolIsReadOnlyOnlyWhenTheServerSaysSo is the governance rule at its source.
//
// A server that says nothing is treated as though the tool changes something: the
// product's rule is that a person holds the door to anything that leaves, and
// reading "probably fine" out of silence is how that rule stops being true.
func TestAToolIsReadOnlyOnlyWhenTheServerSaysSo(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{"tools": []map[string]any{
			{
				"name":        "search_pages",
				"description": "Cari halaman",
				"inputSchema": map[string]any{"type": "object"},
				"annotations": map[string]any{"readOnlyHint": true},
			},
			{
				"name":        "create_page",
				"description": "Buat halaman",
				"inputSchema": map[string]any{"type": "object"},
			},
			{
				"name":        "delete_page",
				"inputSchema": map[string]any{"type": "object"},
				"annotations": map[string]any{"readOnlyHint": false},
			},
		}})
	})

	tools, err := stub.client(t, "").Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 3)

	byName := map[string]Tool{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}

	require.True(t, byName["search_pages"].ReadOnly(), "the server said this changes nothing")
	require.False(t, byName["create_page"].ReadOnly(), "silence is not a promise")
	require.False(t, byName["delete_page"].ReadOnly(), "the server said this changes something")
}

// TestAnEmptyToolListIsNotAnError keeps a server that offers nothing from reading
// as a failure.
func TestAnEmptyToolListIsNotAnError(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{"tools": []any{}})
	})

	tools, err := stub.client(t, "").Tools(context.Background())
	require.NoError(t, err)
	require.Empty(t, tools)
}

// TestACallSendsTheArgumentsTheServerExpects covers the call shape.
func TestACallSendsTheArgumentsTheServerExpects(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": `{"found":3}`},
				{"type": "text", "text": "dan satu catatan"},
			},
		})
	})

	answer, err := stub.client(t, "").Call(context.Background(), "search_pages",
		json.RawMessage(`{"q":"anggaran"}`))
	require.NoError(t, err)
	require.False(t, answer.IsError)
	require.Equal(t, "{\"found\":3}\ndan satu catatan", answer.Text())

	// The arguments travel as an object, and the tool is named the way the server
	// knows it.
	params, ok := stub.lastBody["params"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "search_pages", params["name"])

	arguments, ok := params["arguments"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "anggaran", arguments["q"])
}

// TestAToolCalledWithNoArgumentsSendsAnEmptyObject is what a tool that takes none
// expects, and what a server would otherwise refuse.
func TestAToolCalledWithNoArgumentsSendsAnEmptyObject(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}})
	})

	_, err := stub.client(t, "").Call(context.Background(), "ping", nil)
	require.NoError(t, err)

	params := stub.lastBody["params"].(map[string]any)
	arguments, ok := params["arguments"].(map[string]any)
	require.True(t, ok, "an absent argument list must arrive as an empty object")
	require.Empty(t, arguments)
}

// TestAToolThatReportsFailureIsAFailure is the protocol's own shape: a server may
// answer an error while still returning a result.
func TestAToolThatReportsFailureIsAFailure(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "the tool could not do it"}},
			"isError": true,
		})
	})

	answer, err := stub.client(t, "").Call(context.Background(), "explode", json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrToolFailed)
	require.Contains(t, err.Error(), "the tool could not do it")

	// The content is still returned, because a caller may want to show what the
	// server said about the failure.
	require.True(t, answer.IsError)
	require.Contains(t, answer.Text(), "could not do it")
}

// TestAServerErrorIsReportedWithItsCode covers the JSON-RPC error shape.
func TestAServerErrorIsReportedWithItsCode(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"error":   map[string]any{"code": -32601, "message": "method not found"},
		})
	})

	_, err := stub.client(t, "").Tools(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "method not found")
	require.Contains(t, err.Error(), "-32601")

	// A server's own error is not a transport failure, so it is not retryable:
	// repeating the same call would get the same answer.
	require.NotErrorIs(t, err, ErrServerUnavailable)
}

// TestAnEventStreamAnswerIsRead is the other form the protocol allows.
func TestAnEventStreamAnswerIsRead(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result":  map[string]any{"tools": []map[string]any{{"name": "search_pages"}}},
		})
		require.NoError(t, err)

		// The event name, the data line, and the blank line that ends the event.
		_, _ = w.Write([]byte("event: message\n"))
		_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
	})

	tools, err := stub.client(t, "").Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, "search_pages", tools[0].Name)
}

// TestARefusedCredentialIsReported covers what a person has to fix.
func TestARefusedCredentialIsReported(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	_, err := stub.client(t, "a-stale-token").Initialize(context.Background())
	require.ErrorIs(t, err, ErrUnauthorized)

	// A forbidden answer is the same situation: the credential is not accepted.
	stub2 := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	_, err = stub2.client(t, "a-stale-token").Initialize(context.Background())
	require.ErrorIs(t, err, ErrUnauthorized)
}

// TestAnOutageIsRetryable is the distinction a caller acts on: a server that is
// down is usually down briefly, and a task should wait rather than fail.
func TestAnOutageIsRetryable(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, _ *http.Request, _ map[string]any){
		"a 5xx": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
		"a 503": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		},
		"a body that is not JSON": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			_, _ = w.Write([]byte("<html>not json</html>"))
		},
		"an empty body": func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {},
		"a result with nothing in it": func(w http.ResponseWriter, _ *http.Request, request map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"]})
		},
	}

	for name, handle := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newServer(t, handle)
			_, err := stub.client(t, "").Initialize(context.Background())
			require.ErrorIs(t, err, ErrServerUnavailable)
		})
	}
}

// TestARequestRefusedByTheServerIsNotRetryable is the other half: a 4xx is the
// request's own fault, so repeating it unchanged would get the same answer.
func TestARequestRefusedByTheServerIsNotRetryable(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		http.Error(w, "that method does not exist here", http.StatusNotFound)
	})

	_, err := stub.client(t, "").Initialize(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrServerUnavailable)
	require.Contains(t, err.Error(), "404")
}

// TestAnUnreachableServerIsRetryable covers the transport failure rather than an
// answer.
func TestAnUnreachableServerIsRetryable(t *testing.T) {
	// A server that is started and immediately stopped: the address is real and
	// nothing is listening on it.
	stub := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := stub.URL
	stub.Close()

	client, err := New(Config{Endpoint: address, Timeout: time.Second})
	require.NoError(t, err)

	_, err = client.Initialize(context.Background())
	require.ErrorIs(t, err, ErrServerUnavailable)
}

// TestArgumentsThatAreNotAnObjectAreRefusedBeforeSending keeps a malformed call
// from reaching a server at all.
func TestArgumentsThatAreNotAnObjectAreRefusedBeforeSending(t *testing.T) {
	reached := false
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		reached = true
		result(t, w, request, map[string]any{})
	})

	_, err := stub.client(t, "").Call(context.Background(), "search", json.RawMessage(`["not","an","object"]`))
	require.ErrorContains(t, err, "not an object")
	require.False(t, reached, "a malformed call must not reach the server")
}

// TestAnEndpointIsRequired keeps a client from being built without somewhere to
// send requests.
func TestAnEndpointIsRequired(t *testing.T) {
	_, err := New(Config{Endpoint: "  "})
	require.Error(t, err)

	_, err = New(Config{Endpoint: ""})
	require.Error(t, err)

	// A client with no credential sends none, which is what a server on a private
	// network looks like.
	client, err := New(Config{Endpoint: "https://example.com"})
	require.NoError(t, err)
	require.Empty(t, client.credential)
}

// TestAToolListingKeepsTheServersOwnSchema is what makes a server's tools usable
// as this product's tools: the schema is passed through, not summarised.
func TestAToolListingKeepsTheServersOwnSchema(t *testing.T) {
	schema := `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`

	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{"tools": []map[string]any{
			{"name": "search_pages", "description": "Cari", "inputSchema": json.RawMessage(schema)},
		}})
	})

	tools, err := stub.client(t, "").Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.JSONEq(t, schema, string(tools[0].InputSchema))
}

// TestAPaginatedListingIsReadAsTheFirstPage is a stated limit rather than a silent
// truncation: following the cursor needs a bound this client does not have.
func TestAPaginatedListingIsReadAsTheFirstPage(t *testing.T) {
	stub := newServer(t, func(w http.ResponseWriter, r *http.Request, request map[string]any) {
		result(t, w, request, map[string]any{
			"tools":      []map[string]any{{"name": "search_pages"}},
			"nextCursor": "page-2",
		})
	})

	tools, err := stub.client(t, "").Tools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 1, "the first page is what this client reads")
	require.Equal(t, "search_pages", tools[0].Name)
}

// TestFirstEventDataReadsThePayload covers the reader the event-stream path uses,
// including a body that has no event framing at all.
func TestFirstEventDataReadsThePayload(t *testing.T) {
	require.Equal(t, `{"a":1}`, string(firstEventData([]byte("event: message\ndata: {\"a\":1}\n\n"))))

	// A body with no data line is returned as it is, so the caller reports a
	// readable failure rather than an empty one.
	require.Equal(t, `{"a":1}`, string(firstEventData([]byte(`{"a":1}`))))

	// A data line with surrounding space is trimmed.
	require.Equal(t, `{"a":1}`, string(firstEventData([]byte("data:   {\"a\":1}  \n"))))
}

// TestTruncateBoundsAnErrorMessage keeps a server's whole body out of a log line.
func TestTruncateBoundsAnErrorMessage(t *testing.T) {
	require.Equal(t, "short", truncate("short", 10))
	require.Equal(t, "0123456789…", truncate(strings.Repeat("0123456789", 5), 10))
}

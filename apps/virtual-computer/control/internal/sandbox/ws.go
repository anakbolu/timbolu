package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// This file is the one websocket client in the control plane, and it exists for one
// reason: verifying that a navigation actually loaded.
//
// Chromium's HTTP endpoint reports that it opened a tab, which is not the same thing
// as the page having loaded — a sandbox with no route out opens its error page for
// every external URL. The only way to tell the two apart is the DevTools protocol
// over the target's websocket, and the module is stdlib-only (docs/adr/0003 §3.2), so
// the client is here rather than a dependency.
//
// It is deliberately the smallest thing that works: a masked text frame out, a text
// frame in, ping answered with pong, close and continuation handled because a real
// browser sends them. No extensions, no compression, no fragmentation on send.

// wsConn is one upgraded connection to a DevTools endpoint.
type wsConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

// dialWS performs the RFC 6455 handshake against a ws:// URL and carries the sandbox
// secret in the request headers, which is what the sandbox's gate asks for.
//
// Only ws:// is accepted: the gate speaks plain HTTP on the sandbox network, and a
// wss:// here would mean the control plane had been handed an address outside it.
func dialWS(ctx context.Context, rawURL, secret string) (*wsConn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: parse the debugger address: %w", err)
	}
	if parsed.Scheme != "ws" {
		return nil, fmt.Errorf("vcomputer: the debugger address %q is not a ws:// address", rawURL)
	}
	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), "80")
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: reach the browser: %w", err)
	}
	// The deadline is the caller's, not this function's: one Navigate is bounded as a
	// whole, so every read inside it shares the same instant.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vcomputer: generate the handshake key: %w", err)
	}

	handshake := strings.Join([]string{
		"GET " + parsed.RequestURI() + " HTTP/1.1",
		"Host: " + parsed.Host,
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key),
		"Sec-WebSocket-Version: 13",
		SecretHeader + ": " + secret,
		"", "",
	}, "\r\n")
	if _, err := io.WriteString(conn, handshake); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vcomputer: send the handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vcomputer: read the handshake answer: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		_ = response.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("vcomputer: the debugger refused the connection: %s %s",
			response.Status, strings.TrimSpace(string(body)))
	}
	// The reader keeps whatever the handshake read past the headers, which is where
	// the first frames already are.
	return &wsConn{conn: conn, reader: reader}, nil
}

// Close ends the connection.
func (c *wsConn) Close() error { return c.conn.Close() }

// call sends one DevTools command and answers its result.
//
// It skips the events the browser sends in between — Page.enable alone produces
// several — and answers the first message whose id matches, which is the protocol's
// own correlation rule.
func (c *wsConn) call(id int, method string, params any) (json.RawMessage, error) {
	command := map[string]any{"id": id, "method": method}
	// A command without parameters omits the field rather than sending null: the
	// protocol's own shape is an object, and a browser is free to reject null.
	if params != nil {
		command["params"] = params
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("vcomputer: build the %s command: %w", method, err)
	}
	if err := c.writeText(payload); err != nil {
		return nil, err
	}

	for {
		message, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		var answer struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(message, &answer); err != nil {
			// A message this client cannot read is a message it cannot correlate, and
			// skipping it is better than failing the whole navigation over it.
			continue
		}
		if answer.ID != id {
			continue
		}
		if answer.Error != nil {
			return nil, fmt.Errorf("vcomputer: %s failed: %s", method, answer.Error.Message)
		}
		return answer.Result, nil
	}
}

// writeText sends one masked text frame. A client must mask, and a server is
// required to close the connection when it does not.
func (c *wsConn) writeText(payload []byte) error {
	return c.writeFrame(0x1, payload)
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+14)
	frame = append(frame, 0x80|opcode)

	switch size := len(payload); {
	case size < 126:
		frame = append(frame, 0x80|byte(size))
	case size <= 0xffff:
		frame = append(frame, 0x80|126, byte(size>>8), byte(size))
	default:
		frame = append(frame, 0x80|127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(size))
		frame = append(frame, length[:]...)
	}

	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return fmt.Errorf("vcomputer: generate the frame mask: %w", err)
	}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		return fmt.Errorf("vcomputer: send to the browser: %w", err)
	}
	return nil
}

// readMessage answers the next complete data message: continuation frames are joined,
// a ping is answered with a pong, and a close ends the conversation.
func (c *wsConn) readMessage() ([]byte, error) {
	var message []byte
	for {
		final, opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}

		switch opcode {
		case 0x8:
			return nil, fmt.Errorf("vcomputer: the browser closed the debugger connection")
		case 0x9:
			if err := c.writeFrame(0xa, payload); err != nil {
				return nil, err
			}
		case 0xa:
			// A pong answers a ping this client did not send; there is nothing to do.
		case 0x0, 0x1, 0x2:
			message = append(message, payload...)
			if final {
				return message, nil
			}
		default:
			// Reserved opcodes cannot be interpreted, and a frame this client does
			// not know is not a reason to abandon a navigation.
		}
	}
}

// readFrame reads one frame: whether it is final, its opcode, and its payload.
func (c *wsConn) readFrame() (final bool, opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return false, 0, nil, fmt.Errorf("vcomputer: read from the browser: %w", err)
	}
	final = header[0]&0x80 != 0
	opcode = header[0] & 0x0f
	masked := header[1]&0x80 != 0

	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return false, 0, nil, fmt.Errorf("vcomputer: read a frame length: %w", err)
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return false, 0, nil, fmt.Errorf("vcomputer: read a frame length: %w", err)
		}
		length = binary.BigEndian.Uint64(extended[:])
	}

	// A frame larger than this is not a DevTools answer: the largest thing this
	// client asks for is a document's title and URL. Refusing it keeps a hostile or
	// broken endpoint from allocating the control plane's memory.
	const maxFrame = 1 << 20
	if length > maxFrame {
		return false, 0, nil, fmt.Errorf("vcomputer: the browser sent a %d byte frame", length)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return false, 0, nil, fmt.Errorf("vcomputer: read a frame mask: %w", err)
		}
	}

	payload = make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return false, 0, nil, fmt.Errorf("vcomputer: read a frame payload: %w", err)
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return final, opcode, payload, nil
}

// gateWebSocketURL is the websocket address of one browser target as reached through
// a sandbox's gate.
//
// Chromium answers a webSocketDebuggerUrl pointing at its own loopback address
// (ws://127.0.0.1:9222/devtools/page/<id>), which the control plane cannot dial and
// should not be able to: only the gate may reach 9222. So the path is kept — it is
// what names the target — and the address is replaced with the gate's, under the
// prefix the gate publishes CDP on.
func gateWebSocketURL(gate SandboxTarget, opened cdpTarget) (string, error) {
	base, err := url.Parse(gate.URL)
	if err != nil {
		return "", fmt.Errorf("vcomputer: the sandbox gate address is not usable: %w", err)
	}
	if base.Host == "" {
		return "", fmt.Errorf("vcomputer: the sandbox gate address %q has no host", gate.URL)
	}

	path := "/devtools/page/" + opened.ID
	if opened.WSURL != "" {
		if parsed, err := url.Parse(opened.WSURL); err == nil && parsed.Path != "" {
			path = parsed.Path
		}
	}

	// The sandbox network is plain HTTP in this PoC, but a deployment that put TLS
	// in front of the gate would need wss, and the address is the only thing that
	// says which.
	base.Scheme = "ws"
	if strings.HasPrefix(gate.URL, "https://") {
		base.Scheme = "wss"
	}
	base.Path = gatePrefix + path
	base.RawQuery = ""
	return base.String(), nil
}

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"time"
)

// InMemoryRuntime is the runtime the PoC actually runs: it holds the session
// record, answers a generated screenshot, and writes down every action instead of
// performing it.
//
// It is not a mock for the sake of tests. It is the shape a caller can be built
// against before a container exists: the API surface, the session lifecycle, and
// the Temporal activities are all real, and the only thing missing is the machine.
// It is also what makes the PoC runnable on a laptop with no Docker, which is how
// the HTTP surface gets verified at all.
type InMemoryRuntime struct {
	mu       sync.Mutex
	sessions map[string]*record
	clock    func() time.Time
	// screen is generated once: a screenshot stub that changes between calls is a
	// screenshot nobody can compare.
	screen []byte
}

// record is the session plus what was done to it. The action log is kept out of
// Session because it is evidence for the PoC, not part of what a caller needs.
type record struct {
	session Session
	actions []Action
}

// Compile-time check that the stub is a runtime.
var _ Runtime = (*InMemoryRuntime)(nil)

// NewInMemory builds the stub runtime.
func NewInMemory() *InMemoryRuntime {
	return &InMemoryRuntime{
		sessions: make(map[string]*record),
		clock:    time.Now,
		screen:   placeholderPNG(),
	}
}

// Ensure creates the session record if it is not there and returns it either way.
func (r *InMemoryRuntime) Ensure(_ context.Context, botID string) (Session, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return Session{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock()
	rec, ok := r.sessions[id]
	if !ok {
		rec = &record{session: Session{
			BotID:     id,
			State:     StateRunning,
			Runtime:   "memory",
			NoVNCURL:  novncURL(id),
			CDPURL:    cdpURL(id),
			Volume:    volumeName(id),
			CreatedAt: now,
		}}
		r.sessions[id] = rec
	}

	rec.session.State = StateRunning
	rec.session.LastUsedAt = now
	return rec.session, nil
}

// Status reports the session, or ErrNoSession.
func (r *InMemoryRuntime) Status(_ context.Context, botID string) (Session, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return Session{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.sessions[id]
	if !ok {
		return Session{}, ErrNoSession
	}
	return rec.session, nil
}

// Stop forgets the session. The volume is what outlives it, and the stub has no
// volume to keep, so the next Ensure starts a fresh record.
func (r *InMemoryRuntime) Stop(_ context.Context, botID string) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.sessions[id]; !ok {
		return ErrNoSession
	}
	delete(r.sessions, id)
	return nil
}

func (r *InMemoryRuntime) Screenshot(_ context.Context, botID string) ([]byte, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.sessions[id]
	if !ok {
		return nil, ErrNoSession
	}
	r.touch(rec, Action{Kind: "screenshot", Detail: "placeholder"})

	// A copy, so a caller that writes to the bytes cannot corrupt the next answer.
	out := make([]byte, len(r.screen))
	copy(out, r.screen)
	return out, nil
}

// Click records a press.
func (r *InMemoryRuntime) Click(_ context.Context, botID string, x, y int) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.sessions[id]
	if !ok {
		return ErrNoSession
	}
	r.touch(rec, Action{Kind: "click", Detail: fmt.Sprintf("%d,%d", x, y)})
	return nil
}

// Type records the text. Nothing is typed anywhere: there is no window server.
func (r *InMemoryRuntime) Type(_ context.Context, botID string, text string) error {
	id, err := cleanBotID(botID)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.sessions[id]
	if !ok {
		return ErrNoSession
	}
	r.touch(rec, Action{Kind: "type", Detail: text})
	return nil
}

// Exec echoes the command. It never runs it, on the host or anywhere else: the
// stub exists to show the answer shape, and a PoC that ran arbitrary commands would
// be the exact thing riset §5.1 says must not exist without a gate in front of it.
func (r *InMemoryRuntime) Exec(_ context.Context, botID string, command string) (ExecResult, error) {
	id, err := cleanBotID(botID)
	if err != nil {
		return ExecResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, ErrNoCommand
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.sessions[id]
	if !ok {
		return ExecResult{}, ErrNoSession
	}
	r.touch(rec, Action{Kind: "exec", Detail: command})

	return ExecResult{
		Command:  command,
		Stdout:   "stub runtime: not executed: " + command,
		ExitCode: 0,
		Stub:     true,
	}, nil
}

// Actions returns what was done to a session, which is how a test and the demo page
// see that a call arrived. It is not part of Runtime: the Docker runtime will not
// have it.
func (r *InMemoryRuntime) Actions(botID string) []Action {
	// The id is trimmed the same way Ensure trims it: a caller that asked for
	// " demo-bot " got a session for "demo-bot", and looking it up untrimmed would
	// answer "no actions" for a session that has them.
	id := strings.TrimSpace(botID)

	r.mu.Lock()
	defer r.mu.Unlock()

	rec, ok := r.sessions[id]
	if !ok {
		return nil
	}
	out := make([]Action, len(rec.actions))
	copy(out, rec.actions)
	return out
}

// touch records an action and marks the session used. The caller holds the lock.
func (r *InMemoryRuntime) touch(rec *record, action Action) {
	action.At = r.clock()
	rec.actions = append(rec.actions, action)
	rec.session.LastUsedAt = action.At
}

// The three names a sandbox is reachable by. They are written here rather than in
// DockerRuntime because they are part of what a caller is told, not of how the
// container is made: the Docker runtime must answer the same strings.

// novncURL is where a person watches the desktop.
func novncURL(botID string) string {
	return fmt.Sprintf("http://%s:6080/vnc.html?autoconnect=1&resize=scale", containerName(botID))
}

// cdpURL is Chromium's DevTools endpoint inside the sandbox network.
func cdpURL(botID string) string {
	return fmt.Sprintf("http://%s:9222", containerName(botID))
}

// containerName and volumeName derive both names from the bot id. That is why the
// id is validated before it gets here: these strings end up in a `docker create`
// command line.
func containerName(botID string) string { return "vcomputer-" + botID }
func volumeName(botID string) string    { return "vcomputer-" + botID + "-home" }

// The stub desktop: a dark screen, a window with a title bar and three dots, a few
// content bars, a taskbar, and a cursor cross. Drawn in code so the repository
// carries no binary and the image is reproducible.
const (
	placeholderWidth  = 480
	placeholderHeight = 320
)

func placeholderPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, placeholderWidth, placeholderHeight))
	desktop := color.RGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0xff}
	chrome := color.RGBA{R: 0x24, G: 0x28, B: 0x2e, A: 0xff}
	title := color.RGBA{R: 0x33, G: 0x39, B: 0x42, A: 0xff}
	body := color.RGBA{R: 0x20, G: 0x24, B: 0x2a, A: 0xff}
	bar := color.RGBA{R: 0x3a, G: 0x41, B: 0x4b, A: 0xff}
	accent := color.RGBA{R: 0x4c, G: 0x9a, B: 0x7a, A: 0xff}

	fillRect(img, image.Rect(0, 0, placeholderWidth, placeholderHeight), desktop)

	// Taskbar along the bottom, with one highlighted item.
	fillRect(img, image.Rect(0, placeholderHeight-28, placeholderWidth, placeholderHeight), chrome)
	fillRect(img, image.Rect(12, placeholderHeight-20, 48, placeholderHeight-8), accent)

	// The window, its title bar, and its three dots.
	window := image.Rect(48, 36, placeholderWidth-48, placeholderHeight-56)
	fillRect(img, window, body)
	fillRect(img, image.Rect(window.Min.X, window.Min.Y, window.Max.X, window.Min.Y+22), title)
	for i := range 3 {
		dot := image.Rect(window.Min.X+10+i*14, window.Min.Y+8, window.Min.X+16+i*14, window.Min.Y+14)
		fillRect(img, dot, accent)
	}

	// Content: a URL-ish bar and five text lines of shrinking width.
	fillRect(img, image.Rect(window.Min.X+12, window.Min.Y+34, window.Max.X-12, window.Min.Y+48), bar)
	for i := range 5 {
		top := window.Min.Y + 62 + i*22
		right := window.Max.X - 12 - i*24
		if right <= window.Min.X+12 {
			right = window.Min.X + 12
		}
		fillRect(img, image.Rect(window.Min.X+12, top, right, top+8), bar)
	}

	// Cursor: a small cross where a click would land.
	cx, cy := placeholderWidth*3/4, placeholderHeight*2/3
	fillRect(img, image.Rect(cx-8, cy, cx+8, cy+1), accent)
	fillRect(img, image.Rect(cx, cy-8, cx+1, cy+8), accent)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		// Encoding an RGBA image cannot fail; if it ever does, an empty screenshot is
		// better than a panic in a control plane.
		return nil
	}
	return buf.Bytes()
}

// fillRect paints one rectangle. image/draw would do this too, but a nested loop is
// the whole implementation and keeps the stub dependency-free.
func fillRect(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

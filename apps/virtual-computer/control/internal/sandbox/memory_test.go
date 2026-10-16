package sandbox

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"strings"
	"testing"
	"time"
)

// The stub runtime is what every layer above it is built and tested against while
// there is no container, so its own behaviour is worth pinning: the lifecycle, the
// id rule that protects the container name, and the promise that exec never runs
// anything.

func TestEnsureIsIdempotent(t *testing.T) {
	runtime := NewInMemory()
	ctx := context.Background()

	first, err := runtime.Ensure(ctx, "bot-a")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// The clock is moved on purpose: a second Ensure must not look like a second
	// session, and the two timestamps are what say so.
	runtime.clock = func() time.Time { return first.CreatedAt.Add(time.Minute) }

	second, err := runtime.Ensure(ctx, "bot-a")
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("CreatedAt moved from %v to %v: a second call started a second session", first.CreatedAt, second.CreatedAt)
	}
	if !second.LastUsedAt.After(first.LastUsedAt) {
		t.Fatalf("LastUsedAt = %v, want it after %v: an idle reaper reads this field", second.LastUsedAt, first.LastUsedAt)
	}
	if second.Volume != "vcomputer-bot-a-home" {
		t.Fatalf("volume = %q, want the per-bot volume", second.Volume)
	}
	if second.ContainerID != "" {
		t.Fatalf("container id = %q, want empty: the stub owns no container", second.ContainerID)
	}
}

func TestSessionNamesFollowTheContainerContract(t *testing.T) {
	runtime := NewInMemory()

	session, err := runtime.Ensure(context.Background(), "bot-a")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// These three strings are the contract the sandbox image and the compose file
	// were written against (README, "Arsitektur"): the noVNC port, the CDP port, and
	// the volume mount point. The Docker runtime must answer the same ones.
	if session.NoVNCURL != "http://vcomputer-bot-a:6080/vnc.html?autoconnect=1&resize=scale" {
		t.Fatalf("novnc url = %q", session.NoVNCURL)
	}
	if session.CDPURL != "http://vcomputer-bot-a:9222" {
		t.Fatalf("cdp url = %q", session.CDPURL)
	}
	if session.Runtime != "memory" || session.State != StateRunning {
		t.Fatalf("session = %+v", session)
	}
}

func TestStatusAndStopFollowTheLifecycle(t *testing.T) {
	runtime := NewInMemory()
	ctx := context.Background()

	if _, err := runtime.Status(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("status before ensure: err = %v, want ErrNoSession", err)
	}

	if _, err := runtime.Ensure(ctx, "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := runtime.Status(ctx, "bot-a"); err != nil {
		t.Fatalf("status after ensure: %v", err)
	}

	if err := runtime.Stop(ctx, "bot-a"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := runtime.Status(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("status after stop: err = %v, want ErrNoSession", err)
	}
	// Stopping twice is a caller that retried, not a caller that is wrong: the
	// answer stays the same.
	if err := runtime.Stop(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("second stop: err = %v, want ErrNoSession", err)
	}
}

func TestActionsWithoutASessionAreRefused(t *testing.T) {
	runtime := NewInMemory()
	ctx := context.Background()

	if _, err := runtime.Screenshot(ctx, "bot-a"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("screenshot: err = %v, want ErrNoSession", err)
	}
	if err := runtime.Click(ctx, "bot-a", 1, 2); !errors.Is(err, ErrNoSession) {
		t.Fatalf("click: err = %v, want ErrNoSession", err)
	}
	if err := runtime.Type(ctx, "bot-a", "halo"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("type: err = %v, want ErrNoSession", err)
	}
	if _, err := runtime.Exec(ctx, "bot-a", "ls"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("exec: err = %v, want ErrNoSession", err)
	}
}

func TestScreenshotIsADecodablePNGThatCannotBeCorrupted(t *testing.T) {
	runtime := NewInMemory()
	ctx := context.Background()

	if _, err := runtime.Ensure(ctx, "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	first, err := runtime.Screenshot(ctx, "bot-a")
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("screenshot is not a PNG: %v", err)
	}
	if decoded.Bounds().Dx() < 100 || decoded.Bounds().Dy() < 100 {
		t.Fatalf("screenshot is %dx%d, too small to stand in for a desktop", decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}

	// A caller that writes into the bytes it was given must not change the next
	// answer: the demo page and a test both hold these bytes.
	for i := range first {
		first[i] = 0
	}
	second, err := runtime.Screenshot(ctx, "bot-a")
	if err != nil {
		t.Fatalf("second screenshot: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(second)); err != nil {
		t.Fatalf("the stub's own screen was corrupted by its caller: %v", err)
	}
}

func TestExecEchoesAndNeverRuns(t *testing.T) {
	runtime := NewInMemory()
	ctx := context.Background()

	if _, err := runtime.Ensure(ctx, "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	result, err := runtime.Exec(ctx, "bot-a", "rm -rf /")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !result.Stub {
		t.Fatal("Stub = false: a caller reading only ExitCode would believe the command ran")
	}
	if !strings.Contains(result.Stdout, "rm -rf /") || !strings.Contains(result.Stdout, "not executed") {
		t.Fatalf("stdout = %q, want the echoed command and the word that says it was not run", result.Stdout)
	}

	if _, err := runtime.Exec(ctx, "bot-a", "   "); !errors.Is(err, ErrNoCommand) {
		t.Fatalf("blank command: err = %v, want ErrNoCommand", err)
	}
}

func TestActionsAreRecordedInOrder(t *testing.T) {
	runtime := NewInMemory()
	ctx := context.Background()

	if _, err := runtime.Ensure(ctx, "bot-a"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := runtime.Screenshot(ctx, "bot-a"); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if err := runtime.Click(ctx, "bot-a", 640, 400); err != nil {
		t.Fatalf("click: %v", err)
	}
	if err := runtime.Type(ctx, "bot-a", "halo"); err != nil {
		t.Fatalf("type: %v", err)
	}
	if _, err := runtime.Exec(ctx, "bot-a", "ls -la"); err != nil {
		t.Fatalf("exec: %v", err)
	}

	actions := runtime.Actions("bot-a")
	want := []struct{ kind, detail string }{
		{"screenshot", "placeholder"},
		{"click", "640,400"},
		{"type", "halo"},
		{"exec", "ls -la"},
	}
	if len(actions) != len(want) {
		t.Fatalf("actions = %+v, want %d", actions, len(want))
	}
	for i, expected := range want {
		if actions[i].Kind != expected.kind || actions[i].Detail != expected.detail {
			t.Fatalf("action %d = %+v, want %s %q", i, actions[i], expected.kind, expected.detail)
		}
		if actions[i].At.IsZero() {
			t.Fatalf("action %d has no timestamp", i)
		}
	}

	// The record is a copy: a caller cannot append to the runtime's own slice.
	actions[0].Kind = "tampered"
	if runtime.Actions("bot-a")[0].Kind != "screenshot" {
		t.Fatal("Actions handed out its own slice")
	}

	if runtime.Actions("nobody") != nil {
		t.Fatal("actions for an unknown bot must be nil")
	}
}

// TestBotIDRule covers the check that stands between a request and a container name:
// the id is interpolated into `vcomputer-<id>` and into a `docker create` command
// line, so anything that could change that command line is refused.
func TestBotIDRule(t *testing.T) {
	valid := []string{"bot-a", "bot_a", "bot.a", "BOT", "0", strings.Repeat("a", 64)}
	for _, id := range valid {
		if !ValidBotID(id) {
			t.Fatalf("ValidBotID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		" ",
		"../etc",
		"bot/a",
		"bot a",
		"bot;rm -rf /",
		"bot$(id)",
		"bot-a\n",
		"-lead",
		".lead",
		strings.Repeat("a", 65),
		"bot-ä",
	}
	for _, id := range invalid {
		if ValidBotID(id) {
			t.Fatalf("ValidBotID(%q) = true, want false", id)
		}
	}
}

func TestCleanBotIDTrimsAndClassifies(t *testing.T) {
	id, err := cleanBotID("  bot-a  ")
	if err != nil {
		t.Fatalf("cleanBotID: %v", err)
	}
	if id != "bot-a" {
		t.Fatalf("id = %q, want the trimmed id", id)
	}

	// The two failures are distinct because the control plane answers 400 with a
	// different sentence for each: "you sent nothing" and "that cannot name a
	// container" are not the same mistake.
	if _, err := cleanBotID("   "); !errors.Is(err, ErrNoBotID) {
		t.Fatalf("blank id: err = %v, want ErrNoBotID", err)
	}
	if _, err := cleanBotID("bot/a"); !errors.Is(err, ErrInvalidBotID) {
		t.Fatalf("bad id: err = %v, want ErrInvalidBotID", err)
	}
}

// TestEnsureTrimsTheID proves the rule reaches the runtime, not just the validator:
// a session created for " bot-a " must be reachable as "bot-a".
func TestEnsureTrimsTheID(t *testing.T) {
	runtime := NewInMemory()
	ctx := context.Background()

	session, err := runtime.Ensure(ctx, " bot-a ")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if session.BotID != "bot-a" {
		t.Fatalf("bot id = %q, want the trimmed id", session.BotID)
	}
	if _, err := runtime.Status(ctx, "bot-a"); err != nil {
		t.Fatalf("status with the trimmed id: %v", err)
	}
	if err := runtime.Click(ctx, " bot-a ", 1, 1); err != nil {
		t.Fatalf("click: %v", err)
	}
	if got := len(runtime.Actions(" bot-a ")); got != 1 {
		t.Fatalf("actions = %d, want the one click: the record is keyed by the trimmed id", got)
	}
}

package sandbox

import (
	"context"
	"fmt"
)

// DockerRuntime is the skeleton of the real runtime: the one that creates a
// container per bot, keeps its home in a named volume, and drives the desktop
// inside it.
//
// It compiles, it satisfies Runtime, and it refuses every call with
// ErrNotImplemented — because the alternative, a stub that quietly does nothing,
// would let a caller believe a container exists. The exact command each method will
// run is written in its comment, so the shape of the implementation is reviewable
// now and the code can be filled in without a redesign.
//
// What it deliberately does not do:
//
//   - It does not import a Docker SDK. The PoC has no dependency it does not use,
//     and the backend's go.mod must not grow for a PoC (docs/adr/0003 §3.2).
//   - It does not touch /var/run/docker.sock. The control plane runs on the sandbox
//     node next to the daemon, and the first real implementation talks to that
//     daemon over its API — never by mounting the socket into a sandboxed container
//     (docs/riset-microvm.md §4.2).
//   - It does not pass --privileged, --device, or a runtime flag: none of them exist
//     on EasyPanel, and the isolation story for the PoC is "hardened runc container",
//     not "microVM" (docs/adr/0003 §3.3).
//
// The image it will run is the one built from apps/virtual-computer/sandbox.
type DockerRuntime struct {
	// image is the sandbox image tag. Empty means the default below.
	image string
	// network is the Docker network the sandbox joins. The control plane must share
	// it: that network is how it reaches port 6080 and 9222, and it is the reason no
	// port is ever published.
	network string
}

// Compile-time check that the skeleton is a runtime.
var _ Runtime = (*DockerRuntime)(nil)

// DefaultImage is the image the compose file builds. It is a constant rather than a
// config value so the skeleton and the compose file cannot drift silently.
const DefaultImage = "vcomputer-sandbox:poc"

// NewDocker builds the skeleton.
func NewDocker() *DockerRuntime {
	return &DockerRuntime{image: DefaultImage, network: "vcomputer-poc"}
}

// Ensure creates the container if it is not running.
//
// TODO(real runtime):
//
//	# The volume holds /home/bolu: the Chromium profile and the bot's files. It is
//	# created once and reused, which is what makes "log in once, continue tomorrow"
//	# possible without keeping the process alive (riset §2.3).
//	docker volume create <volumeName(botID)>
//	docker run -d --name <containerName(botID)> \
//	  --network <network> \
//	  --volume <volumeName(botID)>:/home/bolu \
//	  --cap-drop ALL --security-opt no-new-privileges \
//	  --pids-limit 512 --memory 2g --cpus 1 \
//	  --env SCREEN_GEOMETRY=1280x800x24 \
//	  <image>
//
// The absence of -p is the point: the desktop is reached over the sandbox network by
// name, and never from the public internet.
func (r *DockerRuntime) Ensure(_ context.Context, botID string) (Session, error) {
	if _, err := cleanBotID(botID); err != nil {
		return Session{}, err
	}
	return Session{}, fmt.Errorf("%w: docker run --name %s --volume %s:/home/bolu %s",
		ErrNotImplemented, containerName(botID), volumeName(botID), r.image)
}

// Status reads the container's state.
//
// TODO(real runtime): `docker inspect --format '{{.State.Status}}' <containerName>`
// and map running/exited/created onto the State* constants. A bot with no container
// is ErrNoSession, not a session in state "stopped": the record lives in Postgres
// (riset §6.6), and this API reports what the node actually holds.
func (r *DockerRuntime) Status(_ context.Context, botID string) (Session, error) {
	if _, err := cleanBotID(botID); err != nil {
		return Session{}, err
	}
	return Session{}, fmt.Errorf("%w: docker inspect --format '{{.State.Status}}' %s",
		ErrNotImplemented, containerName(botID))
}

// Stop removes the container and keeps the volume.
//
// TODO(real runtime): `docker rm -f <containerName>` — never `-v`. Removing the
// volume would delete the browser profile, which is the whole persistent half of the
// session; a stop is "the machine is off", not "the machine is gone".
func (r *DockerRuntime) Stop(_ context.Context, botID string) error {
	if _, err := cleanBotID(botID); err != nil {
		return err
	}
	return fmt.Errorf("%w: docker rm -f %s (never -v: the volume is the session)",
		ErrNotImplemented, containerName(botID))
}

// Screenshot captures the Xvfb display inside the container.
//
// TODO(real runtime): `docker exec <containerName> import -window root png:-` (or
// `xwd` piped to a converter) and return the bytes. The exec is against the sandbox
// container only — the control plane never runs a command on its own host.
func (r *DockerRuntime) Screenshot(_ context.Context, botID string) ([]byte, error) {
	if _, err := cleanBotID(botID); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: docker exec %s import -window root png:-",
		ErrNotImplemented, containerName(botID))
}

// Click presses a point on the desktop.
//
// TODO(real runtime): `docker exec <containerName> xdotool mousemove <x> <y> click 1`.
// Coordinates come from the model reading a screenshot, so they are bounded against
// the screen geometry before they are passed on.
func (r *DockerRuntime) Click(_ context.Context, botID string, x, y int) error {
	if _, err := cleanBotID(botID); err != nil {
		return err
	}
	return fmt.Errorf("%w: docker exec %s xdotool mousemove %d %d click 1",
		ErrNotImplemented, containerName(botID), x, y)
}

// Type sends text to the focused window.
//
// TODO(real runtime): `docker exec -i <containerName> xdotool type --clearmodifiers -- <text>`.
// Prefer the CDP route (`9222`, Page.insertText) for anything typed into Chromium:
// it is not subject to keyboard layout or focus, and it does not require the
// keystrokes to travel through a window manager that may not be running.
func (r *DockerRuntime) Type(_ context.Context, botID string, text string) error {
	if _, err := cleanBotID(botID); err != nil {
		return err
	}
	return fmt.Errorf("%w: docker exec %s xdotool type --clearmodifiers -- %q",
		ErrNotImplemented, containerName(botID), text)
}

// Exec runs a command inside the sandbox.
//
// TODO(real runtime): `docker exec <containerName> sh -lc <command>`, with the
// container as the only target. Before this is wired to a model it needs the gate
// riset §5.1 describes: `computer.exec` is write_external, so every command is a
// draft a person approves — or an allowlist of read-only commands is designed and
// the rest stay drafts. That decision is open in docs/adr/0003.
func (r *DockerRuntime) Exec(_ context.Context, botID string, command string) (ExecResult, error) {
	if _, err := cleanBotID(botID); err != nil {
		return ExecResult{}, err
	}
	if command == "" {
		return ExecResult{}, ErrNoCommand
	}
	return ExecResult{}, fmt.Errorf("%w: docker exec %s sh -lc %q",
		ErrNotImplemented, containerName(botID), command)
}

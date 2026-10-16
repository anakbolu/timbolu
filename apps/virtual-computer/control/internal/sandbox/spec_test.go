package sandbox

import (
	"strings"
	"testing"
)

// The create request is the one place this package can turn a mistake into a container
// that has the host, so it is tested as data rather than only through `docker run`: the
// fake docker client in docker_test.go records the argv, and what it cannot show is why a
// request would be refused. Every rejection below is one field of containerSpec, and the
// test is that validate says no and names it.

// testSecret is the sandbox secret the specs below carry. It is shaped like the one
// newSecret generates — 32 hex characters — because nothing about validation depends on
// its value, and a fixed one keeps the rendered argv comparable.
const testSecret = "0123456789abcdef0123456789abcdef"

// validSpec is the create request run() builds for one session: the configured image, the
// sandbox network, one named home volume, and the hardening flags. Each case below starts
// from it and changes exactly one thing, so a case that fails does so on the field it
// names and not on some other part of the request.
func validSpec() containerSpec {
	return containerSpec{
		name:    containerName(DefaultNamePrefix, "bot-a"),
		image:   DefaultImage,
		network: DefaultNetwork,
		labels:  []string{botLabel + "=bot-a"},
		mounts:  []mount{{source: volumeName(DefaultNamePrefix, "bot-a"), target: homeDir}},
		env: []envVar{
			{key: "NOVNC_PORT", value: DefaultNOVNCPort},
			{key: "GATE_PORT", value: DefaultGatePort},
			{key: secretEnv, value: testSecret},
			{key: "SCREEN_GEOMETRY", value: DefaultGeometry},
		},
		securityOpts: []string{"no-new-privileges:true"},
		capDrop:      []string{"ALL"},
		memory:       sandboxMemory,
		pidsLimit:    sandboxPidsLimit,
	}
}

// TestContainerSpecRefusesAWeakenedRequest covers the guard field by field. Each case is
// one way a create request can ask for less isolation than this runtime promises, and the
// answer has to name what it refused: the error text is the only thing a caller who did
// not write the request has to go on.
func TestContainerSpecRefusesAWeakenedRequest(t *testing.T) {
	cases := []struct {
		name   string
		weaken func(*containerSpec)
		want   string
	}{
		{
			name:   "privileged",
			weaken: func(s *containerSpec) { s.privileged = true },
			want:   "privileged",
		},
		{
			name:   "an added capability",
			weaken: func(s *containerSpec) { s.capAdd = []string{"SYS_ADMIN"} },
			want:   "capabilities",
		},
		{
			name:   "a bind mount",
			weaken: func(s *containerSpec) { s.bindMounts = []mount{{source: "/", target: "/host"}} },
			want:   "bind-mounts the host path",
		},
		{
			// The same request written into the field for volumes: a source with a path
			// separator in it is a bind mount whatever it is called, and the guard reads
			// the value rather than the field.
			name:   "a host path in the volume field",
			weaken: func(s *containerSpec) { s.mounts = []mount{{source: "/etc", target: "/etc"}} },
			want:   "bind-mounts the host path",
		},
		{
			// A named volume, but not this runtime's: it was created by somebody else and
			// holds whatever they put in it.
			name:   "a volume the runtime does not own",
			weaken: func(s *containerSpec) { s.mounts = []mount{{source: "someone-elses-home", target: homeDir}} },
			want:   "mounts the volume",
		},
		{
			name:   "the host network",
			weaken: func(s *containerSpec) { s.networkMode = "host" },
			want:   "network mode",
		},
		{
			name:   "another network",
			weaken: func(s *containerSpec) { s.networkMode = "bridge" },
			want:   "network mode",
		},
		{
			name:   "the host pid namespace",
			weaken: func(s *containerSpec) { s.pidMode = "host" },
			want:   "pid mode",
		},
		{
			name:   "the host ipc namespace",
			weaken: func(s *containerSpec) { s.ipcMode = "host" },
			want:   "ipc mode",
		},
		{
			name:   "the host user namespace",
			weaken: func(s *containerSpec) { s.usernsMode = "host" },
			want:   "userns mode",
		},
		{
			name:   "a device from the host",
			weaken: func(s *containerSpec) { s.devices = []string{"/dev/kvm"} },
			want:   "devices",
		},
		{
			name:   "an image outside the allowlist",
			weaken: func(s *containerSpec) { s.image = "attacker/box:latest" },
			want:   "allowlist",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spec := validSpec()
			testCase.weaken(&spec)

			err := spec.validate([]string{DefaultImage})
			if err == nil {
				t.Fatalf("validate accepted a request that asks for %s", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("err = %v, want it to name %q", err, testCase.want)
			}
		})
	}
}

// TestContainerSpecRefusesAnEmptyImageAllowlist keeps the fallback on the caller's side: a
// list that was not configured is a bug in whoever called validate, not a licence to
// create a container from any image at all.
func TestContainerSpecRefusesAnEmptyImageAllowlist(t *testing.T) {
	spec := validSpec()

	err := spec.validate(nil)
	if err == nil {
		t.Fatal("validate accepted an empty image allowlist")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("err = %v, want it to name the allowlist", err)
	}
}

// TestContainerSpecRendersTheRunArgv is the other half of the guard: a request built the
// way run() builds one passes, and what it renders is the argv
// docker_test.go:888 asserts flag by flag — the isolation flags, the environment the
// sandbox reads, the one named volume, and the image last.
func TestContainerSpecRendersTheRunArgv(t *testing.T) {
	spec := validSpec()

	if err := spec.validate([]string{DefaultImage}); err != nil {
		t.Fatalf("validate refused the request run() builds: %v", err)
	}

	want := []string{
		"-d",
		"--name", "vcomputer-bot-a",
		"--label", "vcomputer.bot=bot-a",
		"--network", "vcomputer-internal",
		"--volume", "vcomputer-bot-a-home:/home/bolu",
		"--env", "NOVNC_PORT=" + DefaultNOVNCPort,
		"--env", "GATE_PORT=" + DefaultGatePort,
		"--env", "SANDBOX_SECRET=" + testSecret,
		"--env", "SCREEN_GEOMETRY=" + DefaultGeometry,
		"--security-opt", "no-new-privileges:true",
		"--cap-drop", "ALL",
		"--memory", "2g",
		"--pids-limit", "512",
		"vcomputer-sandbox:poc",
	}
	if got := spec.args(); !equalArgv(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

// TestDockerImageAllowlistIsWhatTheGuardChecks covers the runtime side of the allowlist:
// blanks are not images, a call that leaves nothing usable is not a way to allow nothing,
// and no allowlist means the configured image — resolved when it is used, so that
// WithSandboxImage still decides what a session runs.
func TestDockerImageAllowlistIsWhatTheGuardChecks(t *testing.T) {
	t.Setenv("VCOMPUTER_SANDBOX_IMAGE_ALLOWLIST", "")

	runtime := NewDocker(WithSandboxImage("vcomputer-sandbox:poc"))
	if got := runtime.allowedImages(); !equalArgv(got, []string{"vcomputer-sandbox:poc"}) {
		t.Fatalf("allowed images = %v, want the configured image", got)
	}

	// An option that carries no usable image leaves the runtime on the configured image
	// rather than on none, whichever order the two options were applied in.
	runtime = NewDocker(
		WithSandboxImageAllowlist(" ", ""),
		WithSandboxImage("vcomputer-sandbox:poc"),
	)
	if got := runtime.allowedImages(); !equalArgv(got, []string{"vcomputer-sandbox:poc"}) {
		t.Fatalf("allowed images = %v, want the configured image", got)
	}

	runtime = NewDocker(WithSandboxImageAllowlist("registry.example/box:1", " ", "other:tag"))
	if want := []string{"registry.example/box:1", "other:tag"}; !equalArgv(runtime.allowedImages(), want) {
		t.Fatalf("allowed images = %v, want %v", runtime.allowedImages(), want)
	}

	// The environment is how the compose file configures it, and a comma-separated list
	// with a blank in it is what a person writes by hand.
	t.Setenv("VCOMPUTER_SANDBOX_IMAGE_ALLOWLIST", "registry.example/box:1, ,other:tag")
	runtime = NewDocker()
	if want := []string{"registry.example/box:1", "other:tag"}; !equalArgv(runtime.allowedImages(), want) {
		t.Fatalf("allowed images = %v, want %v", runtime.allowedImages(), want)
	}
}

// TestDockerEnsureRefusesAnImageOutsideTheAllowlist covers the wiring: the guard runs
// inside run(), before anything is asked of the daemon, so a session whose image the
// allowlist does not hold fails with no container created at all.
func TestDockerEnsureRefusesAnImageOutsideTheAllowlist(t *testing.T) {
	fixture := newDockerFixture(t, WithSandboxImageAllowlist("other:tag"))

	_, err := fixture.runtime.Ensure(t.Context(), "bot-a")
	if err == nil {
		t.Fatal("ensure created a sandbox from an image the allowlist does not hold")
	}
	if !strings.Contains(err.Error(), "vcomputer:") || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("err = %v, want a vcomputer error naming the allowlist", err)
	}
	if run := findCall(t, fixture.docker.calls(t), "run"); run != nil {
		t.Fatalf("the runtime ran %v for an image the allowlist does not hold", run)
	}
}

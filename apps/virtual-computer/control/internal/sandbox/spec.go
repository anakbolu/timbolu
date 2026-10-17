package sandbox

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// containerSpec is one sandbox create request as data: what the container is called, what
// it is made of, and what it may reach.
//
// It exists because a request that is assembled straight into an argv has nothing to
// check. `docker run` takes a container's isolation from its flags, so the flags are the
// boundary, and a check that reads them back out of a rendered argv has to agree with the
// renderer about their spelling and order for as long as both of them exist. Here the
// request is a value instead: validate sees every field a container could be created
// with, and args renders only what validate has passed.
//
// That is why the fields fall in two groups, and the second one is the reason this type
// is not just the arguments of run(). A caller that has been talked into creating a
// privileged container — "it needs to see a device" — has to write `privileged: true`
// here, and that is a value a check can refuse. There is no second path to `docker run`.
type containerSpec struct {
	// name is the container name, and what the volume it mounts is named after.
	name string
	// image is what the container is created from. It is checked against the images the
	// runtime allows: an image is code, so creating a container from one is running it.
	image string
	// network is the network the container joins, and the only one it may join. The
	// control plane reaches the sandbox's gate over it and it has no route out.
	network string
	// labels are the records the container carries, which is how a cleanup finds it
	// later without trusting a name.
	labels []string
	// mounts are the named volumes the container gets. A source that names a place on
	// the host is a bind mount rather than a volume, and belongs in bindMounts.
	mounts []mount
	// env is what the sandbox reads out of its own environment: the ports it listens on,
	// the display it starts, and the secret its gate answers for.
	env []envVar
	// securityOpts and capDrop are the hardening the container starts with, applied
	// before anything inside it runs.
	securityOpts []string
	capDrop      []string
	// memory is the container's memory ceiling and pidsLimit its process ceiling: they
	// bound how much of the host one sandbox can take with it.
	memory    string
	pidsLimit int

	// The rest are the fields this runtime never sets, and validate refuses all of them.
	// They are here because the data has to be able to hold the dangerous request for the
	// check to be worth anything: a spec that cannot say "privileged" cannot be a spec
	// that refuses it either.
	//
	// privileged gives the container the host's capabilities and devices.
	privileged bool
	// capAdd hands back a capability that `--cap-drop ALL` took away.
	capAdd []string
	// bindMounts are host paths, which is the host's filesystem inside the container.
	bindMounts []mount
	// networkMode is a network asked for alongside the sandbox network — `host` above
	// all, which is every listener on the host.
	networkMode string
	// pidMode, ipcMode and usernsMode decide whose namespaces the container shares:
	// `host` on any of them is the host's processes, the host's shared memory, or the
	// host's users.
	pidMode    string
	ipcMode    string
	usernsMode string
	// devices are host devices passed through into the container.
	devices []string
}

// mount is one place the container sees data from: where it lives and where it appears
// inside the container.
type mount struct {
	// source is a named volume, or a path on the host for a bind mount.
	source string
	// target is the path inside the container.
	target string
}

// envVar is one variable the sandbox reads out of its own environment.
type envVar struct {
	key   string
	value string
}

// homeDir is where a sandbox's home volume is mounted, and its only mount point: the
// Chromium profile and the bot's files live there, which is what lets a session continue
// after its container is gone.
const homeDir = "/home/bolu"

// sandboxMemory and sandboxPidsLimit are the ceilings every sandbox is created with.
// They are the other half of the isolation story: dropping capabilities bounds what the
// container may do, and these bound how much of the host it may do it with.
const (
	sandboxMemory    = "2g"
	sandboxPidsLimit = 512
)

// validate answers whether this request may be rendered into a `docker run`. It is the
// only thing between the fields above and a container: run() calls it before it calls
// args(), and every answer it gives that is not nil names the field it refused, because
// that name is all a caller has to go on.
//
// The rules are the isolation this runtime promises, read off the request rather than
// assumed of whoever built it: no privilege, no added capability, no host filesystem, no
// network but the sandbox network, no shared namespace, no device — and an image the
// deployment allows, because an arbitrary image is arbitrary code with the sandbox's
// access.
//
// allowedImages is the list the image has to be in. An empty list is a caller mistake
// rather than "anything goes": the caller always has the configured sandbox image to
// pass, so an empty list is refused as the bug it is.
func (s containerSpec) validate(allowedImages []string) error {
	if s.privileged {
		return errors.New("sandbox spec is privileged: the sandbox runs with the capabilities and devices it was created with")
	}
	if len(s.capAdd) > 0 {
		return fmt.Errorf("sandbox spec adds the capabilities %s, and every capability is dropped", strings.Join(s.capAdd, ", "))
	}
	if err := s.validateMounts(); err != nil {
		return err
	}
	if s.networkMode != "" && s.networkMode != s.network {
		return fmt.Errorf("sandbox spec asks for network mode %q: the sandbox joins %q and no other network", s.networkMode, s.network)
	}
	if s.pidMode != "" {
		return fmt.Errorf("sandbox spec asks for pid mode %q: the sandbox has its own pid namespace", s.pidMode)
	}
	if s.ipcMode != "" {
		return fmt.Errorf("sandbox spec asks for ipc mode %q: the sandbox has its own ipc namespace", s.ipcMode)
	}
	if s.usernsMode != "" {
		return fmt.Errorf("sandbox spec asks for userns mode %q: the sandbox has its own user namespace", s.usernsMode)
	}
	if len(s.devices) > 0 {
		return fmt.Errorf("sandbox spec passes through the devices %s: the sandbox holds no device from the host", strings.Join(s.devices, ", "))
	}
	if len(allowedImages) == 0 {
		return errors.New("sandbox spec was validated against an empty image allowlist: the caller passes the images it may create a container from")
	}
	for _, allowed := range allowedImages {
		if s.image == allowed {
			return nil
		}
	}
	return fmt.Errorf("sandbox spec image %q is not in the allowlist [%s]", s.image, strings.Join(allowedImages, " "))
}

// validateMounts refuses every way a container could see the host's filesystem. A bind
// mount is a host path handed into the container; a volume this runtime did not create is
// the same thing by another name — somebody else made it, it holds whatever they put
// there, and a sandbox has no business reading it.
func (s containerSpec) validateMounts() error {
	for _, m := range s.bindMounts {
		return fmt.Errorf("sandbox spec bind-mounts the host path %q at %q", m.source, m.target)
	}
	for _, m := range s.mounts {
		if isHostPath(m.source) {
			return fmt.Errorf("sandbox spec bind-mounts the host path %q at %q", m.source, m.target)
		}
		if m.source != s.homeVolume() {
			return fmt.Errorf("sandbox spec mounts the volume %q, and the only volume this container may mount is %q", m.source, s.homeVolume())
		}
	}
	return nil
}

// homeVolume is the volume this container's home lives in. It is derived from the
// container name rather than carried beside it because volumeName() is what names it that
// way: the one volume a sandbox may mount is the one the runtime created for it.
func (s containerSpec) homeVolume() string { return s.name + "-home" }

// isHostPath reports whether a mount source names a place on the host rather than a named
// volume. Docker draws the line in the same place: a source with a path separator in it,
// or the relative paths "." and "..", is a bind mount, and anything else is a volume name.
func isHostPath(source string) bool {
	return source == "." || source == ".." || strings.Contains(source, "/")
}

// args renders the request as the arguments of `docker run`, without the verb itself,
// which is what run() hands the docker client. Every field is rendered: a flag that
// depended on the value being non-empty would be a field that could vanish from the
// command line without saying so.
//
// The fields validate refuses are not rendered at all. A spec that set one of them never
// reaches here, because run() validates before it renders; and if some later caller
// skipped that step, a container with less than it asked for is the direction a mistake
// should fail in.
func (s containerSpec) args() []string {
	args := []string{"-d", "--name", s.name}
	for _, label := range s.labels {
		args = append(args, "--label", label)
	}
	args = append(args, "--network", s.network)
	for _, m := range s.mounts {
		args = append(args, "--volume", m.source+":"+m.target)
	}
	for _, variable := range s.env {
		args = append(args, "--env", variable.key+"="+variable.value)
	}
	for _, option := range s.securityOpts {
		args = append(args, "--security-opt", option)
	}
	for _, capability := range s.capDrop {
		args = append(args, "--cap-drop", capability)
	}
	args = append(args, "--memory", s.memory, "--pids-limit", strconv.Itoa(s.pidsLimit))
	return append(args, s.image)
}

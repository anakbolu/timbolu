package container

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// These tests are the deployment half of the virtual computer's configuration. The loader reads
// `VCOMPUTER_CONTROL_*` from the environment (packages/platform/config), so a compose service
// that does not forward them is a worker that comes up with no control plane however `.env` is
// filled — which is what issue #217 reported: `virtual computer is not configured:
// VCOMPUTER_CONTROL_URL is empty`, from a stack whose `.env` named one.
//
// They read the repository's compose files, because that is where the contract lives, and they
// read them as YAML rather than as text: what is pinned is what compose resolves — the anchor's
// contents, the network a service ends up on — not how the file is spelled.

// composeService is the part of a service these tests look at.
type composeService struct {
	Environment map[string]any `yaml:"environment"`
	Networks    []string       `yaml:"networks"`
}

// composeProject is the part of a compose file these tests look at.
type composeProject struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]struct {
		Name     string `yaml:"name"`
		External bool   `yaml:"external"`
	} `yaml:"networks"`
}

// composeFile reads one of the repository's compose files. The path is relative to this package,
// which sits four directories below the repository root.
func composeFile(t *testing.T, name string) composeProject {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("..", "..", "..", "..", name))
	require.NoError(t, err, "%s is part of the deployment contract these tests pin", name)

	var project composeProject
	require.NoError(t, yaml.Unmarshal(content, &project))
	return project
}

// envValue renders one environment entry. Compose values are not all strings — a port is a
// number — so the map is decoded as `any`, and what the test compares is what compose would
// hand the process.
func envValue(service composeService, key string) (string, bool) {
	value, ok := service.Environment[key]
	if !ok {
		return "", false
	}
	return fmt.Sprint(value), true
}

// mustEnv is envValue for an entry the compose file is expected to carry.
func mustEnv(t *testing.T, service composeService, key string) string {
	t.Helper()

	value, ok := envValue(service, key)
	require.Truef(t, ok, "the compose file no longer sets %s", key)
	return value
}

// composeVar is compose's own substitution syntax: `${VAR}` and `${VAR:-default}`.
var composeVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// composeSubstitute renders one compose value the way compose does, so what a test compares is
// the value a deployment ends up with rather than the variable's spelling in the file. `env` is
// the environment that deployment would have; an unset variable falls back to its default.
func composeSubstitute(t *testing.T, value string, env map[string]string) string {
	t.Helper()

	return composeVar.ReplaceAllStringFunc(value, func(variable string) string {
		match := composeVar.FindStringSubmatch(variable)
		name, fallback := match[1], match[2]

		if set := env[name]; set != "" {
			return set
		}
		require.NotEmptyf(t, fallback, "%s is unset and carries no default", name)
		return fallback
	})
}

// TestAgentWorkerIsGivenTheVirtualComputerSettings: the agent worker is the process that
// registers the module's activities (RegisterAgentTasks), so it is the process the three
// settings have to reach. Reading them from `.env` is what makes the stack configurable at all;
// a compose file that leaves them out is the bug this test is here to keep out.
func TestAgentWorkerIsGivenTheVirtualComputerSettings(t *testing.T) {
	project := composeFile(t, "docker-compose.yml")

	worker, ok := project.Services["agent-worker"]
	require.True(t, ok, "docker-compose.yml no longer runs an agent worker")

	for _, key := range []string{
		"VCOMPUTER_CONTROL_URL",
		"VCOMPUTER_CONTROL_TOKEN",
		"VCOMPUTER_CONTROL_TIMEOUT",
	} {
		value, ok := envValue(worker, key)
		require.Truef(t, ok,
			"agent-worker does not forward %s, so .env cannot configure the virtual computer", key)
		require.Containsf(t, value, "${"+key,
			"%s has to come from the environment rather than be baked into the compose file", key)
	}

	// The token is the shared secret of the one process that calls the control plane. The API,
	// the gateway, and the integration worker never do, so the anchor they share must not carry
	// it: a secret handed to a process with no use for it is a secret with more places to leak.
	//
	// The shared anchor is checked first, so the three negatives below cannot pass by reading a
	// merge key the decoder did not resolve: what they share has to be visible here.
	require.Equal(t, "local", mustEnv(t, project.Services["api"], "ENVIRONMENT"),
		"the services' shared anchor did not resolve, so the checks below would mean nothing")
	for _, name := range []string{"api", "wa-gateway", "integration-worker"} {
		_, ok := envValue(project.Services[name], "VCOMPUTER_CONTROL_TOKEN")
		require.Falsef(t, ok, "%s has no use for the control plane's token", name)
	}
}

// TestTheVirtualComputerOverlayReachesTheControlPlane: forwarding the address is not enough. The
// PoC is a compose project of its own and the only port it publishes is bound to the host's
// loopback, so a worker on the product network resolves nothing. The overlay is what puts it on
// the control plane's network and defaults the address to that service.
func TestTheVirtualComputerOverlayReachesTheControlPlane(t *testing.T) {
	project := composeFile(t, "docker-compose.vcomputer.yml")

	worker, ok := project.Services["agent-worker"]
	require.True(t, ok, "the overlay exists to reach the control plane from the agent worker")

	// The product network carries Postgres, Redis, and Temporal: the overlay adds a network, it
	// does not replace the one the worker runs on.
	require.Contains(t, worker.Networks, "default")
	require.Contains(t, worker.Networks, "vcomputer-edge")

	edge, ok := project.Networks["vcomputer-edge"]
	require.True(t, ok, "the overlay joins a network the PoC creates")
	require.True(t, edge.External,
		"the PoC owns that network, so this file must not try to create it")
	// The name itself is covered below: it is configurable, so what the file carries is the
	// variable and its default rather than the name compose resolves.

	url, ok := envValue(worker, "VCOMPUTER_CONTROL_URL")
	require.True(t, ok, "the overlay is what makes the service name the control plane's address")
	require.Contains(t, url, "vcomputer-control")
}

// TestTheVirtualComputerOverlayNamesTheEdgeNetworkFromTheEnvironment: the network's key is what
// the service's `networks:` list names, so it stays `vcomputer-edge` and only the name compose
// resolves is configurable. The default has to stay the name the PoC gives that network — the
// overlay joins a network it does not create — and the override is for a control plane whose
// edge network is named something else.
func TestTheVirtualComputerOverlayNamesTheEdgeNetworkFromTheEnvironment(t *testing.T) {
	project := composeFile(t, "docker-compose.vcomputer.yml")

	edge, ok := project.Networks["vcomputer-edge"]
	require.True(t, ok, "the key is what agent-worker's networks list names, so it stays fixed")

	require.Equal(t, "vcomputer-edge", composeSubstitute(t, edge.Name, nil),
		"an unset VCOMPUTER_EDGE_NETWORK has to mean the network the PoC creates")

	require.Equal(t, "foo-edge",
		composeSubstitute(t, edge.Name, map[string]string{"VCOMPUTER_EDGE_NETWORK": "foo-edge"}),
		"the override names the network the control plane is on")
}

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	vcomputerservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/vcomputer/service"
)

// TestControlTimeoutFallsBackToTheDefault: the command reads VCOMPUTER_CONTROL_TIMEOUT the way
// the loader does, so a value that is not a duration is the default rather than a refused run.
//
// The run is the only thing this command exists for, and the setting it refuses over bounds one
// call to an optional PoC: a tool that will not start on a typo would be a second place for issue
// #219 to happen, and the loader's own answer to the same value is a warning.
func TestControlTimeoutFallsBackToTheDefault(t *testing.T) {
	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "")
	require.Equal(t, vcomputerservice.DefaultTimeout, controlTimeout(), "an unset value takes the default")

	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "banana")
	require.Equal(t, vcomputerservice.DefaultTimeout, controlTimeout(), "a value that will not parse takes the default")

	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "90s")
	require.Equal(t, 90*time.Second, controlTimeout(), "a value that parses is the one used")
}

// TestControlTimeoutCarriesANonPositiveValueThrough: zero and negative are carried as given,
// because the rule that they are not a bound is vcomputerservice.EffectiveTimeout's — the same
// one the client enforces and the container logs. Clamping here would make the smoke run
// quieter than the deployment it stands in for (issue #257).
func TestControlTimeoutCarriesANonPositiveValueThrough(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want time.Duration
	}{
		"zero":     {"0", 0},
		"negative": {"-5s", -5 * time.Second},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", testCase.raw)

			got := controlTimeout()
			require.Equal(t, testCase.want, got, "the configured value is what this command reads")
			require.Equal(t, vcomputerservice.DefaultTimeout, vcomputerservice.EffectiveTimeout(got),
				"the bound a smoke run gets is the bound a deployment gets")
		})
	}
}

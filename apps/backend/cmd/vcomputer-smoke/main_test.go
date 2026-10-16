package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestControlTimeoutFallsBackToTheDefault: the command reads VCOMPUTER_CONTROL_TIMEOUT the way
// the loader does, so a value that is not a duration is the default rather than a refused run.
//
// The run is the only thing this command exists for, and the setting it refuses over bounds one
// call to an optional PoC: a tool that will not start on a typo would be a second place for issue
// #219 to happen, and the loader's own answer to the same value is a warning.
func TestControlTimeoutFallsBackToTheDefault(t *testing.T) {
	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "")
	require.Equal(t, defaultTimeout, controlTimeout(), "an unset value takes the default")

	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "banana")
	require.Equal(t, defaultTimeout, controlTimeout(), "a value that will not parse takes the default")

	t.Setenv("VCOMPUTER_CONTROL_TIMEOUT", "90s")
	require.Equal(t, 90*time.Second, controlTimeout(), "a value that parses is the one used")
}

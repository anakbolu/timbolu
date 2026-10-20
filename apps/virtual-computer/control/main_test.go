package main

import "testing"

// TestReapOnShutdownIsOffUnlessAsked is #263 as a test: a plain SIGTERM is what an
// ordinary restart looks like, and the sandboxes it would remove carry the bot's home
// volume — the browser profile and the files a session exists for. Only an explicit yes
// may remove them, and every other value has to mean keep: a deployment that sets the
// variable to "0", "no", or something it made up must not lose data over it.
func TestReapOnShutdownIsOffUnlessAsked(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"keep", false},
		{"1", true},
		{"true", true},
		{" 1 ", true},
		{"TRUE", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("VCOMPUTER_REAP_ON_SHUTDOWN", tc.value)
			if got := reapOnShutdown(); got != tc.want {
				t.Fatalf("reapOnShutdown() with VCOMPUTER_REAP_ON_SHUTDOWN=%q = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

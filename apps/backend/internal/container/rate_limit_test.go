package container

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/platform/ratelimit"
)

// TestAMalformedCeilingIsRefusedAtStartup is why the pairs are parsed rather than
// skipped: a ceiling that silently did not apply is a quota that silently gets
// spent, and the deployment would look like it was pacing.
func TestAMalformedCeilingIsRefusedAtStartup(t *testing.T) {
	for name, encoded := range map[string]string{
		"no colon":                   "gmail",
		"no number":                  "gmail:",
		"not a number":               "gmail:abc",
		"zero":                       "gmail:0",
		"negative":                   "gmail:-5",
		"no app":                     ":5",
		"an empty app":               " :5",
		"a bad pair among good ones": "gmail:10,nonsense",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRateLimits(encoded)
			require.Error(t, err, encoded)
		})
	}
}

// TestCeilingsAreParsed covers the shape a deployment writes.
func TestCeilingsAreParsed(t *testing.T) {
	limits, err := parseRateLimits("gmail:10, jira:5,notion:120")
	require.NoError(t, err)
	require.Equal(t, map[string]int{"gmail": 10, "jira": 5, "notion": 120}, limits)

	// No ceiling configured is not an error: it is a deployment that does not
	// pace, which is what a deployment without Redis does.
	limits, err = parseRateLimits("")
	require.NoError(t, err)
	require.Nil(t, limits)

	limits, err = parseRateLimits("  ,  ")
	require.NoError(t, err)
	require.Nil(t, limits)
}

// TestAnAppWithoutItsOwnCeilingFallsBack covers the two ways a ceiling is chosen.
func TestAnAppWithoutItsOwnCeilingFallsBack(t *testing.T) {
	limits := &callLimits{perMinute: map[string]int{"gmail": 10}, fallback: 3}

	require.Equal(t, 10, limits.ceiling("gmail"))
	require.Equal(t, 3, limits.ceiling("jira"), "an app with no ceiling of its own takes the default")

	noFallback := &callLimits{perMinute: map[string]int{"gmail": 10}}
	require.Zero(t, noFallback.ceiling("jira"), "no default and no ceiling means no pacing")
}

// TestALimiterThatCannotAnswerAllowsTheCall keeps Redis being unreachable from
// becoming a reason to lose a task. Losing pacing is recoverable; losing the work
// is not.
func TestALimiterThatCannotAnswerAllowsTheCall(t *testing.T) {
	// A limiter with no client cannot answer, which is the same situation as
	// Redis going away mid-run.
	limits := &callLimits{
		limiter:   ratelimit.New(nil),
		perMinute: map[string]int{"gmail": 10},
		logger:    zap.NewNop(),
	}

	decision, err := limits.Allow(t.Context(), "gmail", uuid.New())
	require.NoError(t, err, "a limiter that cannot answer is not an error for the caller")
	require.True(t, decision.Allowed)
}

// TestALimiterWithoutAConfigurationAllowsEverything keeps a deployment that never
// configured a ceiling from holding work nobody asked to have held.
func TestALimiterWithoutAConfigurationAllowsEverything(t *testing.T) {
	var absent *callLimits

	decision, err := absent.Allow(t.Context(), "gmail", uuid.Nil)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	require.Zero(t, decision.PerMinute)
}

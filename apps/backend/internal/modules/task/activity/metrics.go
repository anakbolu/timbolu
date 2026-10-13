package activity

import (
	"context"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// The task runtime's own measures: how much work the deployment accepted, what it cost, and how
// often an attempt had to be repeated.
//
// Two of these are here rather than anywhere else because this is where the fact is born. A retry
// is only visible to the attempt that *is* the retry — `activity.GetInfo` reports the attempt
// number, and nothing else in the deployment knows a second try happened. A draft's readiness is
// only known to the step that produced it, because that is the only place the task's own start and
// the draft's own creation are both in hand.

// retry records an activity attempt past the first.
//
// The attempt count comes from Temporal rather than from a tally here: a retry caused by a worker
// that died mid-activity never re-enters this code as a first attempt, so counting "how many times
// have I been called" would miss exactly the case that matters.
func (a *Activities) retry(ctx context.Context, name string) {
	if a.deps.Metrics == nil || !activity.IsActivity(ctx) {
		return
	}

	attempt := int(activity.GetInfo(ctx).Attempt)
	if attempt > 1 {
		a.deps.Metrics.ActivityRetry(ctx, name, attempt)
	}
}

// durationOf is how long a task ran.
//
// It prefers the two stored moments over any clock read here, because a task spans processes and
// restarts: the row's own start and finish are the run an operator is looking at, and a duration
// computed from the moment this activity happened to execute would disagree with the timestamps on
// the screen beside it.
func durationOf(task domain.Task, fallback time.Time) float64 {
	if task.StartedAt == nil {
		return 0
	}

	finished := fallback
	if task.FinishedAt != nil {
		finished = *task.FinishedAt
	}

	if finished.Before(*task.StartedAt) {
		return 0
	}

	return finished.Sub(*task.StartedAt).Seconds()
}

// appOfTool names the integration a tool belongs to, from the tool's own name.
//
// Tool names are `<app>.<operation>` by convention, which the registry enforces rather than a
// caller: the name is a constant in the code, so the prefix is a bounded value and safe as a label.
// A name that does not follow the convention is reported under the whole name, which is still
// bounded by the registry — the alternative, guessing, would file two tools under one app.
func appOfTool(name string) string {
	if app, _, found := strings.Cut(name, "."); found && app != "" {
		return app
	}

	return name
}

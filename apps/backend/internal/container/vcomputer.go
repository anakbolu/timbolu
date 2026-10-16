package container

import (
	"strings"

	"go.uber.org/zap"

	vcomputerdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/domain"
	vcomputerservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/service"
	vcomputertemporal "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/temporal"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/workflow"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// NewVComputerTasks builds the virtual computer's activity set.
//
// It is exported because two processes need the same set and must not build it two
// different ways: the agent worker, through the container, and cmd/vcomputer-smoke, which
// starts a worker of its own on its own queue to prove the seam end to end.
//
// An empty ControlURL leaves the set unconfigured rather than absent. That is deliberate:
// the activities are still registered, and each of them answers domain.ErrNotConfigured —
// which is a deployment without a virtual computer, and it is what every deployment is
// until one is running. The alternative, registering nothing when the address is empty,
// would make "this worker cannot drive a computer" and "this worker never heard of one"
// the same state from the outside, and a smoke run would then be unable to tell an
// operator which of the two they have.
//
// Whether one is configured is logged here, once, and never with the token: the address says
// which control plane the process is pointed at, which is the first thing to check when a run
// answers ErrNotConfigured, and the token is a shared secret with no business in a log line.
// An address with no token is a warning rather than a quiet "configured", because the control
// plane requires the bearer on every /v1 route: that deployment is complete-looking at startup
// and refused on every call.
//
// The timeout is logged as the one in force rather than as the one configured: a
// non-positive value is not a bound at all, so the client replaces it with
// vcomputerservice.DefaultTimeout — and a log line that repeated "-5s" while every call
// waited 30s is how an operator comes to believe the bound they wrote is being applied
// (issue #257). A configured value that is not in force is said so once, in a warning of
// its own, with the value that was given.
func NewVComputerTasks(cfg config.VComputerConfig, log *zap.Logger) *vcomputertemporal.Activities {
	if log == nil {
		log = zap.NewNop()
	}

	// The one place the rule lives: what the client enforces and what this function logs
	// are the same value, so neither can drift from the other.
	timeout := vcomputerservice.EffectiveTimeout(cfg.Timeout)

	// The interface is left nil rather than holding a typed nil: the client is only built
	// when there is an address to reach, so the activity set's own `Computer == nil` check is
	// what reports the missing configuration instead of a call that would fail anyway.
	var computer vcomputerdomain.ComputerUseClient
	url := strings.TrimSpace(cfg.ControlURL)
	if url == "" {
		log.Info("virtual computer is not configured: VCOMPUTER_CONTROL_URL is empty, " +
			"so every activity answers ErrNotConfigured")
	} else if strings.TrimSpace(cfg.Token) == "" {
		// The client is still built: the refusal is the honest answer, and treating this as
		// unconfigured would hide which of the two variables is missing.
		computer = vcomputerservice.New(url, cfg.Token, timeout)
		log.Warn("virtual computer is configured without a token: VCOMPUTER_CONTROL_TOKEN is "+
			"empty, so every call will be refused with a 401",
			zap.String("control_url", url),
			zap.Duration("timeout", timeout))
	} else {
		computer = vcomputerservice.New(url, cfg.Token, timeout)
		log.Info("virtual computer is configured",
			zap.String("control_url", url),
			zap.Duration("timeout", timeout))
	}

	if url != "" && cfg.Timeout <= 0 {
		// Only a configured virtual computer is worth a line: without an address no call is
		// made, so the bound is inert. The value is named here rather than in the line above,
		// which now reports the timeout in force; the shipped YAML sets 30s, so a zero that
		// arrives is an override or a config file that dropped the key, and either is worth
		// telling an operator about — the bound they are reading is not the one being applied.
		log.Warn("the virtual computer timeout is not a positive duration "+
			"(VCOMPUTER_CONTROL_TIMEOUT, or the config file's VComputer.Timeout), so it is no "+
			"bound: the control plane's calls use the default timeout instead",
			zap.Duration("configured_timeout", cfg.Timeout),
			zap.Duration("timeout", timeout))
	}

	return vcomputertemporal.New(vcomputertemporal.Deps{Computer: computer, Logger: log})
}

// openVComputer builds the virtual computer's activity set.
//
// It needs nothing else in the container — no database, no Redis, no Temporal client — which
// is why it is a step of its own rather than part of the memory or routine assembly: those
// two are disabled in a process without Postgres, and a bot's computer does not depend on
// what the deployment remembers.
//
// It runs in every process, like the task runtime does, because the container is the one
// assembly point the API and the workers share and the worker is the process that registers
// what is built here. Nothing calls the activities yet: the agent task loop and its tool
// catalog do not reach the sandbox (docs/adr/0003-virtual-computer.md), so this is what makes
// the smoke workflow reachable from a worker.
func (c *Container) openVComputer(cfg *config.Config) {
	c.VComputerTasks = NewVComputerTasks(cfg.VComputer, c.Logger)
}

// RegisterVComputerTasks installs the smoke workflow and the activity set on the agent
// worker.
//
// The activity set is registered even when no control plane is configured, and the workflow
// with it: a deployment without one can then be asked, with a real run, whether it has one —
// and the answer is the honest ErrNotConfigured on the first step rather than "unknown
// workflow type", which would say nothing about the deployment.
func RegisterVComputerTasks(w workerish, c *Container) {
	if c == nil || c.VComputerTasks == nil {
		return
	}

	w.RegisterWorkflow(workflow.SmokeWorkflow)
	w.RegisterActivity(c.VComputerTasks)
}

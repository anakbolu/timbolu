// Command vcomputer-smoke runs one virtual computer end to end: it starts a worker on a
// queue of its own, executes the smoke workflow on it, and stops the sandbox the run made.
//
// It is a development tool and it is not deployed. What it is for is the seam that no unit
// test covers: a real Temporal server, a real control plane, a real sandbox, and the
// activities this backend registers against them. A unit test proves the workflow schedules
// the right calls; only this proves the calls reach a desktop.
//
// It is not the agent worker. The worker here is a process that lives for one run and
// registers nothing but the virtual computer's activities, so a run says nothing about
// whether the agent worker's own task loop works — that loop does not reach the sandbox at
// all yet (docs/adr/0003-virtual-computer.md).
//
//	VCOMPUTER_CONTROL_URL=http://localhost:8090 VCOMPUTER_CONTROL_TOKEN=... \
//	  go run ./cmd/vcomputer-smoke -bot bot-1 -url https://example.com
//
// The run exits non-zero on the first failure and prints what the workflow observed as JSON,
// so it can be read by a person and diffed by a script. It leaves no sandbox behind: the
// workflow ends the session it started, so a run costs one container that is gone by the time
// the command exits. `-keep-session` is the way to ask for the opposite, and `-out` needs it,
// because the capture it takes is asked of a sandbox that would otherwise already be down.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/container"
	vcomputerservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/service"
	vcomputerworkflow "github.com/robzlabz/employeebot/apps/backend/internal/modules/vcomputer/workflow"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/temporal"
	"github.com/robzlabz/employeebot/packages/platform/config"
)

// defaultTimeout is what one call to the control plane gets when VCOMPUTER_CONTROL_TIMEOUT says
// nothing or says something that is not a duration, matching the configuration default.
const defaultTimeout = 30 * time.Second

// runTimeout bounds the whole run. A smoke run is a person watching it, and a sandbox that
// never answers must not leave them with a process that hangs rather than a message.
const runTimeout = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "vcomputer-smoke: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	hostPort := flag.String("temporal", envOr("TEMPORAL_HOST_PORT", "127.0.0.1:7233"),
		"Temporal frontend address ($TEMPORAL_HOST_PORT)")
	namespace := flag.String("namespace", envOr("TEMPORAL_NAMESPACE", "default"), "Temporal namespace")
	// The queue is its own rather than the agent worker's, so a smoke run cannot be picked up
	// by a deployed worker and cannot leave anything behind on a queue that carries real work.
	queue := flag.String("queue", "vcomputer-smoke", "task queue to run the worker on")
	botID := flag.String("bot", "", "bot whose virtual computer to drive (required)")
	pageURL := flag.String("url", "", "page to point the browser at (required)")
	clickX := flag.Int("click-x", 0, "x coordinate to click after the page loads")
	clickY := flag.Int("click-y", 0, "y coordinate to click after the page loads")
	text := flag.String("text", "", "text to type after the click")
	// The session is stopped at the end of the run unless this is set, because the sandbox
	// a smoke run leaves behind is a container and a volume an operator deletes by hand
	// later. The flag is here for the one case that needs the desktop to outlive the run:
	// -out takes its capture after the workflow is done.
	keepSession := flag.Bool("keep-session", false,
		"leave the sandbox running after the run, so the desktop can be looked at afterwards")
	outDir := flag.String("out", "", "directory to write after-temporal-final.png into (optional, and only with -keep-session)")
	flag.Parse()

	if strings.TrimSpace(*botID) == "" {
		return errors.New("-bot is required")
	}
	if strings.TrimSpace(*pageURL) == "" {
		return errors.New("-url is required")
	}
	if *outDir != "" && !*keepSession {
		// Refused here rather than discovered as a 404 at the end: the capture is asked of the
		// sandbox after the workflow is done, and by default the workflow is what stopped that
		// sandbox. An operator who asked for a file gets told which flag it needs before a
		// worker starts, not after a whole run.
		return errors.New("-out needs -keep-session: the run stops the sandbox it started")
	}

	controlURL := strings.TrimSpace(os.Getenv("VCOMPUTER_CONTROL_URL"))
	if controlURL == "" {
		// Refused here rather than left to the workflow: the run would fail on its first
		// activity with the same answer, and a tool whose whole purpose is reaching a control
		// plane should say that it has none before it starts a worker.
		return errors.New("VCOMPUTER_CONTROL_URL is empty: there is no control plane to smoke")
	}

	timeout := controlTimeout()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	// The token is read here and never printed: the line below names the address only.
	cfg := config.VComputerConfig{
		ControlURL: controlURL,
		Token:      os.Getenv("VCOMPUTER_CONTROL_TOKEN"),
		Timeout:    timeout,
	}

	log, err := zap.NewDevelopment()
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	temporalClient, err := client.Dial(client.Options{
		HostPort:  *hostPort,
		Namespace: *namespace,
		Logger:    temporal.NewZapLogger(log),
	})
	if err != nil {
		return fmt.Errorf("dial temporal at %s: %w", *hostPort, err)
	}
	defer temporalClient.Close()

	activities := container.NewVComputerTasks(cfg, log)
	worker := sdkworker.New(temporalClient, *queue, sdkworker.Options{})
	// The registration is the agent worker's own: a run that used a different registration
	// would prove something about this command rather than about what a deployment installs.
	container.RegisterVComputerTasks(worker, &container.Container{VComputerTasks: activities})

	if err := worker.Start(); err != nil {
		return fmt.Errorf("start worker on %s: %w", *queue, err)
	}
	defer worker.Stop()

	in := vcomputerworkflow.SmokeInput{
		BotID:       *botID,
		URL:         *pageURL,
		ClickX:      *clickX,
		ClickY:      *clickY,
		Text:        *text,
		KeepSession: *keepSession,
	}

	run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// The id carries the second the run started, so a retry of the same command is a new
		// run rather than a conflict with the last one.
		ID:        fmt.Sprintf("vcomputer-smoke-%d", time.Now().Unix()),
		TaskQueue: *queue,
	}, vcomputerworkflow.SmokeWorkflow, in)
	if err != nil {
		return fmt.Errorf("start smoke workflow: %w", err)
	}

	fmt.Printf("workflow %s run %s\n", run.GetID(), run.GetRunID())

	var result vcomputerworkflow.SmokeResult
	if err := run.Get(ctx, &result); err != nil {
		return fmt.Errorf("smoke workflow %s: %w", run.GetID(), err)
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	fmt.Println(string(encoded))

	if *outDir != "" {
		// The capture is taken outside the workflow, and that is the second half of the
		// check: the bytes the workflow's own Screenshot activity returned were serialized
		// into a workflow result and are not printed, so this is what proves a caller can
		// still ask the control plane for an image after the run — and gives an operator
		// something to look at. It is asked of a sandbox the run left up, which is what
		// -keep-session is for; the flag pair is checked before the worker starts.
		path, err := saveFinalCapture(ctx, cfg, *botID, *outDir)
		if err != nil {
			return err
		}
		fmt.Printf("saved %s\n", path)
	}

	return nil
}

// controlTimeout reads VCOMPUTER_CONTROL_TIMEOUT the way the configuration does, so a value this
// command accepts is a value a deployment accepts: one that will not parse is reported and the
// default is used rather than the run refused, which is what the loader does with it (issue #219).
func controlTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("VCOMPUTER_CONTROL_TIMEOUT"))
	if raw == "" {
		return defaultTimeout
	}

	timeout, err := time.ParseDuration(raw)
	if err != nil {
		// On stderr, so the JSON a caller reads and diffs stays the only thing on stdout.
		fmt.Fprintf(os.Stderr,
			"vcomputer-smoke: VCOMPUTER_CONTROL_TIMEOUT=%q is not a duration, using %s: %v\n",
			raw, defaultTimeout, err)

		return defaultTimeout
	}
	return timeout
}

// saveFinalCapture asks the control plane for one more screenshot, writes it to outDir, and
// describes what it wrote. It needs a live session, which is why the caller refuses -out
// without -keep-session: the workflow stops the sandbox it started, and the capture is asked
// for after the workflow is done.
func saveFinalCapture(ctx context.Context, cfg config.VComputerConfig, botID, outDir string) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", outDir, err)
	}

	computer := vcomputerservice.New(cfg.ControlURL, cfg.Token, cfg.Timeout)
	png, err := computer.Screenshot(ctx, botID)
	if err != nil {
		return "", fmt.Errorf("capture after the run: %w", err)
	}

	path := filepath.Join(outDir, "after-temporal-final.png")
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}

	return fmt.Sprintf("%s (%d bytes)", path, len(png)), nil
}

// envOr reads an environment variable, falling back to a default.
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

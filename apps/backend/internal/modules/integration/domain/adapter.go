package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This file is the adapter contract. An application's entire surface — what it
// can do, what it can tell us about, and how to run one call — is behind these
// types, so the agent worker never learns that Gmail and Jira are different
// things. Adding an application is adding an adapter.

// Trigger kinds, matching what a routine can be started by.
const (
	// TriggerWebhook is pushed to us by the provider.
	TriggerWebhook = "webhook"
	// TriggerPolling is asked for on a schedule, for a provider with no webhook.
	TriggerPolling = "polling"
)

// Errors the registry reports.
var (
	// ErrAdapterExists means two adapters claimed the same application.
	ErrAdapterExists = errors.New("integration: an adapter for this app is already registered")
	// ErrUnknownAdapter means no adapter handles an application.
	ErrUnknownAdapter = errors.New("integration: no adapter for this app")
	// ErrUnknownTool means the adapter has no tool by that name.
	ErrUnknownTool = errors.New("integration: unknown tool")
	// ErrInvalidSpec means an adapter declared something the registry refuses,
	// which is a programming mistake in the adapter rather than a user error.
	ErrInvalidSpec = errors.New("integration: the adapter declared an invalid spec")
	// ErrSchemaViolation means a call's arguments did not match the tool's
	// schema. It is reported before anything is sent, so a malformed call costs
	// nothing.
	ErrSchemaViolation = errors.New("integration: the arguments do not match the tool's schema")
)

// ToolSpec is one thing an application can do.
//
// The label is the only thing that decides whether a person is asked before the
// call goes out: `write_external` always is, the other two never are. An adapter
// states the label; nothing downstream re-decides it.
type ToolSpec struct {
	// Name is the tool's identity and the model's handle on it, as
	// `app.action`: `gmail.send`, `jira.create_issue`.
	Name string
	// Label is one of the Label* constants.
	Label string
	// Description is what the model reads when choosing a tool.
	Description string
	// Schema is the JSON Schema of the arguments.
	Schema json.RawMessage
}

// Valid checks the spec the way the registry and the schema do, so a mistake in
// an adapter is caught at startup rather than at the first call.
func (s ToolSpec) Valid() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: a tool needs a name", ErrInvalidSpec)
	}
	if !strings.Contains(s.Name, ".") {
		return fmt.Errorf("%w: %q must be named `app.action`", ErrInvalidSpec, s.Name)
	}
	switch s.Label {
	case LabelRead, LabelWriteInternal, LabelWriteExternal:
	default:
		return fmt.Errorf("%w: %q has label %q", ErrInvalidSpec, s.Name, s.Label)
	}
	if len(s.Schema) == 0 {
		return fmt.Errorf("%w: %q has no schema", ErrInvalidSpec, s.Name)
	}
	if !json.Valid(s.Schema) {
		return fmt.Errorf("%w: %q has a schema that is not JSON", ErrInvalidSpec, s.Name)
	}
	return nil
}

// App is the application part of a tool name: `gmail` for `gmail.send`.
func (s ToolSpec) App() string {
	app, _, _ := strings.Cut(s.Name, ".")
	return app
}

// TriggerSpec is something an application can tell us about.
type TriggerSpec struct {
	// App is the application that reports it.
	App string
	// Kind is TriggerWebhook or TriggerPolling.
	Kind string
	// Event is what happened, as `app.thing`: `gmail.email_received`.
	Event string
	// Description is what a routine screen shows when offering the trigger.
	Description string
}

// Valid checks the trigger the way the registry does.
func (t TriggerSpec) Valid() error {
	if strings.TrimSpace(t.App) == "" {
		return fmt.Errorf("%w: a trigger needs an app", ErrInvalidSpec)
	}
	switch t.Kind {
	case TriggerWebhook, TriggerPolling:
	default:
		return fmt.Errorf("%w: trigger %q has kind %q", ErrInvalidSpec, t.Event, t.Kind)
	}
	if !strings.Contains(t.Event, ".") {
		return fmt.Errorf("%w: trigger %q must be named `app.thing`", ErrInvalidSpec, t.Event)
	}
	return nil
}

// Call is one tool invocation, with the credential already opened.
//
// The token is here rather than fetched by the adapter so that "decrypt only
// where it is used" stays one path: the runtime opens it once, hands it over,
// and the adapter never touches the key ring.
type Call struct {
	Scope         Scope
	IntegrationID uuid.UUID
	Tool          string
	Arguments     json.RawMessage
	Token         Token
}

// Result is what a tool answered.
type Result struct {
	// Content is what the model reads. It is JSON so a provider's own shape
	// survives to the model instead of being flattened into prose.
	Content json.RawMessage
	// Summary is one line for the step ledger and the task feed, so a person can
	// see what happened without reading the payload.
	Summary string
}

// LimitDecision is what a limiter answers about one call.
type LimitDecision struct {
	// Allowed says whether the call may go out now.
	Allowed bool
	// RetryAfter is how long to wait when it may not. A caller holds the work for
	// that long rather than failing it: a limit is a pace, not a refusal.
	RetryAfter time.Duration
	// PerMinute is the ceiling in force, so what a person reads can name it
	// instead of saying "too many".
	PerMinute int
}

// CallLimiter decides whether a workspace may spend one call on an integration.
//
// It is a port because the bucket lives in Redis: the domain layer holds no
// infrastructure, and the container hands over the platform limiter.
type CallLimiter interface {
	// Allow takes one token for a connection, or reports how long to wait.
	Allow(ctx context.Context, app string, integrationID uuid.UUID) (LimitDecision, error)
}

// RateLimitedError says a call was held rather than refused.
//
// It is a type rather than a bare sentinel because the wait is the useful part:
// the runtime schedules its retry from it instead of guessing, and what a person
// reads names the ceiling.
type RateLimitedError struct {
	App        string
	PerMinute  int
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("%s: %s allows %d calls a minute and they are spent; retry in %s",
		ErrRateLimited.Error(), e.App, e.PerMinute, e.RetryAfter.Round(time.Second))
}

// Unwrap makes the error answer errors.Is for ErrRateLimited, so a caller that
// only needs "was this a limit" does not have to know the type.
func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// Adapter is one application's whole surface.
//
// Everything provider-specific lives behind this: the HTTP calls, the OAuth
// endpoints, the pagination, the error shapes. The runtime only sees tools with
// labels and a call that returns a result.
type Adapter interface {
	// App is the application key, as it appears in `integrations.app` and as the
	// prefix of every tool name: `gmail`.
	App() string
	// Tools is what this application can do.
	Tools() []ToolSpec
	// Triggers is what this application can tell us about. An application with
	// nothing to report returns an empty list.
	Triggers() []TriggerSpec
	// Execute runs one call. The token is already open, and the adapter must not
	// log it.
	Execute(ctx context.Context, call Call) (Result, error)
}

// SchemaValidator compiles and applies the JSON Schema of a tool's arguments.
//
// It is a port because validating a schema means a JSON Schema library, and the
// domain layer holds no dependencies beyond the standard library. The container
// supplies an implementation.
type SchemaValidator interface {
	// Validate reports whether the arguments match the schema. A mismatch is
	// ErrSchemaViolation with the reason attached.
	Validate(schema, arguments json.RawMessage) error
}

// Registry is every adapter this deployment knows, by application.
type Registry interface {
	// Register adds an adapter. A second adapter for the same app is refused
	// rather than silently replacing the first: which one won would depend on
	// the order the container happened to build them in.
	Register(adapter Adapter) error
	// Apps lists the registered application keys, sorted.
	Apps() []string
	// Adapter returns the adapter for an application.
	Adapter(app string) (Adapter, bool)
	// Tools lists every tool of every adapter, sorted by name.
	Tools() []ToolSpec
	// ToolsFor lists one application's tools, sorted by name.
	ToolsFor(app string) []ToolSpec
	// Triggers lists every trigger of every adapter, sorted.
	Triggers() []TriggerSpec
	// Lookup finds a tool by its name.
	Lookup(name string) (ToolSpec, Adapter, bool)
}

// ToolName builds a tool name from an application and an action, so the
// convention is stated once rather than in every adapter.
func ToolName(app, action string) string {
	return app + "." + action
}

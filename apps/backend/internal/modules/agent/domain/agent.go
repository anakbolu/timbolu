// Package domain defines the agent registry contracts: the Bolu profile, the
// derived display status, the tool and grant shapes, and the repository and
// service interfaces.
//
// The display status is never stored. Only the rest switch is a column; the
// rest is computed from the tasks and drafts that reference the agent, so the
// registry cannot drift from the work.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// StoredStatus is the agent's own switch.
const (
	StoredActive  = "active"
	StoredResting = "resting"
)

// DisplayStatus is what the UI shows. It is derived, never stored.
const (
	// DisplayWorking: at least one task is queued or running.
	DisplayWorking = "working"
	// DisplayWaiting: no task in flight, but a draft is waiting for a human.
	DisplayWaiting = "waiting"
	// DisplayIdle: nothing in flight, nothing waiting, and the switch is on.
	DisplayIdle = "idle"
	// DisplayResting: the rest switch is off.
	DisplayResting = "resting"
)

// Tool labels decide whether a call needs human approval.
const (
	LabelRead          = "read"
	LabelWriteInternal = "write_internal"
	LabelWriteExternal = "write_external"
)

// Grant permissions.
const (
	PermissionRead      = "read"
	PermissionReadWrite = "read_write"
)

// Errors returned by the service.
var (
	ErrAgentNotFound       = errors.New("agent: bolu not found")
	ErrTeamNotFound        = errors.New("agent: team not found")
	ErrInvalidInput        = errors.New("agent: invalid input")
	ErrInvalidStatus       = errors.New("agent: unknown status")
	ErrInvalidPermission   = errors.New("agent: unknown permission")
	ErrAgentHasRunningTask = errors.New("agent: this bolu is still working on a task")
	ErrAgentLimitReached   = errors.New("agent: the plan's bolu limit is reached")
	ErrIntegrationNotFound = errors.New("agent: integration not found")
	// ErrOrderIncomplete: the arrangement a client sent is not the workspace's
	// registry. A partial layout is refused rather than applied, because the rows
	// it forgot would keep the positions they had and the list would interleave
	// two arrangements.
	ErrOrderIncomplete = errors.New("agent: the arrangement does not list every bolu exactly once")
)

// Agent is one Bolu.
type Agent struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	TeamID      uuid.UUID
	TeamName    string
	TeamKind    string

	Name    string
	Role    string
	Persona string
	Tone    string
	Shape   string
	Color   string

	// Position is where the Bolu sits in its team's list, counting from zero. The
	// sidebar orders by it, and it is the only part of a Bolu that a drag changes.
	Position int32

	// Status is the stored switch (active or resting).
	Status string
	// Display is the derived status the UI shows.
	Display string
	// Reason explains a working or waiting status.
	Reason *Reason

	// MemoryExtraction is the switch that decides whether a quiet conversation is
	// read into memory automatically. A person can still ask for something to be
	// remembered while it is off: the switch governs the extractor, not the user.
	MemoryExtraction bool

	TemplateKey  string
	Tools        []string
	DefaultModel map[string]any

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Reason is why an agent is working or waiting.
type Reason struct {
	Kind      string    `json:"kind"` // task | draft
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title,omitempty"`
	Status    string    `json:"status,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Team groups the Bolu of one workspace.
type Team struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	Agents []Agent   `json:"agents"`
}

// Tool is one entry of the tool catalogue.
type Tool struct {
	Name        string `json:"name"`
	Integration string `json:"integration_app"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// NeedsConnection is false for a tool that belongs to the workspace itself — reading its product
	// catalogue, recording an order — rather than to a connected account. The tool filter offers such
	// a tool to any Bolu whose profile asks for it, and the hub runs it without a connection.
	NeedsConnection bool `json:"needs_connection"`
}

// Grant gives one agent access to one integration.
type Grant struct {
	ID            uuid.UUID `json:"id"`
	AgentID       uuid.UUID `json:"agent_id"`
	IntegrationID uuid.UUID `json:"integration_id"`
	App           string    `json:"app"`
	AccountLabel  string    `json:"account_label"`
	Status        string    `json:"status"`
	Permission    string    `json:"permission"`
}

// Integration is a connected application of the workspace.
type Integration struct {
	ID           uuid.UUID `json:"id"`
	App          string    `json:"app"`
	AccountLabel string    `json:"account_label"`
	Status       string    `json:"status"`
}

// Activity is the raw input of the derived status.
type Activity struct {
	Working bool
	Waiting bool
}

// DeriveStatus turns the stored switch and the activity into the display status.
// The order matters: a resting Bolu is resting even if it still has a task
// finishing up, and work outranks a pending draft.
func DeriveStatus(stored string, activity Activity) string {
	if stored == StoredResting {
		return DisplayResting
	}
	switch {
	case activity.Working:
		return DisplayWorking
	case activity.Waiting:
		return DisplayWaiting
	default:
		return DisplayIdle
	}
}

// AcceptsNewTasks reports whether the agent may be given new work. A resting
// Bolu never accepts new tasks; the ones already running are left to finish.
func AcceptsNewTasks(stored string) bool {
	return stored == StoredActive
}

// CreateRequest is a new Bolu. CopyFrom copies an existing agent's profile, and
// TemplateKey starts from a template; both are optional.
type CreateRequest struct {
	TeamID      uuid.UUID
	Name        string
	Role        string
	Persona     string
	Tone        string
	Shape       string
	Color       string
	Tools       []string
	TemplateKey string
	CopyFrom    uuid.UUID
}

// UpdateRequest edits a Bolu profile.
type UpdateRequest struct {
	Name         string
	Role         string
	Persona      string
	Tone         string
	Shape        string
	Color        string
	Tools        []string
	DefaultModel map[string]any
}

// Arrangement is the sidebar's layout: which team each Bolu is in, and in what
// order, as one whole list.
//
// It is sent whole rather than as a single move because the two are not the same
// claim. "Move Oren down one" is a statement about a list the client read at some
// moment; "the sidebar reads Oren, Senja, Kopi" is a statement about the list
// itself, and it is what makes a drag idempotent — the same body sent twice leaves
// the same sidebar, and a second tab that reordered in the meantime cannot produce
// two Bolu in the same slot.
type Arrangement struct {
	Teams []TeamArrangement
}

// TeamArrangement is one team's row of the sidebar, in order.
type TeamArrangement struct {
	TeamID uuid.UUID
	// Agents is the team's Bolu, first to last. It must name every live Bolu of
	// the team exactly once; a list that does not is refused.
	Agents []uuid.UUID
}

// Placement is one Bolu's resolved position: the team it belongs to and where it
// sits in that team's list.
type Placement struct {
	AgentID  uuid.UUID
	TeamID   uuid.UUID
	Position int32
}

// Repository is the persistence contract. Every method runs inside the caller's
// tenant scope, so Row Level Security applies.
type Repository interface {
	List(ctx context.Context, scope Scope) ([]Agent, error)
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Agent, error)
	Create(ctx context.Context, scope Scope, agent NewAgent) (Agent, error)
	Update(ctx context.Context, scope Scope, id uuid.UUID, req UpdateRequest) (Agent, error)
	SetStatus(ctx context.Context, scope Scope, id uuid.UUID, status string) (Agent, error)
	SetMemoryExtraction(ctx context.Context, scope Scope, id uuid.UUID, enabled bool) (Agent, error)
	// SetPlacements writes a whole arrangement in one transaction: either every
	// Bolu lands where the arrangement puts it or none of them move.
	SetPlacements(ctx context.Context, scope Scope, placements []Placement) error
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error

	Activity(ctx context.Context, scope Scope) (map[uuid.UUID]Activity, error)
	ActivityFor(ctx context.Context, scope Scope, id uuid.UUID) (Activity, error)
	ReasonFor(ctx context.Context, scope Scope, id uuid.UUID) (*Reason, error)
	RunningTasks(ctx context.Context, scope Scope, id uuid.UUID) (int64, error)
	CountLive(ctx context.Context, scope Scope) (int64, error)

	Teams(ctx context.Context, scope Scope) ([]Team, error)
	Templates(ctx context.Context) ([]Template, error)
	Template(ctx context.Context, key string) (Template, error)
	AgentTemplate(ctx context.Context, key string) (NewAgent, error)

	Grants(ctx context.Context, scope Scope, agentID uuid.UUID) ([]Grant, error)
	SetGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID, permission string) (Grant, error)
	DeleteGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID) error
	AllowedTools(ctx context.Context, scope Scope, agentID uuid.UUID) ([]Tool, error)
	ToolCatalog(ctx context.Context) ([]Tool, error)
	Integrations(ctx context.Context, scope Scope) ([]Integration, error)
}

// NewAgent is the row to insert.
type NewAgent struct {
	TeamID       uuid.UUID
	Name         string
	Role         string
	Persona      string
	Tone         string
	Shape        string
	Color        string
	Tools        []string
	TemplateKey  string
	DefaultModel map[string]any
}

// Template is a seeded Bolu profile.
type Template struct {
	Key          string
	Name         string
	Role         string
	Persona      string
	Tone         string
	Shape        string
	Color        string
	Tools        []string
	Integrations []string
	SortOrder    int32
}

// Scope is the identity a query runs as.
type Scope struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
}

// IsZero reports whether the scope carries no identity.
func (s Scope) IsZero() bool {
	return s.UserID == uuid.Nil && s.WorkspaceID == uuid.Nil
}

// PlanLimits reports how many Bolu a workspace may have, which is the subscription's
// answer. A deployment without billing leaves it unwired, and the registry then uses
// its own configured cap alone.
type PlanLimits interface {
	MaxAgents(ctx context.Context, workspaceID uuid.UUID) (int, error)
}

// Service is the agent registry use case contract.
type Service interface {
	List(ctx context.Context, scope Scope) ([]Agent, error)
	Get(ctx context.Context, scope Scope, id uuid.UUID) (Agent, error)
	Create(ctx context.Context, scope Scope, req CreateRequest) (Agent, error)
	Update(ctx context.Context, scope Scope, id uuid.UUID, req UpdateRequest) (Agent, error)
	SetStatus(ctx context.Context, scope Scope, id uuid.UUID, status string) (Agent, error)
	SetMemoryExtraction(ctx context.Context, scope Scope, id uuid.UUID, enabled bool) (Agent, error)
	Delete(ctx context.Context, scope Scope, id uuid.UUID) error

	// Arrange applies the sidebar's layout and answers with the teams as they now
	// read, so the screen that dragged a Bolu renders the stored order rather
	// than the one it hoped for.
	Arrange(ctx context.Context, scope Scope, arrangement Arrangement) ([]Team, error)

	Teams(ctx context.Context, scope Scope) ([]Team, error)
	Templates(ctx context.Context) ([]Template, error)

	Grants(ctx context.Context, scope Scope, agentID uuid.UUID) ([]Grant, error)
	SetGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID, permission string) (Grant, error)
	DeleteGrant(ctx context.Context, scope Scope, agentID, integrationID uuid.UUID) error
	AllowedTools(ctx context.Context, scope Scope, agentID uuid.UUID) ([]Tool, error)
	ToolCatalog(ctx context.Context) ([]Tool, error)
	Integrations(ctx context.Context, scope Scope) ([]Integration, error)

	// AcceptsNewTasks reports whether a Bolu may be given work. The task runtime
	// (EPIC 6, #52) calls it before starting a task.
	AcceptsNewTasks(ctx context.Context, scope Scope, id uuid.UUID) (bool, error)
}

// MarshalTools renders a tool list for the JSONB column.
func MarshalTools(tools []string) ([]byte, error) {
	if tools == nil {
		tools = []string{}
	}
	return json.Marshal(tools)
}

// UnmarshalTools reads a tool list from the JSONB column.
func UnmarshalTools(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var tools []string
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	if tools == nil {
		return []string{}, nil
	}
	return tools, nil
}

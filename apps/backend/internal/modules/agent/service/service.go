// Package service implements the agent registry use cases: the Bolu profile
// CRUD, the derived status, the teams, and the tool grants.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
)

// PersonaMaxLength bounds the persona field. A persona is injected into every
// prompt, so an unbounded one would quietly multiply the token cost of every
// task.
const PersonaMaxLength = 4000

// NameMaxLength bounds the display name.
const NameMaxLength = 60

// Config holds the service settings.
type Config struct {
	// MaxAgentsPerWorkspace caps the registry when the plan does not say
	// otherwise. Zero means the plan decides alone.
	MaxAgentsPerWorkspace int
}

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Repository
	// Limits reports the plan's Bolu allowance. Optional: without it the
	// registry uses Config.MaxAgentsPerWorkspace, and with neither the registry
	// is unlimited.
	Limits domain.PlanLimits
	Config Config
}

type service struct {
	deps   domain.Repository
	limits domain.PlanLimits
	cfg    Config
}

// New builds the agent service.
func New(deps Deps) domain.Service {
	return &service{deps: deps.Repository, limits: deps.Limits, cfg: deps.Config}
}

// List returns every live Bolu with its derived status.
func (s *service) List(ctx context.Context, scope domain.Scope) ([]domain.Agent, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	agents, err := s.deps.List(ctx, scope)
	if err != nil {
		return nil, err
	}

	activity, err := s.deps.Activity(ctx, scope)
	if err != nil {
		return nil, err
	}

	for i := range agents {
		agents[i].Display = domain.DeriveStatus(agents[i].Status, activity[agents[i].ID])
	}

	return agents, nil
}

// Get returns one Bolu with its derived status and the reason behind it.
func (s *service) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Agent, error) {
	if err := requireScope(scope); err != nil {
		return domain.Agent{}, err
	}

	agent, err := s.deps.Get(ctx, scope, id)
	if err != nil {
		return domain.Agent{}, err
	}

	return s.decorate(ctx, scope, agent)
}

// Create adds a Bolu. The profile comes from the request, from a template, or
// from an existing Bolu, in that order of precedence.
func (s *service) Create(ctx context.Context, scope domain.Scope, req domain.CreateRequest) (domain.Agent, error) {
	if err := requireScope(scope); err != nil {
		return domain.Agent{}, err
	}

	input, err := s.buildNewAgent(ctx, scope, req)
	if err != nil {
		return domain.Agent{}, err
	}

	if err := s.checkLimit(ctx, scope); err != nil {
		return domain.Agent{}, err
	}

	created, err := s.deps.Create(ctx, scope, input)
	if err != nil {
		return domain.Agent{}, err
	}

	return s.decorate(ctx, scope, created)
}

// Update edits a Bolu profile. The change takes effect on the next task,
// because the persona is read when the prompt is assembled.
func (s *service) Update(ctx context.Context, scope domain.Scope, id uuid.UUID, req domain.UpdateRequest) (domain.Agent, error) {
	if err := requireScope(scope); err != nil {
		return domain.Agent{}, err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return domain.Agent{}, fmt.Errorf("%w: name is required", domain.ErrInvalidInput)
	}
	if len([]rune(name)) > NameMaxLength {
		return domain.Agent{}, fmt.Errorf("%w: name is longer than %d characters", domain.ErrInvalidInput, NameMaxLength)
	}
	if len([]rune(req.Persona)) > PersonaMaxLength {
		return domain.Agent{}, fmt.Errorf("%w: persona is longer than %d characters", domain.ErrInvalidInput, PersonaMaxLength)
	}

	tools := req.Tools
	if tools == nil {
		// An omitted tool list keeps the current one, so an edit that only
		// changes the persona does not silently strip the agent's tools.
		current, err := s.deps.Get(ctx, scope, id)
		if err != nil {
			return domain.Agent{}, err
		}
		tools = current.Tools
	}

	updated, err := s.deps.Update(ctx, scope, id, domain.UpdateRequest{
		Name:         name,
		Role:         strings.TrimSpace(req.Role),
		Persona:      strings.TrimSpace(req.Persona),
		Tone:         strings.TrimSpace(req.Tone),
		Shape:        strings.TrimSpace(req.Shape),
		Color:        strings.TrimSpace(req.Color),
		Tools:        tools,
		DefaultModel: req.DefaultModel,
	})
	if err != nil {
		return domain.Agent{}, err
	}

	return s.decorate(ctx, scope, updated)
}

// SetStatus flips the rest switch.
func (s *service) SetStatus(ctx context.Context, scope domain.Scope, id uuid.UUID, status string) (domain.Agent, error) {
	if err := requireScope(scope); err != nil {
		return domain.Agent{}, err
	}
	if status != domain.StoredActive && status != domain.StoredResting {
		return domain.Agent{}, fmt.Errorf("%w: %q", domain.ErrInvalidStatus, status)
	}

	updated, err := s.deps.SetStatus(ctx, scope, id, status)
	if err != nil {
		return domain.Agent{}, err
	}

	return s.decorate(ctx, scope, updated)
}

// SetMemoryExtraction flips automatic memory extraction for one Bolu.
func (s *service) SetMemoryExtraction(ctx context.Context, scope domain.Scope, id uuid.UUID, enabled bool) (domain.Agent, error) {
	if err := requireScope(scope); err != nil {
		return domain.Agent{}, err
	}

	updated, err := s.deps.SetMemoryExtraction(ctx, scope, id, enabled)
	if err != nil {
		return domain.Agent{}, err
	}

	return s.decorate(ctx, scope, updated)
}

// Delete removes a Bolu from the registry. It refuses while the Bolu is still
// working, because the task would be left without its agent.
func (s *service) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	if err := requireScope(scope); err != nil {
		return err
	}

	running, err := s.deps.RunningTasks(ctx, scope, id)
	if err != nil {
		return err
	}
	if running > 0 {
		return fmt.Errorf("%w (%d task berjalan)", domain.ErrAgentHasRunningTask, running)
	}

	return s.deps.Delete(ctx, scope, id)
}

// Teams lists the teams with their Bolu, each carrying its derived status.
func (s *service) Teams(ctx context.Context, scope domain.Scope) ([]domain.Team, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}

	teams, err := s.deps.Teams(ctx, scope)
	if err != nil {
		return nil, err
	}

	activity, err := s.deps.Activity(ctx, scope)
	if err != nil {
		return nil, err
	}

	for i := range teams {
		for j := range teams[i].Agents {
			agent := &teams[i].Agents[j]
			agent.Display = domain.DeriveStatus(agent.Status, activity[agent.ID])
		}
	}

	return teams, nil
}

// Templates lists the seeded Bolu profiles.
func (s *service) Templates(ctx context.Context) ([]domain.Template, error) {
	// The template list is read through the repository's tenant-scoped helpers,
	// so it needs a scope; any authenticated user may read it.
	return s.deps.Templates(ctx)
}

// Grants lists the grants of one Bolu.
func (s *service) Grants(ctx context.Context, scope domain.Scope, agentID uuid.UUID) ([]domain.Grant, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	if _, err := s.deps.Get(ctx, scope, agentID); err != nil {
		return nil, err
	}
	return s.deps.Grants(ctx, scope, agentID)
}

// SetGrant gives a Bolu access to an integration. Revoking a grant immediately
// removes the matching tools from the next task's tool list.
func (s *service) SetGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID, permission string) (domain.Grant, error) {
	if err := requireScope(scope); err != nil {
		return domain.Grant{}, err
	}
	if permission != domain.PermissionRead && permission != domain.PermissionReadWrite {
		return domain.Grant{}, fmt.Errorf("%w: %q", domain.ErrInvalidPermission, permission)
	}
	if _, err := s.deps.Get(ctx, scope, agentID); err != nil {
		return domain.Grant{}, err
	}

	return s.deps.SetGrant(ctx, scope, agentID, integrationID, permission)
}

// DeleteGrant revokes a grant.
func (s *service) DeleteGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	return s.deps.DeleteGrant(ctx, scope, agentID, integrationID)
}

// AllowedTools is the tool list a task may offer this Bolu.
func (s *service) AllowedTools(ctx context.Context, scope domain.Scope, agentID uuid.UUID) ([]domain.Tool, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	if _, err := s.deps.Get(ctx, scope, agentID); err != nil {
		return nil, err
	}
	return s.deps.AllowedTools(ctx, scope, agentID)
}

// ToolCatalog lists every known tool with its label.
func (s *service) ToolCatalog(ctx context.Context) ([]domain.Tool, error) {
	return s.deps.ToolCatalog(ctx)
}

// Integrations lists the workspace's connected applications.
func (s *service) Integrations(ctx context.Context, scope domain.Scope) ([]domain.Integration, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	return s.deps.Integrations(ctx, scope)
}

// AcceptsNewTasks reports whether a Bolu may be given work.
func (s *service) AcceptsNewTasks(ctx context.Context, scope domain.Scope, id uuid.UUID) (bool, error) {
	agent, err := s.deps.Get(ctx, scope, id)
	if err != nil {
		return false, err
	}
	return domain.AcceptsNewTasks(agent.Status), nil
}

// decorate fills in the derived status and the reason behind it.
func (s *service) decorate(ctx context.Context, scope domain.Scope, agent domain.Agent) (domain.Agent, error) {
	activity, err := s.deps.ActivityFor(ctx, scope, agent.ID)
	if err != nil {
		return domain.Agent{}, err
	}

	agent.Display = domain.DeriveStatus(agent.Status, activity)
	if agent.Display == domain.DisplayWorking || agent.Display == domain.DisplayWaiting {
		reason, err := s.deps.ReasonFor(ctx, scope, agent.ID)
		if err != nil {
			return domain.Agent{}, err
		}
		agent.Reason = reason
	}

	return agent, nil
}

// buildNewAgent resolves the profile to insert.
func (s *service) buildNewAgent(ctx context.Context, scope domain.Scope, req domain.CreateRequest) (domain.NewAgent, error) {
	if req.TeamID == uuid.Nil {
		return domain.NewAgent{}, fmt.Errorf("%w: team is required", domain.ErrInvalidInput)
	}

	var input domain.NewAgent

	switch {
	case req.CopyFrom != uuid.Nil:
		source, err := s.deps.Get(ctx, scope, req.CopyFrom)
		if err != nil {
			return domain.NewAgent{}, err
		}
		input = domain.NewAgent{
			Name:         source.Name + " (salinan)",
			Role:         source.Role,
			Persona:      source.Persona,
			Tone:         source.Tone,
			Shape:        source.Shape,
			Color:        source.Color,
			Tools:        source.Tools,
			DefaultModel: source.DefaultModel,
		}

	case strings.TrimSpace(req.TemplateKey) != "":
		fromTemplate, err := s.deps.AgentTemplate(ctx, req.TemplateKey)
		if err != nil {
			return domain.NewAgent{}, err
		}
		input = fromTemplate
	}

	// Explicit request fields win over the copied profile.
	if name := strings.TrimSpace(req.Name); name != "" {
		input.Name = name
	}
	if input.Name == "" {
		return domain.NewAgent{}, fmt.Errorf("%w: name is required", domain.ErrInvalidInput)
	}
	if len([]rune(input.Name)) > NameMaxLength {
		return domain.NewAgent{}, fmt.Errorf("%w: name is longer than %d characters", domain.ErrInvalidInput, NameMaxLength)
	}
	if req.Role != "" {
		input.Role = strings.TrimSpace(req.Role)
	}
	if req.Persona != "" {
		input.Persona = strings.TrimSpace(req.Persona)
	}
	if req.Tone != "" {
		input.Tone = strings.TrimSpace(req.Tone)
	}
	if req.Shape != "" {
		input.Shape = strings.TrimSpace(req.Shape)
	}
	if req.Color != "" {
		input.Color = strings.TrimSpace(req.Color)
	}
	if req.Tools != nil {
		input.Tools = req.Tools
	}
	if input.Shape == "" {
		input.Shape = "circle"
	}

	if len([]rune(input.Persona)) > PersonaMaxLength {
		return domain.NewAgent{}, fmt.Errorf("%w: persona is longer than %d characters", domain.ErrInvalidInput, PersonaMaxLength)
	}

	input.TeamID = req.TeamID
	return input, nil
}

// checkLimit enforces the Bolu allowance of the plan. The plan wins when it
// reports a limit; otherwise the configured cap applies, and with neither the
// registry is unlimited.
func (s *service) checkLimit(ctx context.Context, scope domain.Scope) error {
	limit := s.cfg.MaxAgentsPerWorkspace
	if s.limits != nil {
		fromPlan, err := s.limits.MaxAgents(ctx, scope.WorkspaceID)
		if err != nil {
			return err
		}
		if fromPlan > 0 {
			limit = fromPlan
		}
	}
	if limit <= 0 {
		return nil
	}

	count, err := s.deps.CountLive(ctx, scope)
	if err != nil {
		return err
	}
	if count >= int64(limit) {
		return fmt.Errorf("%w (batas %d Bolu)", domain.ErrAgentLimitReached, limit)
	}

	return nil
}

func requireScope(scope domain.Scope) error {
	if scope.IsZero() || scope.WorkspaceID == uuid.Nil {
		return fmt.Errorf("%w: a workspace scope is required", domain.ErrInvalidInput)
	}
	return nil
}

var _ domain.Service = (*service)(nil)

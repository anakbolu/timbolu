// Package repository implements the agent registry data access with pgx and the
// sqlc-generated queries. Every method runs inside the caller's tenant scope, so
// Row Level Security filters the statements.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository/sqlcgen"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository {
	return &Repository{pool: pool}
}

// List returns every live Bolu of the workspace, with its derived status.
func (r *Repository) List(ctx context.Context, scope domain.Scope) ([]domain.Agent, error) {
	var agents []domain.Agent

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListAgents(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("agent: list: %w", err)
		}

		agents = make([]domain.Agent, 0, len(rows))
		for _, row := range rows {
			agent, err := toAgentFromList(row)
			if err != nil {
				return err
			}
			agents = append(agents, agent)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return agents, nil
}

// Get returns one live Bolu.
func (r *Repository) Get(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Agent, error) {
	var agent domain.Agent

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetAgent(ctx, sqlcgen.GetAgentParams{ID: id, WorkspaceID: scope.WorkspaceID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAgentNotFound
			}
			return fmt.Errorf("agent: get: %w", err)
		}

		agent, err = toAgentFromGet(row)
		return err
	})
	if err != nil {
		return domain.Agent{}, err
	}

	return agent, nil
}

// Create inserts a Bolu.
func (r *Repository) Create(ctx context.Context, scope domain.Scope, input domain.NewAgent) (domain.Agent, error) {
	tools, err := domain.MarshalTools(input.Tools)
	if err != nil {
		return domain.Agent{}, fmt.Errorf("agent: encode tools: %w", err)
	}
	model, err := marshalModel(input.DefaultModel)
	if err != nil {
		return domain.Agent{}, err
	}

	var created sqlcgen.Agent

	err = r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.CreateAgent(ctx, sqlcgen.CreateAgentParams{
			WorkspaceID:  scope.WorkspaceID,
			TeamID:       input.TeamID,
			Name:         input.Name,
			Role:         input.Role,
			Persona:      input.Persona,
			Tone:         input.Tone,
			Shape:        input.Shape,
			Color:        input.Color,
			Tools:        tools,
			TemplateKey:  input.TemplateKey,
			DefaultModel: model,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				// A foreign key failure here means the team is not in this
				// workspace, which RLS also refuses.
				return domain.ErrTeamNotFound
			}
			return fmt.Errorf("agent: create: %w", err)
		}
		created = row
		return nil
	})
	if err != nil {
		return domain.Agent{}, err
	}

	// Read the row back so the response carries the team name like every other
	// read path.
	return r.Get(ctx, scope, created.ID)
}

// Update edits a Bolu profile.
func (r *Repository) Update(ctx context.Context, scope domain.Scope, id uuid.UUID, req domain.UpdateRequest) (domain.Agent, error) {
	tools, err := domain.MarshalTools(req.Tools)
	if err != nil {
		return domain.Agent{}, fmt.Errorf("agent: encode tools: %w", err)
	}
	model, err := marshalModel(req.DefaultModel)
	if err != nil {
		return domain.Agent{}, err
	}

	err = r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.UpdateAgent(ctx, sqlcgen.UpdateAgentParams{
			ID:           id,
			WorkspaceID:  scope.WorkspaceID,
			Name:         req.Name,
			Role:         req.Role,
			Persona:      req.Persona,
			Tone:         req.Tone,
			Shape:        req.Shape,
			Color:        req.Color,
			Tools:        tools,
			DefaultModel: model,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAgentNotFound
			}
			return fmt.Errorf("agent: update: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Agent{}, err
	}

	return r.Get(ctx, scope, id)
}

// SetStatus flips the rest switch.
func (r *Repository) SetStatus(ctx context.Context, scope domain.Scope, id uuid.UUID, status string) (domain.Agent, error) {
	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		_, err := queries.SetAgentStatus(ctx, sqlcgen.SetAgentStatusParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
			Status:      status,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAgentNotFound
			}
			return fmt.Errorf("agent: set status: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Agent{}, err
	}

	return r.Get(ctx, scope, id)
}

// Delete marks a Bolu deleted. Its tasks, drafts, and messages stay.
func (r *Repository) Delete(ctx context.Context, scope domain.Scope, id uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		removed, err := queries.SoftDeleteAgent(ctx, sqlcgen.SoftDeleteAgentParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return fmt.Errorf("agent: delete: %w", err)
		}
		if removed == 0 {
			return domain.ErrAgentNotFound
		}
		return nil
	})
}

// Activity returns the status inputs for every live Bolu at once.
func (r *Repository) Activity(ctx context.Context, scope domain.Scope) (map[uuid.UUID]domain.Activity, error) {
	activity := map[uuid.UUID]domain.Activity{}

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.AgentActivity(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("agent: activity: %w", err)
		}
		for _, row := range rows {
			activity[row.ID] = domain.Activity{Working: row.Working, Waiting: row.Waiting}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return activity, nil
}

// ActivityFor returns the status inputs for one Bolu.
func (r *Repository) ActivityFor(ctx context.Context, scope domain.Scope, id uuid.UUID) (domain.Activity, error) {
	var activity domain.Activity

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.AgentActivityByID(ctx, sqlcgen.AgentActivityByIDParams{
			ID:          id,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAgentNotFound
			}
			return fmt.Errorf("agent: activity for %s: %w", id, err)
		}
		activity = domain.Activity{Working: row.Working, Waiting: row.Waiting}
		return nil
	})
	if err != nil {
		return domain.Activity{}, err
	}

	return activity, nil
}

// ReasonFor explains a working or waiting status.
func (r *Repository) ReasonFor(ctx context.Context, scope domain.Scope, id uuid.UUID) (*domain.Reason, error) {
	var reason *domain.Reason

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		task, err := queries.LatestTaskForAgent(ctx, id)
		switch {
		case err == nil:
			reason = &domain.Reason{
				Kind:      "task",
				ID:        task.ID,
				Title:     task.Title,
				Status:    task.Status,
				CreatedAt: task.CreatedAt,
			}
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			// Fall through to the draft.
		default:
			return fmt.Errorf("agent: latest task: %w", err)
		}

		draft, err := queries.LatestPendingDraftForAgent(ctx, id)
		switch {
		case err == nil:
			reason = &domain.Reason{
				Kind:      "draft",
				ID:        draft.ID,
				Title:     draft.ActionKind,
				Status:    "pending",
				CreatedAt: draft.CreatedAt,
			}
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		default:
			return fmt.Errorf("agent: latest draft: %w", err)
		}
	})
	if err != nil {
		return nil, err
	}

	return reason, nil
}

// RunningTasks counts the tasks in flight for one Bolu.
func (r *Repository) RunningTasks(ctx context.Context, scope domain.Scope, id uuid.UUID) (int64, error) {
	var count int64

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		running, err := queries.CountRunningTasksForAgent(ctx, id)
		if err != nil {
			return fmt.Errorf("agent: count running tasks: %w", err)
		}
		count = running
		return nil
	})
	if err != nil {
		return 0, err
	}

	return count, nil
}

// CountLive counts the live Bolu of the workspace, which the plan limit uses.
func (r *Repository) CountLive(ctx context.Context, scope domain.Scope) (int64, error) {
	var count int64

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		total, err := queries.CountLiveAgents(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("agent: count live: %w", err)
		}
		count = total
		return nil
	})
	if err != nil {
		return 0, err
	}

	return count, nil
}

// Teams lists the teams of the workspace with their Bolu.
func (r *Repository) Teams(ctx context.Context, scope domain.Scope) ([]domain.Team, error) {
	agents, err := r.List(ctx, scope)
	if err != nil {
		return nil, err
	}

	var teams []domain.Team

	err = r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListWorkspaceTeams(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("agent: list teams: %w", err)
		}

		byTeam := map[uuid.UUID][]domain.Agent{}
		for _, agent := range agents {
			byTeam[agent.TeamID] = append(byTeam[agent.TeamID], agent)
		}

		teams = make([]domain.Team, 0, len(rows))
		for _, row := range rows {
			teams = append(teams, domain.Team{
				ID:     row.ID,
				Name:   row.Name,
				Kind:   row.Kind,
				Agents: byTeam[row.ID],
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return teams, nil
}

// Templates lists the seeded profiles, in their display order.
func (r *Repository) Templates(ctx context.Context) ([]domain.Template, error) {
	var templates []domain.Template

	err := r.readPublic(ctx, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListAgentTemplates(ctx)
		if err != nil {
			return fmt.Errorf("agent: list templates: %w", err)
		}

		templates = make([]domain.Template, 0, len(rows))
		for _, row := range rows {
			tools, err := domain.UnmarshalTools(row.Tools)
			if err != nil {
				return fmt.Errorf("agent: decode template tools: %w", err)
			}
			var integrations []string
			if err := json.Unmarshal(row.Integrations, &integrations); err != nil {
				return fmt.Errorf("agent: decode template integrations: %w", err)
			}

			templates = append(templates, domain.Template{
				Key:          row.Key,
				Name:         row.Name,
				Role:         row.Role,
				Persona:      row.Persona,
				Tone:         row.Tone,
				Shape:        row.Shape,
				Color:        row.Color,
				Tools:        tools,
				Integrations: integrations,
				SortOrder:    row.SortOrder,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return templates, nil
}

// Template reads one seeded template.
func (r *Repository) Template(ctx context.Context, key string) (domain.Template, error) {
	var template domain.Template

	err := r.readPublic(ctx, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetAgentTemplate(ctx, key)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrAgentNotFound
			}
			return fmt.Errorf("agent: get template: %w", err)
		}

		tools, err := domain.UnmarshalTools(row.Tools)
		if err != nil {
			return fmt.Errorf("agent: decode template tools: %w", err)
		}
		var integrations []string
		if err := json.Unmarshal(row.Integrations, &integrations); err != nil {
			return fmt.Errorf("agent: decode template integrations: %w", err)
		}

		template = domain.Template{
			Key:          row.Key,
			Name:         row.Name,
			Role:         row.Role,
			Persona:      row.Persona,
			Tone:         row.Tone,
			Shape:        row.Shape,
			Color:        row.Color,
			Tools:        tools,
			Integrations: integrations,
			SortOrder:    row.SortOrder,
		}
		return nil
	})
	if err != nil {
		return domain.Template{}, err
	}

	return template, nil
}

// AgentTemplate converts a seeded template into a new agent row.
func (r *Repository) AgentTemplate(ctx context.Context, key string) (domain.NewAgent, error) {
	template, err := r.Template(ctx, key)
	if err != nil {
		return domain.NewAgent{}, err
	}

	return domain.NewAgent{
		Name:        template.Name,
		Role:        template.Role,
		Persona:     template.Persona,
		Tone:        template.Tone,
		Shape:       template.Shape,
		Color:       template.Color,
		Tools:       template.Tools,
		TemplateKey: template.Key,
	}, nil
}

// Grants lists the grants of one Bolu.
func (r *Repository) Grants(ctx context.Context, scope domain.Scope, agentID uuid.UUID) ([]domain.Grant, error) {
	var grants []domain.Grant

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListGrants(ctx, sqlcgen.ListGrantsParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     agentID,
		})
		if err != nil {
			return fmt.Errorf("agent: list grants: %w", err)
		}

		grants = make([]domain.Grant, 0, len(rows))
		for _, row := range rows {
			grants = append(grants, domain.Grant{
				ID:            row.ID,
				AgentID:       row.AgentID,
				IntegrationID: row.IntegrationID,
				App:           row.IntegrationApp,
				AccountLabel:  row.AccountLabel,
				Status:        row.IntegrationStatus,
				Permission:    row.Permission,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return grants, nil
}

// SetGrant creates or updates a grant.
func (r *Repository) SetGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID, permission string) (domain.Grant, error) {
	var grant domain.Grant

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.UpsertGrant(ctx, sqlcgen.UpsertGrantParams{
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       agentID,
			IntegrationID: integrationID,
			Permission:    permission,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return domain.ErrIntegrationNotFound
			}
			return fmt.Errorf("agent: set grant: %w", err)
		}

		app, label, status, err := r.integration(ctx, queries, scope, integrationID)
		if err != nil {
			return err
		}

		grant = domain.Grant{
			ID:            row.ID,
			AgentID:       row.AgentID,
			IntegrationID: row.IntegrationID,
			App:           app,
			AccountLabel:  label,
			Status:        status,
			Permission:    row.Permission,
		}
		return nil
	})
	if err != nil {
		return domain.Grant{}, err
	}

	return grant, nil
}

// DeleteGrant revokes a grant, which immediately removes the matching tools.
func (r *Repository) DeleteGrant(ctx context.Context, scope domain.Scope, agentID, integrationID uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		removed, err := queries.DeleteGrant(ctx, sqlcgen.DeleteGrantParams{
			WorkspaceID:   scope.WorkspaceID,
			AgentID:       agentID,
			IntegrationID: integrationID,
		})
		if err != nil {
			return fmt.Errorf("agent: delete grant: %w", err)
		}
		if removed == 0 {
			return domain.ErrIntegrationNotFound
		}
		return nil
	})
}

// AllowedTools is the effective tool list: the agent's tools, restricted to the
// integrations it was granted.
func (r *Repository) AllowedTools(ctx context.Context, scope domain.Scope, agentID uuid.UUID) ([]domain.Tool, error) {
	var tools []domain.Tool

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.AllowedToolsForAgent(ctx, sqlcgen.AllowedToolsForAgentParams{
			ID:          agentID,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return fmt.Errorf("agent: allowed tools: %w", err)
		}

		tools = make([]domain.Tool, 0, len(rows))
		for _, row := range rows {
			tools = append(tools, domain.Tool{
				Name:        row.Name,
				Integration: row.IntegrationApp,
				Label:       row.Label,
				Description: row.Description,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return tools, nil
}

// ToolCatalog lists every known tool.
func (r *Repository) ToolCatalog(ctx context.Context) ([]domain.Tool, error) {
	var tools []domain.Tool

	err := r.readPublic(ctx, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListToolCatalog(ctx)
		if err != nil {
			return fmt.Errorf("agent: tool catalog: %w", err)
		}

		tools = make([]domain.Tool, 0, len(rows))
		for _, row := range rows {
			tools = append(tools, domain.Tool{
				Name:        row.Name,
				Integration: row.IntegrationApp,
				Label:       row.Label,
				Description: row.Description,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return tools, nil
}

// Integrations lists the connected applications of the workspace.
func (r *Repository) Integrations(ctx context.Context, scope domain.Scope) ([]domain.Integration, error) {
	var integrations []domain.Integration

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListIntegrations(ctx, scope.WorkspaceID)
		if err != nil {
			return fmt.Errorf("agent: list integrations: %w", err)
		}

		integrations = make([]domain.Integration, 0, len(rows))
		for _, row := range rows {
			integrations = append(integrations, domain.Integration{
				ID:           row.ID,
				App:          row.App,
				AccountLabel: row.AccountLabel,
				Status:       row.Status,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return integrations, nil
}

// Provision copies every template into the workspace. It satisfies
// database.TemplateProvisioner and runs inside the onboarding transaction.
func (r *Repository) integration(ctx context.Context, queries *sqlcgen.Queries, scope domain.Scope, integrationID uuid.UUID) (string, string, string, error) {
	rows, err := queries.ListIntegrations(ctx, scope.WorkspaceID)
	if err != nil {
		return "", "", "", fmt.Errorf("agent: read integration: %w", err)
	}
	for _, row := range rows {
		if row.ID == integrationID {
			return row.App, row.AccountLabel, row.Status, nil
		}
	}
	return "", "", "", domain.ErrIntegrationNotFound
}

func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("agent: database is not configured")
	}
	return r.pool.InScopeRead(ctx, toDatabaseScope(scope), func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("agent: database is not configured")
	}
	return r.pool.InScope(ctx, toDatabaseScope(scope), func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

// readPublic runs a query that is not tenant scoped (the seeded templates and
// the tool catalogue are shared by every workspace). It still needs a
// transaction because the sqlc queries take a pgx handle.
func (r *Repository) readPublic(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("agent: database is not configured")
	}
	return r.pool.InScopeRead(ctx, database.Scope{UserID: uuid.New()}, func(tx pgx.Tx) error {
		return fn(sqlcgen.New(tx))
	})
}

func toDatabaseScope(scope domain.Scope) database.Scope {
	return database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID}
}

// agentRow is the shape both joined queries return. sqlc generates a separate
// struct per query, so the two are flattened into this one before conversion.
type agentRow struct {
	ID           uuid.UUID
	WorkspaceID  uuid.UUID
	TeamID       uuid.UUID
	Name         string
	Role         string
	Persona      string
	Tone         string
	Shape        string
	Color        string
	Status       string
	TemplateKey  string
	DefaultModel []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Tools        []byte
	TeamName     string
	TeamKind     string
}

func toAgentFromList(row sqlcgen.ListAgentsRow) (domain.Agent, error) {
	return toAgent(agentRow{
		ID: row.ID, WorkspaceID: row.WorkspaceID, TeamID: row.TeamID,
		Name: row.Name, Role: row.Role, Persona: row.Persona, Tone: row.Tone,
		Shape: row.Shape, Color: row.Color, Status: row.Status,
		TemplateKey: row.TemplateKey, DefaultModel: row.DefaultModel,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Tools: row.Tools,
		TeamName: row.TeamName, TeamKind: row.TeamKind,
	})
}

func toAgentFromGet(row sqlcgen.GetAgentRow) (domain.Agent, error) {
	return toAgent(agentRow{
		ID: row.ID, WorkspaceID: row.WorkspaceID, TeamID: row.TeamID,
		Name: row.Name, Role: row.Role, Persona: row.Persona, Tone: row.Tone,
		Shape: row.Shape, Color: row.Color, Status: row.Status,
		TemplateKey: row.TemplateKey, DefaultModel: row.DefaultModel,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Tools: row.Tools,
		TeamName: row.TeamName, TeamKind: row.TeamKind,
	})
}

func toAgent(row agentRow) (domain.Agent, error) {
	tools, err := domain.UnmarshalTools(row.Tools)
	if err != nil {
		return domain.Agent{}, fmt.Errorf("agent: decode tools: %w", err)
	}
	model, err := unmarshalModel(row.DefaultModel)
	if err != nil {
		return domain.Agent{}, err
	}

	return domain.Agent{
		ID:           row.ID,
		WorkspaceID:  row.WorkspaceID,
		TeamID:       row.TeamID,
		TeamName:     row.TeamName,
		TeamKind:     row.TeamKind,
		Name:         row.Name,
		Role:         row.Role,
		Persona:      row.Persona,
		Tone:         row.Tone,
		Shape:        row.Shape,
		Color:        row.Color,
		Status:       row.Status,
		TemplateKey:  row.TemplateKey,
		Tools:        tools,
		DefaultModel: model,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}, nil
}

func marshalModel(model map[string]any) ([]byte, error) {
	if model == nil {
		model = map[string]any{}
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("agent: encode default model: %w", err)
	}
	return raw, nil
}

func unmarshalModel(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var model map[string]any
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, fmt.Errorf("agent: decode default model: %w", err)
	}
	if model == nil {
		return map[string]any{}, nil
	}
	return model, nil
}

// Compile-time check: the repository is the registry store.
var _ domain.Repository = (*Repository)(nil)

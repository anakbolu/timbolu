// Package repository implements the checklist module's data access with pgx and the sqlc-generated
// queries.
//
// Every method runs inside the caller's tenant scope, so Row Level Security filters the statements even
// where one forgets its workspace filter. Two things are the database's rather than this file's, and
// the difference matters when reading what follows: `checklists_agent_active_key` refuses a second
// open plan for one Bolu, and the trigger on `checklist_items` refuses a task ticked while its
// subtasks are open. This file does not re-check either — it translates the refusal into the module's
// own error and lets the constraint be the one that holds.
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/repository/sqlcgen"
	"github.com/robzlabz/employeebot/packages/platform/database"
)

// Repository is the Postgres-backed implementation of domain.Repository.
type Repository struct {
	pool *database.Pool
}

// New builds the repository.
func New(pool *database.Pool) *Repository { return &Repository{pool: pool} }

// Compile-time check that the repository is the interface the module asked for.
var _ domain.Repository = (*Repository)(nil)

// read runs one query in the caller's scope, read-only.
func (r *Repository) read(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return r.pool.InScopeRead(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID},
		func(tx pgx.Tx) error { return fn(sqlcgen.New(tx)) })
}

// write runs one query in the caller's scope.
func (r *Repository) write(ctx context.Context, scope domain.Scope, fn func(*sqlcgen.Queries) error) error {
	if r == nil || r.pool == nil {
		return fmt.Errorf("%w: database is not configured", domain.ErrInvalidInput)
	}
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return r.pool.InScope(ctx, database.Scope{UserID: scope.UserID, WorkspaceID: scope.WorkspaceID},
		func(tx pgx.Tx) error { return fn(sqlcgen.New(tx)) })
}

// ---------------------------------------------------------------- the list

// Active returns the Bolu's open plan.
func (r *Repository) Active(ctx context.Context, scope domain.Scope, agentID uuid.UUID) (domain.Checklist, error) {
	var plan domain.Checklist

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		row, err := queries.GetActiveChecklist(ctx, sqlcgen.GetActiveChecklistParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     agentID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: Bolu %s has no plan in progress", domain.ErrNotFound, agentID)
			}

			return fmt.Errorf("checklist: get active: %w", err)
		}

		plan, err = load(ctx, queries, scope.WorkspaceID, row)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return domain.Checklist{}, err
	}

	return plan, nil
}

// Create writes a plan and its items in one transaction.
//
// The items are inserted in the order they were written and carry that order as their position, so a
// plan read back is the plan that was meant rather than the order a database happened to return.
func (r *Repository) Create(ctx context.Context, scope domain.Scope, input domain.NewChecklist) (domain.Checklist, error) {
	var stored domain.Checklist

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		header, err := queries.CreateChecklist(ctx, sqlcgen.CreateChecklistParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     input.AgentID,
			Title:       input.Title,
			TaskID:      optionalID(input.TaskID),
		})
		if err != nil {
			return translateWrite(err, "create checklist")
		}

		for index, task := range input.Tasks {
			parent, err := queries.CreateChecklistItem(ctx, sqlcgen.CreateChecklistItemParams{
				ChecklistID: header.ID,
				WorkspaceID: scope.WorkspaceID,
				Title:       task.Title,
				Position:    int32(index),
			})
			if err != nil {
				return translateWrite(err, "create checklist item")
			}

			for subIndex, subtask := range task.Subtasks {
				if _, err := queries.CreateChecklistItem(ctx, sqlcgen.CreateChecklistItemParams{
					ChecklistID: header.ID,
					WorkspaceID: scope.WorkspaceID,
					ParentID:    optionalID(parent.ID),
					Title:       subtask,
					Position:    int32(subIndex),
				}); err != nil {
					return translateWrite(err, "create checklist subtask")
				}
			}
		}

		stored, err = load(ctx, queries, scope.WorkspaceID, header)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return domain.Checklist{}, err
	}

	return stored, nil
}

// MarkDone ticks one item and settles the plan if that was the last thing open.
//
// The tick, the count, and the settle are one transaction, because the interesting failure is exactly
// the one a split would allow: a plan whose last task is ticked but which is left open, so the Bolu
// cannot start the next one.
func (r *Repository) MarkDone(ctx context.Context, scope domain.Scope, checklistID, itemID uuid.UUID) (domain.Checklist, error) {
	return r.change(ctx, scope, checklistID, func(queries *sqlcgen.Queries) error {
		if _, err := queries.MarkChecklistItemDone(ctx, sqlcgen.MarkChecklistItemDoneParams{
			ID:          itemID,
			ChecklistID: checklistID,
			WorkspaceID: scope.WorkspaceID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: item %s", domain.ErrNotFound, itemID)
			}

			return translateWrite(err, "mark item done")
		}

		return nil
	})
}

// RemoveItem deletes one item, and with it the subtasks underneath it, and settles the plan the same
// way a tick does.
//
// Removing the last open task finishes the plan rather than leaving an empty one open. The
// alternative is a Bolu holding a plan with nothing in it and no way to start another, which is a
// worse answer than "the plan is over" — the model removed the work because it decided it was not to
// be done, and that is an ending.
func (r *Repository) RemoveItem(ctx context.Context, scope domain.Scope, checklistID, itemID uuid.UUID) (domain.Checklist, error) {
	return r.change(ctx, scope, checklistID, func(queries *sqlcgen.Queries) error {
		removed, err := queries.DeleteChecklistItem(ctx, sqlcgen.DeleteChecklistItemParams{
			ID:          itemID,
			ChecklistID: checklistID,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return translateWrite(err, "delete checklist item")
		}
		if removed == 0 {
			return fmt.Errorf("%w: item %s", domain.ErrNotFound, itemID)
		}

		return nil
	})
}

// change runs one write, settles the plan if nothing is left open, and answers with the plan as it now
// is. Both of the operations that can finish a plan go through it, so the rule is written once.
func (r *Repository) change(
	ctx context.Context,
	scope domain.Scope,
	checklistID uuid.UUID,
	apply func(*sqlcgen.Queries) error,
) (domain.Checklist, error) {
	var stored domain.Checklist

	err := r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		if err := apply(queries); err != nil {
			return err
		}

		open, err := queries.CountOpenTasks(ctx, sqlcgen.CountOpenTasksParams{
			ChecklistID: checklistID,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return fmt.Errorf("checklist: count open tasks: %w", err)
		}

		// Nothing left open settles the plan; anything else reads it as it stands. The two branches
		// are exclusive because settling is a write: running it and then reading the row back would
		// answer "done" for a plan that still has work in it.
		var row sqlcgen.Checklist
		if open == 0 {
			row, err = queries.CompleteChecklist(ctx, sqlcgen.CompleteChecklistParams{
				ID:          checklistID,
				WorkspaceID: scope.WorkspaceID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: checklist %s", domain.ErrNotFound, checklistID)
				}

				return translateWrite(err, "complete checklist")
			}
		} else {
			row, err = queries.GetChecklist(ctx, sqlcgen.GetChecklistParams{
				ID:          checklistID,
				WorkspaceID: scope.WorkspaceID,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("%w: checklist %s", domain.ErrNotFound, checklistID)
				}

				return fmt.Errorf("checklist: get: %w", err)
			}
		}

		stored, err = load(ctx, queries, scope.WorkspaceID, row)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return domain.Checklist{}, err
	}

	return stored, nil
}

// Clear deletes a plan and everything in it.
func (r *Repository) Clear(ctx context.Context, scope domain.Scope, checklistID uuid.UUID) error {
	return r.write(ctx, scope, func(queries *sqlcgen.Queries) error {
		deleted, err := queries.DeleteChecklist(ctx, sqlcgen.DeleteChecklistParams{
			ID:          checklistID,
			WorkspaceID: scope.WorkspaceID,
		})
		if err != nil {
			return translateWrite(err, "delete checklist")
		}
		if deleted == 0 {
			return fmt.Errorf("%w: checklist %s", domain.ErrNotFound, checklistID)
		}

		return nil
	})
}

// List returns plans, newest first, each with its items.
func (r *Repository) List(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Checklist, error) {
	filter = filter.Normalised()

	var plans []domain.Checklist

	err := r.read(ctx, scope, func(queries *sqlcgen.Queries) error {
		rows, err := queries.ListChecklists(ctx, sqlcgen.ListChecklistsParams{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     optionalID(filter.AgentID),
			Status:      filter.Status,
			LimitCount:  int32(filter.Limit),
		})
		if err != nil {
			return fmt.Errorf("checklist: list: %w", err)
		}

		if len(rows) == 0 {
			plans = []domain.Checklist{}

			return nil
		}

		ids := make([]uuid.UUID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}

		// The items of the whole page in one query: a screen that read them one plan at a time would
		// spend a round trip per row to render a list.
		items, err := queries.ListItemsForChecklists(ctx, sqlcgen.ListItemsForChecklistsParams{
			WorkspaceID:  scope.WorkspaceID,
			ChecklistIds: ids,
		})
		if err != nil {
			return fmt.Errorf("checklist: list items: %w", err)
		}

		grouped := group(items)
		plans = make([]domain.Checklist, 0, len(rows))
		for _, row := range rows {
			plans = append(plans, toChecklist(row, grouped[row.ID]))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return plans, nil
}

// ---------------------------------------------------------------- loading

// load reads one plan and its items, assembling the tree the domain talks about.
func load(ctx context.Context, queries *sqlcgen.Queries, workspaceID uuid.UUID, row sqlcgen.Checklist) (domain.Checklist, error) {
	items, err := queries.ListChecklistItems(ctx, sqlcgen.ListChecklistItemsParams{
		ChecklistID: row.ID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		return domain.Checklist{}, fmt.Errorf("checklist: list items: %w", err)
	}

	return toChecklist(row, items), nil
}

// group splits items by the plan they belong to, which is what a list read needs.
func group(items []sqlcgen.ChecklistItem) map[uuid.UUID][]sqlcgen.ChecklistItem {
	grouped := make(map[uuid.UUID][]sqlcgen.ChecklistItem, len(items))
	for _, item := range items {
		grouped[item.ChecklistID] = append(grouped[item.ChecklistID], item)
	}

	return grouped
}

// toChecklist maps a row and its items onto the domain type, nesting subtasks under their task.
//
// An item whose parent is not in this slice is kept as a task rather than dropped: the rows are
// written together and read together, so a subtask without its parent is a state that cannot arise —
// and silently hiding an item would be the worse way to be wrong if it ever did.
func toChecklist(row sqlcgen.Checklist, items []sqlcgen.ChecklistItem) domain.Checklist {
	plan := domain.Checklist{
		ID:          row.ID,
		AgentID:     row.AgentID,
		Title:       row.Title,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		CompletedAt: row.CompletedAt,
		Items:       make([]domain.Item, 0, len(items)),
	}
	if row.TaskID != nil {
		plan.TaskID = *row.TaskID
	}

	// Two passes: the tasks first, then the subtasks attached to them. One pass would depend on the
	// rows arriving parents-first, which is true today and would be a silent rule to keep true.
	index := make(map[uuid.UUID]int, len(items))
	for _, item := range items {
		if item.ParentID != nil {
			continue
		}

		index[item.ID] = len(plan.Items)
		plan.Items = append(plan.Items, toItem(item))
	}

	for _, item := range items {
		if item.ParentID == nil {
			continue
		}

		position, found := index[*item.ParentID]
		if !found {
			plan.Items = append(plan.Items, toItem(item))

			continue
		}

		plan.Items[position].Subtasks = append(plan.Items[position].Subtasks, toItem(item))
	}

	return plan
}

// toItem maps one row.
func toItem(row sqlcgen.ChecklistItem) domain.Item {
	item := domain.Item{
		ID:          row.ID,
		ChecklistID: row.ChecklistID,
		Title:       row.Title,
		Done:        row.Done,
		Position:    int(row.Position),
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		DoneAt:      row.DoneAt,
	}
	if row.ParentID != nil {
		item.ParentID = *row.ParentID
	}

	return item
}

// optionalID renders a uuid that may be absent as NULL rather than as the zero uuid, which the columns
// would reject or, worse, accept as a real reference.
func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}

	return &id
}

// translateWrite turns a driver error into this module's own.
//
// The two constraint names are the module's two rules, and they are named rather than inferred: a
// unique violation on `checklists_agent_active_key` is "you already have a plan", and a check
// violation from the items trigger is "that task still has subtasks open". Both are refused by the
// service before they get here; this is what keeps the answer the same if one ever is not.
func translateWrite(err error, operation string) error {
	if err == nil {
		return nil
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			if pgErr.ConstraintName == "checklists_agent_active_key" {
				return fmt.Errorf("%w: %s", domain.ErrActiveExists, pgErr.Detail)
			}

			return fmt.Errorf("%w: %s", domain.ErrInvalidInput, pgErr.Detail)
		case "23514": // check_violation
			if pgErr.Message == "checklist: a task with unfinished subtasks cannot be marked done" {
				return fmt.Errorf("%w: %s", domain.ErrSubtasksOpen, pgErr.Message)
			}

			return fmt.Errorf("%w: %s", domain.ErrInvalidInput, pgErr.Message)
		case "23503", "22P02", "22003": // foreign key, cast, out of range
			return fmt.Errorf("%w: %s", domain.ErrInvalidInput, pgErr.Message)
		}
	}

	return fmt.Errorf("checklist: %s: %w", operation, err)
}

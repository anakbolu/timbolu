// Package service implements the checklist module's use cases: the plan a Bolu writes for itself, the
// ticks it makes, and the two ways it throws one away.
//
// It depends only on the module's domain contracts, so the rules can be tested without a database —
// which matters here more than elsewhere, because the rules are the product: one plan at a time, a
// task is not done while its subtasks are open, and a plan is over when nothing is left open.
//
// The refusals are written for the reader they have, which is a model in the middle of a job. Each one
// says what was asked for and what is in the way, because a tool result is the whole of what the model
// gets: "you already have a plan" without the list is a message a model can only answer by guessing.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain"
)

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Repository
	Logger     *zap.Logger
}

// Service is the checklist module's implementation.
type Service struct {
	deps domain.Repository
	log  *zap.Logger
}

// New builds the service.
func New(deps Deps) *Service {
	logger := deps.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	return &Service{deps: deps.Repository, log: logger}
}

// Compile-time check that the service is the interface it declares.
var _ domain.Service = (*Service)(nil)

// Read returns the Bolu's open plan.
//
// ErrNotFound is the honest answer for a Bolu that has not written one, and the tool turns it into an
// empty list rather than an error: "what is my plan" has the answer "you have not made one", which is
// not a failure.
func (s *Service) Read(ctx context.Context, scope domain.Scope, agentID uuid.UUID) (domain.Checklist, error) {
	if err := requireScope(scope); err != nil {
		return domain.Checklist{}, err
	}
	if agentID == uuid.Nil {
		return domain.Checklist{}, fmt.Errorf("%w: Bolu tidak dikenal", domain.ErrInvalidInput)
	}

	return s.deps.Active(ctx, scope, agentID)
}

// Create writes the Bolu's next plan.
//
// It is refused while one is still open, and the refusal carries what is still open rather than only
// the fact of the refusal. That is the whole point of the rule: a model that is told "you still have a
// plan" and not what is in it has nothing to do next except ask a person, and the plan it wrote is
// the one thing that could have told it.
func (s *Service) Create(ctx context.Context, scope domain.Scope, plan domain.NewChecklist) (domain.Checklist, error) {
	if err := requireScope(scope); err != nil {
		return domain.Checklist{}, err
	}
	if plan.AgentID == uuid.Nil {
		return domain.Checklist{}, fmt.Errorf("%w: Bolu tidak dikenal", domain.ErrInvalidInput)
	}

	title, err := planTitle(plan.Title)
	if err != nil {
		return domain.Checklist{}, err
	}

	tasks, err := planTasks(plan.Tasks)
	if err != nil {
		return domain.Checklist{}, err
	}

	// The read is what turns the database's constraint into a sentence. The constraint still holds —
	// it is what makes the rule true under two writers — but the model gets the list instead of a
	// unique-violation.
	current, err := s.deps.Active(ctx, scope, plan.AgentID)
	switch {
	case err == nil:
		return domain.Checklist{}, fmt.Errorf("%w: %s", domain.ErrActiveExists, openWork(current))
	case errors.Is(err, domain.ErrNotFound):
		// Nothing open: the next plan may be written.
	default:
		return domain.Checklist{}, err
	}

	plan.Title = title
	plan.Tasks = tasks

	return s.deps.Create(ctx, scope, plan)
}

// Check ticks one item of the open plan.
//
// Three cases, and each answers with the plan rather than with an acknowledgement:
//
//   - **An item that is already ticked** is not an error. A model that ticks the same thing twice
//     meant the same thing both times, and refusing it would only cost a round.
//   - **A task with subtasks still open** is refused, and the refusal names them: the model has to
//     know which line it forgot, and the alternative — ticking the parent and leaving the subtasks —
//     is a plan that reads as finished while work is left in it.
//   - **The last open task** settles the plan, which is the answer the model needs: it is free to
//     write the next one.
func (s *Service) Check(ctx context.Context, scope domain.Scope, agentID, itemID uuid.UUID) (domain.Checklist, error) {
	plan, item, err := s.locate(ctx, scope, agentID, itemID)
	if err != nil {
		return domain.Checklist{}, err
	}
	if item.Done {
		return plan, nil
	}
	if open := item.OpenSubtasks(); len(open) > 0 {
		return domain.Checklist{}, fmt.Errorf("%w: %s", domain.ErrSubtasksOpen, openSubtasks(open))
	}

	return s.deps.MarkDone(ctx, scope, plan.ID, itemID)
}

// Remove deletes one item of the open plan, subtasks and all.
func (s *Service) Remove(ctx context.Context, scope domain.Scope, agentID, itemID uuid.UUID) (domain.Checklist, error) {
	plan, _, err := s.locate(ctx, scope, agentID, itemID)
	if err != nil {
		return domain.Checklist{}, err
	}

	return s.deps.RemoveItem(ctx, scope, plan.ID, itemID)
}

// Clear throws the open plan away, which is what frees the Bolu to write another.
//
// It is the escape hatch the "one plan at a time" rule needs: a model that wrote a plan the job turned
// out not to need would otherwise be holding it until every line of it was ticked, which is a rule
// that punishes writing anything down.
func (s *Service) Clear(ctx context.Context, scope domain.Scope, agentID uuid.UUID) error {
	plan, err := s.Read(ctx, scope, agentID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("%w: tidak ada rencana aktif untuk dibuang", domain.ErrNotFound)
		}

		return err
	}

	return s.deps.Clear(ctx, scope, plan.ID)
}

// List returns plans for a screen.
func (s *Service) List(ctx context.Context, scope domain.Scope, filter domain.Filter) ([]domain.Checklist, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	switch filter.Status {
	case "", domain.StatusActive, domain.StatusDone:
	default:
		return nil, fmt.Errorf("%w: status %q tidak dikenal", domain.ErrInvalidInput, filter.Status)
	}

	return s.deps.List(ctx, scope, filter)
}

// locate finds one item inside the Bolu's open plan.
//
// Both of the writes that name an item go through it, so "which plan is this item in" and "is the
// Bolu's plan open at all" are answered once. A tick against a plan that has already finished is
// reported as "no plan in progress" rather than as "no such item", because the second would send the
// model looking for a typo in an id that was never wrong.
func (s *Service) locate(ctx context.Context, scope domain.Scope, agentID, itemID uuid.UUID) (domain.Checklist, domain.Item, error) {
	if err := requireScope(scope); err != nil {
		return domain.Checklist{}, domain.Item{}, err
	}
	if agentID == uuid.Nil {
		return domain.Checklist{}, domain.Item{}, fmt.Errorf("%w: Bolu tidak dikenal", domain.ErrInvalidInput)
	}
	if itemID == uuid.Nil {
		return domain.Checklist{}, domain.Item{}, fmt.Errorf("%w: tugasnya belum disebut", domain.ErrInvalidInput)
	}

	plan, err := s.deps.Active(ctx, scope, agentID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Rewritten for the model, which has to know what to do about it: the repository's own
			// message names the Bolu, and what the caller needs is the tool that writes a plan.
			return domain.Checklist{}, domain.Item{}, fmt.Errorf(
				"%w: tidak ada rencana aktif; tulis rencananya dulu dengan checklist.create", domain.ErrNotFound)
		}

		return domain.Checklist{}, domain.Item{}, err
	}

	item, found := plan.Find(itemID)
	if !found {
		return domain.Checklist{}, domain.Item{}, fmt.Errorf(
			"%w: tugas %s tidak ada di rencana yang sedang aktif", domain.ErrNotFound, itemID)
	}

	return plan, item, nil
}

// ---------------------------------------------------------------- validation

// requireScope refuses a call that names no workspace, which would otherwise be answered by Row Level
// Security as an empty result and read as "you have no plan".
func requireScope(scope domain.Scope) error {
	if scope.IsZero() {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return nil
}

// planTitle normalises the plan's own title, which is optional: a plan can be a list without a name.
func planTitle(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	if len([]rune(trimmed)) > domain.MaxTitleRunes {
		return "", fmt.Errorf("%w: judul rencana lebih dari %d karakter", domain.ErrInvalidInput, domain.MaxTitleRunes)
	}

	return trimmed, nil
}

// planTasks validates the tasks and their subtasks, and trims every title.
//
// The bounds are the product's: a plan is a list a person reads, and a model given no ceiling writes a
// hundred lines and calls it thorough.
func planTasks(tasks []domain.NewItem) ([]domain.NewItem, error) {
	if len(tasks) == 0 {
		return nil, fmt.Errorf("%w: rencananya belum berisi tugas apa pun", domain.ErrInvalidInput)
	}
	if len(tasks) > domain.MaxTasks {
		return nil, fmt.Errorf(
			"%w: satu rencana paling banyak %d tugas (diminta %d)", domain.ErrInvalidInput, domain.MaxTasks, len(tasks))
	}

	cleaned := make([]domain.NewItem, 0, len(tasks))
	for index, task := range tasks {
		title, err := taskTitle(task.Title, index+1)
		if err != nil {
			return nil, err
		}
		if len(task.Subtasks) > domain.MaxSubtasks {
			return nil, fmt.Errorf(
				"%w: tugas %q punya lebih dari %d sub-tugas", domain.ErrInvalidInput, title, domain.MaxSubtasks)
		}

		subtasks := make([]string, 0, len(task.Subtasks))
		for subIndex, subtask := range task.Subtasks {
			cleanedSubtask, err := subtaskTitle(subtask, title, subIndex+1)
			if err != nil {
				return nil, err
			}

			subtasks = append(subtasks, cleanedSubtask)
		}

		cleaned = append(cleaned, domain.NewItem{Title: title, Subtasks: subtasks})
	}

	return cleaned, nil
}

// taskTitle validates one task's title, naming its place in the list so a model that sent twenty
// tasks knows which one to fix.
func taskTitle(title string, position int) (string, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", fmt.Errorf("%w: tugas ke-%d belum punya judul", domain.ErrInvalidInput, position)
	}
	if len([]rune(trimmed)) > domain.MaxTitleRunes {
		return "", fmt.Errorf(
			"%w: judul tugas ke-%d lebih dari %d karakter", domain.ErrInvalidInput, position, domain.MaxTitleRunes)
	}

	return trimmed, nil
}

// subtaskTitle validates one subtask's title against its task.
func subtaskTitle(title, task string, position int) (string, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", fmt.Errorf("%w: sub-tugas ke-%d dari %q belum punya judul", domain.ErrInvalidInput, position, task)
	}
	if len([]rune(trimmed)) > domain.MaxTitleRunes {
		return "", fmt.Errorf(
			"%w: judul sub-tugas ke-%d dari %q lebih dari %d karakter",
			domain.ErrInvalidInput, position, task, domain.MaxTitleRunes)
	}

	return trimmed, nil
}

// ---------------------------------------------------------------- messages

// openWork renders what is still open in a plan, which is what a refused create answers with.
//
// It lists the tasks, and the subtasks of the one the model is most likely to be in the middle of,
// because the message is read by a model deciding what to do next: the first unfinished task and its
// open lines are what it needs, and the rest of the list is there so it can see the shape of the job.
func openWork(plan domain.Checklist) string {
	done, total := plan.Counts()

	open := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		if item.Done {
			continue
		}

		line := item.Title
		if unfinished := item.OpenSubtasks(); len(unfinished) > 0 {
			titles := make([]string, 0, len(unfinished))
			for _, subtask := range unfinished {
				titles = append(titles, subtask.Title)
			}

			line += " (belum: " + strings.Join(titles, ", ") + ")"
		}

		open = append(open, line)
	}

	if len(open) == 0 {
		return fmt.Sprintf("rencana %q masih aktif walau semua tugasnya sudah ditandai selesai (%d/%d)", plan.Title, done, total)
	}

	return fmt.Sprintf(
		"rencana %q masih berjalan (%d dari %d selesai). Selesaikan dulu, atau buang rencananya dengan checklist.clear. Yang belum selesai: %s",
		plan.Title, done, total, strings.Join(open, "; "))
}

// openSubtasks renders the subtasks that stop a task from being ticked.
func openSubtasks(items []domain.Item) string {
	titles := make([]string, 0, len(items))
	for _, item := range items {
		titles = append(titles, item.Title)
	}

	return fmt.Sprintf("sub-tugas ini belum selesai: %s. Tandai dulu satu per satu, baru tugas induknya.",
		strings.Join(titles, ", "))
}

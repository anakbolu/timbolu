package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/domain/mocks"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/service"
)

// This file is the module's rules rather than Postgres's: one plan at a time, a task is not done while
// its subtasks are open, and what a call without a tenant is told. The repository is a mock precisely
// because none of that is storage's job — and because the refusals are the product here, a test that
// needed a database would be testing the wrong layer.

// harness is the service over a scripted repository.
type harness struct {
	repo  *mocks.Repository
	svc   *service.Service
	scope domain.Scope
	agent uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	repo := mocks.NewRepository(t)

	return &harness{
		repo:  repo,
		svc:   service.New(service.Deps{Repository: repo, Logger: zap.NewNop()}),
		scope: domain.Scope{UserID: uuid.New(), WorkspaceID: uuid.New()},
		agent: uuid.New(),
	}
}

// taskSpec is one task in a fixture: its title, whether it is already ticked, and its subtasks.
type taskSpec struct {
	title    string
	done     bool
	subtasks []subSpec
}

// subSpec is one subtask in a fixture.
type subSpec struct {
	title string
	done  bool
}

// task is an open task, with whatever subtasks are given.
func task(title string, subtasks ...subSpec) taskSpec {
	return taskSpec{title: title, subtasks: subtasks}
}

// doneTask is a task that is already ticked.
func doneTask(title string, subtasks ...subSpec) taskSpec {
	return taskSpec{title: title, done: true, subtasks: subtasks}
}

// sub is an open subtask.
func sub(title string) subSpec { return subSpec{title: title} }

// doneSub is a subtask that is already ticked.
func doneSub(title string) subSpec { return subSpec{title: title, done: true} }

// plan builds a plan from those fixtures, with an id on everything so a test can tick one line.
func plan(tasks ...taskSpec) domain.Checklist {
	checklist := domain.Checklist{ID: uuid.New(), AgentID: uuid.New(), Title: "Rencana", Status: domain.StatusActive}
	for _, spec := range tasks {
		item := domain.Item{ID: uuid.New(), ChecklistID: checklist.ID, Title: spec.title, Done: spec.done}
		for _, sub := range spec.subtasks {
			item.Subtasks = append(item.Subtasks, domain.Item{
				ID: uuid.New(), ChecklistID: checklist.ID, ParentID: item.ID, Title: sub.title, Done: sub.done,
			})
		}

		checklist.Items = append(checklist.Items, item)
	}

	return checklist
}

// TestACallWithoutAWorkspaceIsRefused: the workspace is the one thing every method needs, so a call
// that names none must not reach storage — a scope-less query would see every tenant's rows or none,
// and both are worse than a refusal.
func TestACallWithoutAWorkspaceIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Read(t.Context(), domain.Scope{}, h.agent)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.svc.Create(t.Context(), domain.Scope{}, domain.NewChecklist{AgentID: h.agent})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.svc.Check(t.Context(), domain.Scope{}, h.agent, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.svc.Remove(t.Context(), domain.Scope{}, h.agent, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	require.ErrorIs(t, h.svc.Clear(t.Context(), domain.Scope{}, h.agent), domain.ErrInvalidInput)

	_, err = h.svc.List(t.Context(), domain.Scope{}, domain.Filter{})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestACallWithoutABoluIsRefused: a plan belongs to a Bolu, so a call that names none has nowhere to
// put it. It is refused before storage rather than written against the zero uuid, which the foreign key
// would reject as a driver error.
func TestACallWithoutABoluIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Read(t.Context(), h.scope, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.svc.Create(t.Context(), h.scope, domain.NewChecklist{Tasks: []domain.NewItem{{Title: "satu"}}})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	_, err = h.svc.Check(t.Context(), h.scope, uuid.Nil, uuid.New())
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	require.ErrorIs(t, h.svc.Clear(t.Context(), h.scope, uuid.Nil), domain.ErrInvalidInput)
}

// TestAPlanNeedsAtLeastOneTask: a plan with nothing in it is not a plan, and it would leave the Bolu
// holding an empty list it has to clear before it can write a real one.
func TestAPlanNeedsAtLeastOneTask(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Create(t.Context(), h.scope, domain.NewChecklist{AgentID: h.agent})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "belum berisi tugas")
}

// TestEveryTaskAndSubtaskNeedsATitle: a title is the whole of what a person reads, so a blank one is
// refused with the place it was in — a model that sent twenty tasks has to know which one to fix.
func TestEveryTaskAndSubtaskNeedsATitle(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.Create(t.Context(), h.scope, domain.NewChecklist{
		AgentID: h.agent,
		Tasks:   []domain.NewItem{{Title: " "}},
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "tugas ke-1")

	_, err = h.svc.Create(t.Context(), h.scope, domain.NewChecklist{
		AgentID: h.agent,
		Tasks:   []domain.NewItem{{Title: "cari harga"}, {Title: "kirim tagihan", Subtasks: []string{" "}}},
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "kirim tagihan")
}

// TestTheBoundsAreEnforced: a plan is a list a person reads. A model given no ceiling writes a hundred
// lines and calls it thorough, so the ceilings are the product's judgement rather than storage's.
func TestTheBoundsAreEnforced(t *testing.T) {
	h := newHarness(t)

	many := make([]domain.NewItem, 0, domain.MaxTasks+1)
	for range domain.MaxTasks + 1 {
		many = append(many, domain.NewItem{Title: "tugas"})
	}

	_, err := h.svc.Create(t.Context(), h.scope, domain.NewChecklist{AgentID: h.agent, Tasks: many})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
	require.Contains(t, err.Error(), "paling banyak")

	subtasks := make([]string, 0, domain.MaxSubtasks+1)
	for range domain.MaxSubtasks + 1 {
		subtasks = append(subtasks, "langkah")
	}

	_, err = h.svc.Create(t.Context(), h.scope, domain.NewChecklist{
		AgentID: h.agent,
		Tasks:   []domain.NewItem{{Title: "satu", Subtasks: subtasks}},
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	long := strings.Repeat("a", domain.MaxTitleRunes+1)
	_, err = h.svc.Create(t.Context(), h.scope, domain.NewChecklist{
		AgentID: h.agent,
		Title:   long,
		Tasks:   []domain.NewItem{{Title: "satu"}},
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestASecondPlanIsRefusedWithWhatIsStillOpen: the rule is one plan at a time, and the refusal is the
// part a model can act on — "you still have a plan" without the list leaves it nothing to do but ask a
// person, and the list is the one thing that could have told it.
func TestASecondPlanIsRefusedWithWhatIsStillOpen(t *testing.T) {
	h := newHarness(t)

	open := plan(
		task("cari harga kopi", sub("buka katalog"), sub("catat harga")),
		task("kirim tagihan"),
	)
	open.Title = "Tagih pelanggan"
	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(open, nil)

	_, err := h.svc.Create(t.Context(), h.scope, domain.NewChecklist{
		AgentID: h.agent,
		Tasks:   []domain.NewItem{{Title: "yang baru"}},
	})
	require.ErrorIs(t, err, domain.ErrActiveExists)
	require.Contains(t, err.Error(), "Tagih pelanggan")
	require.Contains(t, err.Error(), "cari harga kopi")
	require.Contains(t, err.Error(), "buka katalog")
	require.Contains(t, err.Error(), "checklist.clear")
}

// TestTheNextPlanIsWrittenWhenNothingIsOpen: a Bolu that finished its plan — or never wrote one — may
// write the next, and the titles arrive trimmed with the order they were written in.
func TestTheNextPlanIsWrittenWhenNothingIsOpen(t *testing.T) {
	h := newHarness(t)

	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).
		Return(domain.Checklist{}, domain.ErrNotFound)

	var written domain.NewChecklist
	h.repo.EXPECT().Create(mock.Anything, h.scope, mock.Anything).
		Run(func(_ context.Context, _ domain.Scope, arg domain.NewChecklist) { written = arg }).
		Return(domain.Checklist{ID: uuid.New()}, nil)

	_, err := h.svc.Create(t.Context(), h.scope, domain.NewChecklist{
		AgentID: h.agent,
		TaskID:  uuid.New(),
		Title:   "  Tagih pelanggan  ",
		Tasks: []domain.NewItem{
			{Title: "  cari harga  ", Subtasks: []string{" buka katalog "}},
			{Title: "kirim tagihan"},
		},
	})
	require.NoError(t, err)

	require.Equal(t, "Tagih pelanggan", written.Title)
	require.Equal(t, h.agent, written.AgentID)
	require.Len(t, written.Tasks, 2)
	require.Equal(t, "cari harga", written.Tasks[0].Title)
	require.Equal(t, []string{"buka katalog"}, written.Tasks[0].Subtasks)
	require.Equal(t, "kirim tagihan", written.Tasks[1].Title)
}

// TestAFailureToReadThePlanIsNotAnEmptyPlan: an error that is not "you have no plan" must not be read
// as one. Treating it that way would write a second plan over the top of a plan the database failed to
// hand back.
func TestAFailureToReadThePlanIsNotAnEmptyPlan(t *testing.T) {
	h := newHarness(t)

	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(domain.Checklist{}, errDatabase)

	_, err := h.svc.Create(t.Context(), h.scope, domain.NewChecklist{
		AgentID: h.agent,
		Tasks:   []domain.NewItem{{Title: "satu"}},
	})
	require.ErrorIs(t, err, errDatabase)
}

// TestTickingATaskWithOpenSubtasksIsRefused: a task is done when its subtasks are, so ticking the
// parent first is refused with the lines that are still open — the alternative is a plan that reads as
// finished while work is left in it.
func TestTickingATaskWithOpenSubtasksIsRefused(t *testing.T) {
	h := newHarness(t)

	open := plan(task("kirim tagihan", sub("susun tagihan"), doneSub("kirim")))
	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(open, nil)

	_, err := h.svc.Check(t.Context(), h.scope, h.agent, open.Items[0].ID)
	require.ErrorIs(t, err, domain.ErrSubtasksOpen)
	require.Contains(t, err.Error(), "susun tagihan")
}

// TestTickingSomethingAlreadyDoneChangesNothing: a model that ticks the same line twice meant the same
// thing both times, so the second call answers with the plan rather than costing a round.
func TestTickingSomethingAlreadyDoneChangesNothing(t *testing.T) {
	h := newHarness(t)

	finished := plan(doneTask("cari harga"))
	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(finished, nil)

	updated, err := h.svc.Check(t.Context(), h.scope, h.agent, finished.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, finished, updated)
}

// TestTickingTheLastTaskFinishesThePlan: the answer carries the settled plan, which is the thing the
// model needs — it is free to write the next one.
func TestTickingTheLastTaskFinishesThePlan(t *testing.T) {
	h := newHarness(t)

	open := plan(task("cari harga", doneSub("buka katalog")))
	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(open, nil)

	// The settled plan is built separately rather than edited from `open`: the items are a slice, so
	// ticking a line on a copy would tick it on the plan the service is still reading.
	settled := plan(doneTask("cari harga", doneSub("buka katalog")))
	settled.ID = open.ID
	settled.Status = domain.StatusDone
	h.repo.EXPECT().MarkDone(mock.Anything, h.scope, open.ID, open.Items[0].ID).Return(settled, nil)

	updated, err := h.svc.Check(t.Context(), h.scope, h.agent, open.Items[0].ID)
	require.NoError(t, err)
	require.False(t, updated.Active())
	require.Equal(t, domain.StatusDone, updated.Status)
}

// TestTickingAnItemThatIsNotInThePlanIsRefused: an id from an older plan is a mistake the model has to
// hear about, not a silent no-op — and the message says the item is not in the plan that is open,
// which is what sends it to checklist.read rather than hunting for a typo.
func TestTickingAnItemThatIsNotInThePlanIsRefused(t *testing.T) {
	h := newHarness(t)

	open := plan(task("cari harga"))
	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(open, nil)

	_, err := h.svc.Check(t.Context(), h.scope, h.agent, uuid.New())
	require.ErrorIs(t, err, domain.ErrNotFound)

	_, err = h.svc.Check(t.Context(), h.scope, h.agent, uuid.Nil)
	require.ErrorIs(t, err, domain.ErrInvalidInput)
}

// TestRemovingAnItemAnswersWithWhatIsLeft: dropping a line is a change to the plan like a tick is, so
// the model reads the plan back rather than an acknowledgement.
func TestRemovingAnItemAnswersWithWhatIsLeft(t *testing.T) {
	h := newHarness(t)

	open := plan(task("cari harga"), task("kirim tagihan"))
	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(open, nil)

	left := plan(task("kirim tagihan"))
	h.repo.EXPECT().RemoveItem(mock.Anything, h.scope, open.ID, open.Items[0].ID).Return(left, nil)

	updated, err := h.svc.Remove(t.Context(), h.scope, h.agent, open.Items[0].ID)
	require.NoError(t, err)
	require.Len(t, updated.Items, 1)
	require.Equal(t, "kirim tagihan", updated.Items[0].Title)
}

// TestClearingIsWhatFreesTheNextPlan: the escape hatch the one-plan rule needs. It names the plan the
// Bolu actually holds rather than the one the caller thought it had.
func TestClearingIsWhatFreesTheNextPlan(t *testing.T) {
	h := newHarness(t)

	open := plan(task("cari harga"))
	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(open, nil)
	h.repo.EXPECT().Clear(mock.Anything, h.scope, open.ID).Return(nil)

	require.NoError(t, h.svc.Clear(t.Context(), h.scope, h.agent))
}

// TestClearingWithNoPlanIsReported: there is nothing to throw away, and saying so is what stops a model
// from believing it freed a plan that never existed.
func TestClearingWithNoPlanIsReported(t *testing.T) {
	h := newHarness(t)

	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(domain.Checklist{}, domain.ErrNotFound)

	err := h.svc.Clear(t.Context(), h.scope, h.agent)
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.Contains(t, err.Error(), "tidak ada rencana aktif")
}

// TestTickingWithNoPlanSaysWhatToDo: a tick against a plan that is over is the case a model hits when it
// lost track, so the refusal names the tool that starts the next one rather than only the fact.
func TestTickingWithNoPlanSaysWhatToDo(t *testing.T) {
	h := newHarness(t)

	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(domain.Checklist{}, domain.ErrNotFound)

	_, err := h.svc.Check(t.Context(), h.scope, h.agent, uuid.New())
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.Contains(t, err.Error(), "checklist.create")
}

// TestReadingAPlanWithNoPlanIsNotFound: "you have not written one" is the honest answer, and the tool
// turns it into an empty list rather than an error — a model that receives an error learns its tool is
// broken.
func TestReadingAPlanWithNoPlanIsNotFound(t *testing.T) {
	h := newHarness(t)

	h.repo.EXPECT().Active(mock.Anything, h.scope, h.agent).Return(domain.Checklist{}, domain.ErrNotFound)

	_, err := h.svc.Read(t.Context(), h.scope, h.agent)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// TestTheListRefusesAStatusItDoesNotHave: a filter is a question, and a question about a state the
// module does not have is a client bug rather than an empty page.
func TestTheListRefusesAStatusItDoesNotHave(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.List(t.Context(), h.scope, domain.Filter{Status: "abandoned"})
	require.ErrorIs(t, err, domain.ErrInvalidInput)

	for _, status := range []string{"", domain.StatusActive, domain.StatusDone} {
		h.repo.EXPECT().List(mock.Anything, h.scope, mock.Anything).Return([]domain.Checklist{}, nil)
		_, err := h.svc.List(t.Context(), h.scope, domain.Filter{Status: status})
		require.NoError(t, err)
	}
}

// errDatabase stands in for a storage failure: something that is neither "not found" nor this module's
// own refusal.
var errDatabase = errors.New("checklist: the database is on fire")

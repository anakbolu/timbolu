package adapter_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/adapter"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain"
	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
)

// The adapter decides nothing about the plan — that is the service's job — so what this file tests is
// its contract with the runtime: which tools it declares, what it does with a call's JSON, and the
// exact shape the model reads back. The port is a hand-written fake rather than the mock so that the
// fake can be a plain function of its inputs; the point is the JSON, not the wiring.

// fakePlans is the adapter's port, scripted per test and recording what it was asked.
type fakePlans struct {
	readResult  domain.Checklist
	readErr     error
	readScope   domain.Scope
	readAgentID uuid.UUID

	createResult domain.Checklist
	createErr    error
	createScope  domain.Scope
	created      domain.NewChecklist

	checkResult domain.Checklist
	checkErr    error
	checkScope  domain.Scope
	checkedItem uuid.UUID
	checkedWith uuid.UUID

	removeResult domain.Checklist
	removeErr    error
	removeItem   uuid.UUID

	clearErr     error
	clearScope   domain.Scope
	clearAgentID uuid.UUID
}

func (f *fakePlans) Read(_ context.Context, scope domain.Scope, agentID uuid.UUID) (domain.Checklist, error) {
	f.readScope, f.readAgentID = scope, agentID

	return f.readResult, f.readErr
}

func (f *fakePlans) Create(_ context.Context, scope domain.Scope, plan domain.NewChecklist) (domain.Checklist, error) {
	f.createScope, f.created = scope, plan

	return f.createResult, f.createErr
}

func (f *fakePlans) Check(_ context.Context, scope domain.Scope, agentID, itemID uuid.UUID) (domain.Checklist, error) {
	f.checkScope, f.checkedWith, f.checkedItem = scope, agentID, itemID

	return f.checkResult, f.checkErr
}

func (f *fakePlans) Remove(_ context.Context, scope domain.Scope, agentID, itemID uuid.UUID) (domain.Checklist, error) {
	f.checkedWith, f.removeItem = agentID, itemID

	return f.removeResult, f.removeErr
}

func (f *fakePlans) Clear(_ context.Context, scope domain.Scope, agentID uuid.UUID) error {
	f.clearScope, f.clearAgentID = scope, agentID

	return f.clearErr
}

// callScope is a scope with the attribution a plan has to carry: the Bolu that owns it, and the task it
// was written during.
func callScope() integrationdomain.Scope {
	return integrationdomain.Scope{
		UserID:      uuid.New(),
		WorkspaceID: uuid.New(),
		AgentID:     uuid.New(),
		TaskID:      uuid.New(),
	}
}

// execute runs one call against a fake.
func execute(t *testing.T, plans *fakePlans, scope integrationdomain.Scope, tool string, arguments json.RawMessage) (integrationdomain.Result, error) {
	t.Helper()

	return adapter.New(plans).Execute(t.Context(), integrationdomain.Call{
		Scope:     scope,
		Tool:      tool,
		Arguments: arguments,
	})
}

// arguments renders the tool arguments a model would send.
func arguments(t *testing.T, value any) json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return encoded
}

// payload reads a result's content as the document the model reads.
func payload(t *testing.T, result integrationdomain.Result) map[string]any {
	t.Helper()

	var document map[string]any
	require.NoError(t, json.Unmarshal(result.Content, &document))

	return document
}

// TestTheAdapterIsTheChecklistApplication: a plan belongs to the Bolu, not to an account somebody
// connected, so the adapter declares that a call needs neither a token nor a connection — which is what
// lets the tool filter offer these tools without a grant.
func TestTheAdapterIsTheChecklistApplication(t *testing.T) {
	a := adapter.New(&fakePlans{})

	require.Equal(t, adapter.App, a.App())
	require.False(t, a.NeedsConnection())
	require.False(t, a.NeedsToken())
	require.Nil(t, a.Triggers())

	var connectionless integrationdomain.Connectionless = a
	require.False(t, connectionless.NeedsConnection())

	var credentialled integrationdomain.Credentialled = a
	require.False(t, credentialled.NeedsToken())
}

// TestTheToolsAreDeclaredWithTheirLabels: the label is the only thing that decides whether a person is
// asked before a call goes out, so writing and moving a plan is `write_internal` — it changes what is
// inside Bolu and nothing outside it — while reading it stays `read`.
func TestTheToolsAreDeclaredWithTheirLabels(t *testing.T) {
	specs := adapter.New(&fakePlans{}).Tools()
	require.Len(t, specs, 5)

	byName := map[string]integrationdomain.ToolSpec{}
	for _, spec := range specs {
		require.NoError(t, spec.Valid())
		require.Equal(t, adapter.App, spec.App())

		var schema map[string]any
		require.NoError(t, json.Unmarshal(spec.Schema, &schema))
		require.Equal(t, "object", schema["type"])

		byName[spec.Name] = spec
	}

	require.Equal(t, integrationdomain.LabelWriteInternal, byName[adapter.ToolCreate].Label)
	require.Equal(t, integrationdomain.LabelWriteInternal, byName[adapter.ToolCheck].Label)
	require.Equal(t, integrationdomain.LabelWriteInternal, byName[adapter.ToolRemove].Label)
	require.Equal(t, integrationdomain.LabelWriteInternal, byName[adapter.ToolClear].Label)
	require.Equal(t, integrationdomain.LabelRead, byName[adapter.ToolRead].Label)

	// The create schema requires the tasks, because a plan with none is the mistake the model makes
	// most often and a schema is the cheapest place to catch it.
	var createSchema map[string]any
	require.NoError(t, json.Unmarshal(byName[adapter.ToolCreate].Schema, &createSchema))
	require.Equal(t, []any{"tasks"}, createSchema["required"])
}

// TestAnUnknownToolIsRefused: a name the adapter does not answer to is a runtime mistake, not a
// workspace question, so it is reported as such rather than answered as an empty result.
func TestAnUnknownToolIsRefused(t *testing.T) {
	_, err := execute(t, &fakePlans{}, callScope(), "checklist.does_not_exist", json.RawMessage(`{}`))
	require.ErrorIs(t, err, integrationdomain.ErrUnknownTool)
}

// TestArgumentsThatAreNotJSONAreASchemaViolation: every tool parses before it does anything, so
// malformed arguments cost nothing and are reported before the port is reached.
func TestArgumentsThatAreNotJSONAreASchemaViolation(t *testing.T) {
	// The three that read arguments. The other two take none, so there is nothing for them to parse:
	// the hub's own schema check is what refuses a call that sends them anyway.
	tools := []string{adapter.ToolCreate, adapter.ToolCheck, adapter.ToolRemove}

	for _, tool := range tools {
		t.Run(tool, func(t *testing.T) {
			_, err := execute(t, &fakePlans{}, callScope(), tool, json.RawMessage(`not json`))
			require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)
		})
	}
}

// TestCreatingAPlanAnswersWithItsIds: the answer is what makes the rest of the plan work — a model that
// has just written a list can tick a line without reading it back, because the ids travel with it.
func TestCreatingAPlanAnswersWithItsIds(t *testing.T) {
	scope := callScope()
	fake := &fakePlans{createResult: plan()}

	result, err := execute(t, fake, scope, adapter.ToolCreate, arguments(t, map[string]any{
		"title": "Tagih pelanggan",
		"tasks": []any{
			map[string]any{"title": "cari harga", "subtasks": []string{"buka katalog"}},
			map[string]any{"title": "kirim tagihan"},
		},
	}))
	require.NoError(t, err)

	// The plan is attributed to the Bolu that wrote it and the task it was working on.
	require.Equal(t, scope.AgentID, fake.created.AgentID)
	require.Equal(t, scope.TaskID, fake.created.TaskID)
	require.Equal(t, scope.UserID, fake.createScope.UserID)
	require.Equal(t, scope.WorkspaceID, fake.createScope.WorkspaceID)
	require.Equal(t, "Tagih pelanggan", fake.created.Title)
	require.Len(t, fake.created.Tasks, 2)
	require.Equal(t, []string{"buka katalog"}, fake.created.Tasks[0].Subtasks)

	document := payload(t, result)
	rencana, ok := document["rencana"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "active", rencana["status"])
	// The fixture is one ticked task with a ticked subtask and one open task: three lines, two done.
	require.Equal(t, float64(2), rencana["selesai"])
	require.Equal(t, float64(3), rencana["total"])

	tasks := rencana["tugas"].([]any)
	require.Len(t, tasks, 2)
	require.NotEmpty(t, tasks[0].(map[string]any)["id"])
	require.Equal(t, "buka katalog", tasks[0].(map[string]any)["sub_tugas"].([]any)[0].(map[string]any)["judul"])
	require.Equal(t, true, tasks[0].(map[string]any)["selesai"], "the fixture's first task is ticked")
	require.Equal(t, false, tasks[1].(map[string]any)["selesai"])

	require.Contains(t, result.Summary, "Rencana")
}

// TestARefusedCreateIsReportedAsAnError: the service's refusal is what the model has to read, so it
// travels unchanged — a second plan is refused with what is still open rather than with an
// acknowledgement.
func TestARefusedCreateIsReportedAsAnError(t *testing.T) {
	fake := &fakePlans{createErr: domain.ErrActiveExists}

	_, err := execute(t, fake, callScope(), adapter.ToolCreate, arguments(t, map[string]any{
		"tasks": []any{map[string]any{"title": "yang baru"}},
	}))

	require.ErrorIs(t, err, domain.ErrActiveExists)
}

// TestTickingALineNamesTheItemAndTheBolu: the id in the arguments is the item, and the Bolu comes from
// the call's scope rather than from the arguments — a model cannot tick a line in somebody else's plan
// by naming their id.
func TestTickingALineNamesTheItemAndTheBolu(t *testing.T) {
	scope := callScope()
	itemID := uuid.New()
	fake := &fakePlans{checkResult: plan()}

	result, err := execute(t, fake, scope, adapter.ToolCheck, arguments(t, map[string]any{"id": itemID.String()}))
	require.NoError(t, err)

	require.Equal(t, itemID, fake.checkedItem)
	require.Equal(t, scope.AgentID, fake.checkedWith)
	require.Equal(t, scope.WorkspaceID, fake.checkScope.WorkspaceID)
	require.Contains(t, result.Summary, "Satu langkah selesai")
}

// TestAnIdThatIsNotAUUIDIsASchemaViolation: the model is the one that has to fix it, and "the id must
// be a uuid" is a sentence it can act on where a driver error is not.
func TestAnIdThatIsNotAUUIDIsASchemaViolation(t *testing.T) {
	for _, tool := range []string{adapter.ToolCheck, adapter.ToolRemove} {
		t.Run(tool, func(t *testing.T) {
			_, err := execute(t, &fakePlans{}, callScope(), tool, arguments(t, map[string]any{"id": "tugas pertama"}))
			require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)
			require.Contains(t, err.Error(), "uuid")
		})
	}
}

// TestFinishingThePlanSaysSo: the answer is where the model learns it may write the next plan, so a
// settled one is summarised as finished rather than as one more line ticked.
func TestFinishingThePlanSaysSo(t *testing.T) {
	finished := plan()
	finished.Status = domain.StatusDone

	result, err := execute(t, &fakePlans{checkResult: finished}, callScope(), adapter.ToolCheck,
		arguments(t, map[string]any{"id": uuid.New().String()}))
	require.NoError(t, err)

	require.Contains(t, result.Summary, "selesai")
	require.Equal(t, "done", payload(t, result)["rencana"].(map[string]any)["status"])
}

// TestRemovingALineAnswersWithWhatIsLeft: dropping a line is a change to the plan like a tick is, so
// the model reads the plan back rather than an acknowledgement.
func TestRemovingALineAnswersWithWhatIsLeft(t *testing.T) {
	itemID := uuid.New()
	fake := &fakePlans{removeResult: plan()}

	result, err := execute(t, fake, callScope(), adapter.ToolRemove, arguments(t, map[string]any{"id": itemID.String()}))
	require.NoError(t, err)

	require.Equal(t, itemID, fake.removeItem)
	require.Contains(t, result.Summary, "dihapus")
	require.NotNil(t, payload(t, result)["rencana"])
}

// TestClearingSaysTheBoluIsFreeAgain: the point of clearing is that the Bolu may write another plan, and
// the answer says so — a model that is not told would go looking for the plan it just threw away.
func TestClearingSaysTheBoluIsFreeAgain(t *testing.T) {
	scope := callScope()
	fake := &fakePlans{}

	result, err := execute(t, fake, scope, adapter.ToolClear, json.RawMessage(`{}`))
	require.NoError(t, err)

	require.Equal(t, scope.AgentID, fake.clearAgentID)
	require.Equal(t, scope.WorkspaceID, fake.clearScope.WorkspaceID)

	document := payload(t, result)
	require.Equal(t, true, document["dibuang"])
	require.Contains(t, document["catatan"], "checklist.create")
}

// TestReadingWithNoPlanIsAnAnswerNotAnError: "what am I working on" has the answer "nothing", and a
// model that receives an error learns its tool is broken rather than that it has not written a plan.
func TestReadingWithNoPlanIsAnAnswerNotAnError(t *testing.T) {
	fake := &fakePlans{readErr: domain.ErrNotFound}

	result, err := execute(t, fake, callScope(), adapter.ToolRead, json.RawMessage(`{}`))
	require.NoError(t, err)

	document := payload(t, result)
	require.Nil(t, document["rencana"])
	require.Contains(t, document["catatan"], "Belum ada rencana")
	require.Contains(t, result.Summary, "Belum ada")
}

// TestReadingAPlanAnswersWithTheTree: the read is what a model does after a restart, so it carries the
// same shape the write did — the ids, the ticks, and the subtasks nested under their task.
func TestReadingAPlanAnswersWithTheTree(t *testing.T) {
	scope := callScope()
	fake := &fakePlans{readResult: plan()}

	result, err := execute(t, fake, scope, adapter.ToolRead, json.RawMessage(`{}`))
	require.NoError(t, err)

	require.Equal(t, scope.AgentID, fake.readAgentID)
	require.Equal(t, scope.WorkspaceID, fake.readScope.WorkspaceID)

	rencana := payload(t, result)["rencana"].(map[string]any)
	require.Equal(t, float64(3), rencana["total"])
	require.Equal(t, float64(2), rencana["selesai"])
	require.Len(t, rencana["tugas"].([]any), 2)
}

// plan is a fixture with two tasks, one of them ticked and carrying a subtask.
func plan() domain.Checklist {
	id := uuid.New()

	return domain.Checklist{
		ID:      id,
		AgentID: uuid.New(),
		Title:   "Tagih pelanggan",
		Status:  domain.StatusActive,
		Items: []domain.Item{
			{
				ID: id, Title: "cari harga", Done: true,
				Subtasks: []domain.Item{{ID: uuid.New(), Title: "buka katalog", Done: true}},
			},
			{ID: uuid.New(), Title: "kirim tagihan"},
		},
	}
}

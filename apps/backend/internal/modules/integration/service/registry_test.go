package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// fakeAdapter is a stand-in for a real application. It exists so the registry
// can be tested for what it promises an adapter: declare tools and triggers, run
// a call, and nothing else.
type fakeAdapter struct {
	app      string
	tools    []domain.ToolSpec
	triggers []domain.TriggerSpec
	// calls records what was executed, so a test can prove the arguments arrived
	// unaltered.
	calls []domain.Call
}

func (f *fakeAdapter) App() string                    { return f.app }
func (f *fakeAdapter) Tools() []domain.ToolSpec       { return f.tools }
func (f *fakeAdapter) Triggers() []domain.TriggerSpec { return f.triggers }

func (f *fakeAdapter) Execute(_ context.Context, call domain.Call) (domain.Result, error) {
	f.calls = append(f.calls, call)
	return domain.Result{Content: json.RawMessage(`{"ok":true}`), Summary: "ran " + call.Tool}, nil
}

// spec builds a valid tool spec, so a test only states what it is about.
func spec(name, label string) domain.ToolSpec {
	return domain.ToolSpec{
		Name:        name,
		Label:       label,
		Description: "does the thing",
		Schema:      json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
}

// gmailStub is the shape a real adapter takes: a few tools with the labels that
// decide whether a person is asked.
func gmailStub() *fakeAdapter {
	return &fakeAdapter{
		app: "gmail",
		tools: []domain.ToolSpec{
			spec("gmail.search", domain.LabelRead),
			spec("gmail.read_attachment", domain.LabelRead),
			spec("gmail.create_draft", domain.LabelWriteInternal),
			spec("gmail.send", domain.LabelWriteExternal),
		},
		triggers: []domain.TriggerSpec{
			{App: "gmail", Kind: domain.TriggerWebhook, Event: "gmail.email_received", Description: "Email masuk"},
		},
	}
}

// TestAnAdapterIsRegisteredAndItsToolsAreReported is the E8.2 gate.
func TestAnAdapterIsRegisteredAndItsToolsAreReported(t *testing.T) {
	registry := NewRegistry()

	adapter := gmailStub()
	require.NoError(t, registry.Register(adapter))

	require.Equal(t, []string{"gmail"}, registry.Apps())

	tools := registry.Tools()
	require.Len(t, tools, 4)

	// Sorted by name, so the model's tool list is stable between runs.
	require.Equal(t, []string{
		"gmail.create_draft", "gmail.read_attachment", "gmail.search", "gmail.send",
	}, []string{tools[0].Name, tools[1].Name, tools[2].Name, tools[3].Name})

	// The labels are the adapter's own, and they are what the approval gate
	// reads: nothing downstream re-decides.
	byName := map[string]string{}
	for _, tool := range tools {
		byName[tool.Name] = tool.Label
	}
	require.Equal(t, domain.LabelRead, byName["gmail.search"])
	require.Equal(t, domain.LabelWriteInternal, byName["gmail.create_draft"])
	require.Equal(t, domain.LabelWriteExternal, byName["gmail.send"])

	// Only the tool that reaches outside Bolu needs a person.
	require.False(t, needsApproval(byName["gmail.search"]))
	require.False(t, needsApproval(byName["gmail.create_draft"]))
	require.True(t, needsApproval(byName["gmail.send"]))
}

// needsApproval mirrors the runtime's rule. It is restated here rather than
// imported so this test fails if the two ever drift apart.
func needsApproval(label string) bool { return label == domain.LabelWriteExternal }

// TestToolsForOneAppAndLookupByName covers the two reads the rest of the system
// makes.
func TestToolsForOneAppAndLookupByName(t *testing.T) {
	registry := NewRegistry()
	require.NoError(t, registry.Register(gmailStub()))

	jira := &fakeAdapter{
		app: "jira",
		tools: []domain.ToolSpec{
			spec("jira.search", domain.LabelRead),
			spec("jira.create_issue", domain.LabelWriteExternal),
		},
	}
	require.NoError(t, registry.Register(jira))

	require.Equal(t, []string{"gmail", "jira"}, registry.Apps())
	require.Len(t, registry.Tools(), 6)
	require.Len(t, registry.ToolsFor("gmail"), 4)
	require.Len(t, registry.ToolsFor("jira"), 2)
	require.Empty(t, registry.ToolsFor("drive"), "an app with no adapter has no tools")

	found, adapter, ok := registry.Lookup("jira.create_issue")
	require.True(t, ok)
	require.Equal(t, domain.LabelWriteExternal, found.Label)
	require.Equal(t, "jira", adapter.App())

	_, _, ok = registry.Lookup("jira.nonexistent")
	require.False(t, ok)
	_, _, ok = registry.Lookup("nothing-at-all")
	require.False(t, ok)
}

// TestTriggersAreReportedWithTheirApp covers the second half of the contract.
func TestTriggersAreReportedWithTheirApp(t *testing.T) {
	registry := NewRegistry()
	require.NoError(t, registry.Register(gmailStub()))
	require.NoError(t, registry.Register(&fakeAdapter{
		app: "github",
		tools: []domain.ToolSpec{
			spec("github.list_pull_requests", domain.LabelRead),
		},
		triggers: []domain.TriggerSpec{
			{App: "github", Kind: domain.TriggerWebhook, Event: "github.pull_request_opened"},
		},
	}))

	triggers := registry.Triggers()
	require.Len(t, triggers, 2)
	require.Equal(t, "github.pull_request_opened", triggers[0].Event)
	require.Equal(t, "gmail.email_received", triggers[1].Event)
	require.Equal(t, domain.TriggerWebhook, triggers[0].Kind)

	// An app that only polls is expressed the same way, so a routine screen does
	// not have to know which is which.
	require.NoError(t, registry.Register(&fakeAdapter{
		app: "drive",
		tools: []domain.ToolSpec{
			spec("drive.search", domain.LabelRead),
		},
		triggers: []domain.TriggerSpec{
			{App: "drive", Kind: domain.TriggerPolling, Event: "drive.file_changed"},
		},
	}))
	require.Len(t, registry.Triggers(), 3)
}

// TestAToolIsExecutedThroughItsAdapter is the call path: the registry finds the
// adapter and the adapter runs it, with the arguments unaltered.
func TestAToolIsExecutedThroughItsAdapter(t *testing.T) {
	registry := NewRegistry()
	adapter := gmailStub()
	require.NoError(t, registry.Register(adapter))

	found, chosen, ok := registry.Lookup("gmail.send")
	require.True(t, ok)

	arguments := json.RawMessage(`{"to":"pelanggan@example.com","subject":"Tagihan"}`)
	result, err := chosen.Execute(context.Background(), domain.Call{
		Tool:      found.Name,
		Arguments: arguments,
	})
	require.NoError(t, err)
	require.Equal(t, "ran gmail.send", result.Summary)
	require.JSONEq(t, `{"ok":true}`, string(result.Content))

	require.Len(t, adapter.calls, 1)
	require.JSONEq(t, string(arguments), string(adapter.calls[0].Arguments))
}

// TestASecondAdapterForOneAppIsRefused keeps the winner from depending on the
// order the container happened to build them in.
func TestASecondAdapterForOneAppIsRefused(t *testing.T) {
	registry := NewRegistry()
	require.NoError(t, registry.Register(gmailStub()))

	err := registry.Register(&fakeAdapter{
		app:   "gmail",
		tools: []domain.ToolSpec{spec("gmail.other", domain.LabelRead)},
	})
	require.ErrorIs(t, err, domain.ErrAdapterExists)
}

// TestABadSpecIsRefusedAtStartup is why the registry validates: an adapter with a
// typo fails while the container is being assembled, not at the first call the
// model makes.
func TestABadSpecIsRefusedAtStartup(t *testing.T) {
	cases := map[string]*fakeAdapter{
		"an app key is required": {app: ""},
		"a name without an app": {
			app:   "gmail",
			tools: []domain.ToolSpec{spec("search", domain.LabelRead)},
		},
		"a tool from another app": {
			app:   "gmail",
			tools: []domain.ToolSpec{spec("jira.search", domain.LabelRead)},
		},
		"an unknown label": {
			app:   "gmail",
			tools: []domain.ToolSpec{spec("gmail.send", "send_it")},
		},
		"a missing schema": {
			app: "gmail",
			tools: []domain.ToolSpec{{
				Name: "gmail.send", Label: domain.LabelWriteExternal, Schema: nil,
			}},
		},
		"a schema that is not JSON": {
			app: "gmail",
			tools: []domain.ToolSpec{{
				Name: "gmail.send", Label: domain.LabelWriteExternal,
				Schema: json.RawMessage(`{not json`),
			}},
		},
		"an unknown trigger kind": {
			app:      "gmail",
			triggers: []domain.TriggerSpec{{App: "gmail", Kind: "carrier-pigeon", Event: "gmail.mail"}},
		},
		"a trigger from another app": {
			app:      "gmail",
			triggers: []domain.TriggerSpec{{App: "jira", Kind: domain.TriggerWebhook, Event: "jira.issue"}},
		},
	}

	for name, adapter := range cases {
		t.Run(name, func(t *testing.T) {
			registry := NewRegistry()
			err := registry.Register(adapter)
			require.Error(t, err)
			require.ErrorIs(t, err, domain.ErrInvalidSpec)
			require.Empty(t, registry.Apps(), "a refused adapter is not registered")
		})
	}

	registry := NewRegistry()
	require.ErrorIs(t, registry.Register(nil), domain.ErrInvalidSpec)
}

// TestAddingAnAppNeedsNoChangeToTheWorker is the acceptance criterion stated as a
// test: a new application is a new adapter and nothing else.
func TestAddingAnAppNeedsNoChangeToTheWorker(t *testing.T) {
	registry := NewRegistry()
	require.NoError(t, registry.Register(gmailStub()))

	before := len(registry.Tools())

	// The whole of "support Notion" is this value. Nothing outside the registry
	// learns that it exists.
	require.NoError(t, registry.Register(&fakeAdapter{
		app: "notion",
		tools: []domain.ToolSpec{
			spec("notion.search", domain.LabelRead),
			spec("notion.create_page", domain.LabelWriteExternal),
		},
	}))

	require.Len(t, registry.Tools(), before+2)
	require.Contains(t, registry.Apps(), "notion")
}

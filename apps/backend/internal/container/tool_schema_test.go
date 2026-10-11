package container

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	integrationservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/service"
)

// schemaAdapter is an adapter that declares one tool with a required field, so
// the validator has something with a shape to enforce.
type schemaAdapter struct{}

func (schemaAdapter) App() string { return "gmail" }

func (schemaAdapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{{
		Name:        "gmail.send",
		Label:       integrationdomain.LabelWriteExternal,
		Description: "Kirim email",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":      {"type": "string", "minLength": 3},
				"subject": {"type": "string"}
			},
			"required": ["to"],
			"additionalProperties": false
		}`),
	}}
}

func (schemaAdapter) Triggers() []integrationdomain.TriggerSpec { return nil }

func (schemaAdapter) Execute(context.Context, integrationdomain.Call) (integrationdomain.Result, error) {
	return integrationdomain.Result{}, nil
}

// TestAToolSchemaIsEnforcedBeforeExecution is the E8.2 rule: a call whose
// arguments do not match the tool's schema is refused before anything is sent.
func TestAToolSchemaIsEnforcedBeforeExecution(t *testing.T) {
	registry := integrationservice.NewRegistry()
	require.NoError(t, registry.Register(schemaAdapter{}))

	schemas, err := newToolSchemas(registry)
	require.NoError(t, err)
	require.NotNil(t, schemas)

	schema := registry.Tools()[0].Schema

	// A matching call passes.
	require.NoError(t, schemas.Validate(schema, json.RawMessage(`{"to":"a@b.c"}`)))
	require.NoError(t, schemas.Validate(schema, json.RawMessage(`{"to":"a@b.c","subject":"Tagihan"}`)))

	// A missing required field is refused, and the message names it.
	err = schemas.Validate(schema, json.RawMessage(`{"subject":"Tagihan"}`))
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)
	require.Contains(t, err.Error(), "to")

	// A field the tool does not take is refused rather than passed to a provider.
	err = schemas.Validate(schema, json.RawMessage(`{"to":"a@b.c","cc":"x@y.z"}`))
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)

	// A wrong type is refused.
	err = schemas.Validate(schema, json.RawMessage(`{"to":42}`))
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)

	// Arguments that are not JSON at all are refused with a reason.
	err = schemas.Validate(schema, json.RawMessage(`{not json`))
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)

	// A tool called with no arguments is validated as an empty object, which is
	// what its schema then decides.
	err = schemas.Validate(schema, nil)
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)
}

// TestASchemaNobodyCompiledIsRefused keeps a tool that appeared after startup
// from being called unchecked.
func TestASchemaNobodyCompiledIsRefused(t *testing.T) {
	schemas := &toolSchemas{compiled: map[string]*jsonschema.Schema{}}

	err := schemas.Validate(json.RawMessage(`{"type":"object"}`), json.RawMessage(`{}`))
	require.ErrorIs(t, err, integrationdomain.ErrSchemaViolation)
	require.Contains(t, err.Error(), "not compiled at startup")
}

// TestABrokenSchemaIsReportedAtStartup is why compilation happens while the
// process is starting rather than on the first call.
func TestABrokenSchemaIsReportedAtStartup(t *testing.T) {
	registry := integrationservice.NewRegistry()
	require.NoError(t, registry.Register(brokenSchemaAdapter{}))

	_, err := newToolSchemas(registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "gmail.broken")
}

// brokenSchemaAdapter declares a schema that is valid JSON but not a valid
// schema.
type brokenSchemaAdapter struct{}

func (brokenSchemaAdapter) App() string { return "gmail" }

func (brokenSchemaAdapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{{
		Name:        "gmail.broken",
		Label:       integrationdomain.LabelRead,
		Description: "A tool whose schema is wrong",
		Schema:      json.RawMessage(`{"type":"object","properties":{"q":{"type":"nonsense"}}}`),
	}}
}

func (brokenSchemaAdapter) Triggers() []integrationdomain.TriggerSpec { return nil }

func (brokenSchemaAdapter) Execute(context.Context, integrationdomain.Call) (integrationdomain.Result, error) {
	return integrationdomain.Result{}, nil
}

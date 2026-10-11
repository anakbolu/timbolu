package container

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// toolSchemas validates a tool call's arguments against the schema the adapter
// declared for that tool.
//
// Schemas are compiled once, at startup, and kept: a model calls the same few
// tools over and over, and compiling a schema per call would put a parser in the
// middle of every step. They are keyed by the schema document itself, because the
// validation port is handed the schema rather than the tool's name — the adapter
// owns the name, and the bytes are what it declared.
type toolSchemas struct {
	compiled map[string]*jsonschema.Schema
}

// newToolSchemas compiles every tool schema the registry declares.
//
// A schema that will not compile is reported at startup rather than at the first
// call: an adapter with a broken schema is a bug in that adapter, and a person
// waiting on a task is the worst time to find out.
func newToolSchemas(registry integrationdomain.Registry) (*toolSchemas, error) {
	if registry == nil {
		return nil, nil
	}

	tools := registry.Tools()
	compiled := make(map[string]*jsonschema.Schema, len(tools))

	for _, tool := range tools {
		key := string(tool.Schema)
		if _, seen := compiled[key]; seen {
			// Two tools may legitimately declare the same shape, and compiling it
			// twice would only cost memory.
			continue
		}

		parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.Schema))
		if err != nil {
			return nil, fmt.Errorf("integration: parse %s schema: %w", tool.Name, err)
		}

		compiler := jsonschema.NewCompiler()
		resource := tool.Name + ".json"
		if err := compiler.AddResource(resource, parsed); err != nil {
			return nil, fmt.Errorf("integration: register %s schema: %w", tool.Name, err)
		}

		schema, err := compiler.Compile(resource)
		if err != nil {
			return nil, fmt.Errorf("integration: compile %s schema: %w", tool.Name, err)
		}
		compiled[key] = schema
	}

	return &toolSchemas{compiled: compiled}, nil
}

// Validate reports whether the arguments match the tool's schema.
//
// A schema that was never compiled is refused rather than allowed through. The
// only way to reach that is for an adapter to have been registered after startup,
// and running a call whose schema was never checked is the thing this type
// exists to prevent.
func (t *toolSchemas) Validate(schema json.RawMessage, arguments json.RawMessage) error {
	if t == nil {
		return nil
	}

	compiled, ok := t.compiled[string(schema)]
	if !ok {
		return fmt.Errorf("%w: this tool's schema was not compiled at startup",
			integrationdomain.ErrSchemaViolation)
	}

	// A tool that takes no arguments is called with none, and an empty object is
	// what its schema expects.
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}

	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	if err != nil {
		return fmt.Errorf("%w: the arguments are not valid JSON: %w",
			integrationdomain.ErrSchemaViolation, err)
	}

	if err := compiled.Validate(document); err != nil {
		// Written for the agent that produced the call, because that is who has to
		// fix it: the message names the offending path.
		return fmt.Errorf("%w: %s", integrationdomain.ErrSchemaViolation, schemaMessage(err))
	}
	return nil
}

// schemaMessage renders a validation failure as one line naming the path, which
// is what a repair loop sends back to the model.
func schemaMessage(err error) string {
	var validation *jsonschema.ValidationError
	if errors.As(err, &validation) {
		if causes := validation.Causes; len(causes) > 0 {
			parts := make([]string, 0, len(causes))
			for _, cause := range causes {
				parts = append(parts, cause.Error())
			}
			return strings.Join(parts, "; ")
		}
	}
	return err.Error()
}

// compile-time check that the validator satisfies the port it is wired to.
var _ integrationdomain.SchemaValidator = (*toolSchemas)(nil)

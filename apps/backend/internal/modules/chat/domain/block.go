package domain

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// schemaPrinter renders a validation kind as a sentence. The validator requires
// a printer rather than accepting nil, and English is the language the error is
// written for: the message is addressed to the agent that produced the block.
var schemaPrinter = message.NewPrinter(language.English)

// The block schemas. One document per block type, compiled once, and applied
// before a block is stored: an invalid chart specification therefore never
// reaches a renderer, and the agent gets the validator's own message back so it
// can correct itself.
//
// They are JSON Schema rather than hand-written Go checks because the same
// documents are what the repair loop reports on, and because a shape this
// nested reads better as data than as code.
var blockSchemas = map[string]string{
	BlockText: `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["type", "markdown"],
		"properties": {
			"type": {"const": "text"},
			"markdown": {"type": "string"},
			"title": {"type": "string"}
		},
		"additionalProperties": true
	}`,

	BlockTable: `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["type", "columns", "rows"],
		"properties": {
			"type": {"const": "table"},
			"title": {"type": "string"},
			"columns": {
				"type": "array",
				"minItems": 1,
				"maxItems": ` + itoa(TableMaxColumns) + `,
				"items": {"type": "string"}
			},
			"rows": {
				"type": "array",
				"maxItems": ` + itoa(TableMaxRows) + `,
				"items": {
					"type": "array",
					"items": {"type": "string"}
				}
			},
			"align": {
				"type": "array",
				"items": {"enum": ["left", "right", "center"]}
			}
		},
		"additionalProperties": true
	}`,

	BlockDraft: `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["type", "draft_id"],
		"properties": {
			"type": {"const": "draft"},
			"draft_id": {"type": "string", "format": "uuid"},
			"action_kind": {"type": "string"},
			"title": {"type": "string"},
			"summary": {"type": "string"},
			"status": {"enum": ["pending", "approved", "revise", "sent", "canceled"]},
			"blocked_reason": {"type": "string"},
			"not_before": {"type": "string"},
			"revision": {"type": "integer"},
			"fields": {
				"type": "array",
				"items": {
					"type": "array",
					"minItems": 2,
					"maxItems": 2,
					"items": {"type": "string"}
				}
			}
		},
		"additionalProperties": true
	}`,

	BlockChart: `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["type", "spec"],
		"properties": {
			"type": {"const": "chart"},
			"title": {"type": "string"},
			"spec": {"$ref": "#/$defs/spec"}
		},
		"$defs": {
			"spec": {
				"type": "object",
				"required": ["kind"],
				"properties": {
					"kind": {"enum": ["bar", "line", "area", "pie", "scatter"]},
					"title": {"type": "string"},
					"x_label": {"type": "string"},
					"y_label": {"type": "string"},
					"unit": {"type": "string"},
					"categories": {
						"type": "array",
						"minItems": 1,
						"maxItems": ` + itoa(ChartMaxPoints) + `,
						"items": {"type": "string"}
					},
					"series": {
						"type": "array",
						"minItems": 1,
						"maxItems": ` + itoa(ChartMaxSeries) + `,
						"items": {
							"type": "object",
							"required": ["data"],
							"properties": {
								"name": {"type": "string"},
								"data": {
									"type": "array",
									"maxItems": ` + itoa(ChartMaxPoints) + `,
									"items": {"type": "number"}
								}
							}
						}
					},
					"slices": {
						"type": "array",
						"minItems": 1,
						"maxItems": ` + itoa(ChartMaxSeries) + `,
						"items": {
							"type": "object",
							"required": ["name", "value"],
							"properties": {
								"name": {"type": "string"},
								"value": {"type": "number"}
							}
						}
					},
					"points": {
						"type": "array",
						"minItems": 1,
						"maxItems": ` + itoa(ChartMaxPoints) + `,
						"items": {
							"type": "object",
							"required": ["x", "y"],
							"properties": {
								"x": {"type": ["number", "string"]},
								"y": {"type": "number"}
							}
						}
					},
					"stacked": {"type": "boolean"}
				},
				"allOf": [
					{
						"if": {"properties": {"kind": {"enum": ["bar", "line", "area"]}}},
						"then": {
							"required": ["categories", "series"],
							"properties": {
								"series": {"items": {"required": ["name"]}}
							}
						}
					},
					{
						"if": {"properties": {"kind": {"const": "pie"}}},
						"then": {"required": ["slices"]}
					},
					{
						"if": {"properties": {"kind": {"const": "scatter"}}},
						"then": {"required": ["points"]}
					}
				]
			}
		},
		"additionalProperties": true
	}`,

	BlockMermaid: `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["type", "code"],
		"properties": {
			"type": {"const": "mermaid"},
			"title": {"type": "string"},
			"code": {"type": "string", "minLength": 1},
			"diagram": {"enum": ["erd", "flowchart", "sequence", "gantt", "state", "mindmap", "class", "pie", "other"]}
		},
		"additionalProperties": true
	}`,

	BlockHTML: `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type": "object",
		"required": ["type", "content_ref", "byte_size"],
		"properties": {
			"type": {"const": "html"},
			"title": {"type": "string"},
			"content_ref": {"type": "string", "minLength": 1},
			"byte_size": {"type": "integer", "minimum": 1, "maximum": ` + itoa(HTMLMaxBytes) + `},
			"caption": {"type": "string"}
		},
		"additionalProperties": true
	}`,
}

// compiled holds the schemas after the first validation. Compilation is a parse
// per schema, so doing it once per process is enough.
var (
	compiledOnce sync.Once
	compiled     map[string]*jsonschema.Schema
	compiledErr  error
)

func compileSchemas() {
	compiled = make(map[string]*jsonschema.Schema, len(blockSchemas))

	for name, document := range blockSchemas {
		parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(document))
		if err != nil {
			compiledErr = fmt.Errorf("chat: parse %s schema: %w", name, err)
			return
		}

		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(name+".json", parsed); err != nil {
			compiledErr = fmt.Errorf("chat: register %s schema: %w", name, err)
			return
		}

		schema, err := compiler.Compile(name + ".json")
		if err != nil {
			compiledErr = fmt.Errorf("chat: compile %s schema: %w", name, err)
			return
		}
		compiled[name] = schema
	}
}

// Schemas returns the compiled schema per block type. It is exported so a test
// can assert the set matches BlockTypes.
func Schemas() (map[string]*jsonschema.Schema, error) {
	compiledOnce.Do(compileSchemas)
	return compiled, compiledErr
}

// ValidateBlock checks one block against the schema of its type and applies the
// rules a schema cannot express.
//
// The error it returns is written for the agent that produced the block: it
// names the offending path, because that message is what the repair loop sends
// back.
func ValidateBlock(block Block) error {
	if block.Type == "" {
		return fmt.Errorf("%w: block has no type", ErrInvalidBlock)
	}
	if !knownBlockType(block.Type) {
		return fmt.Errorf("%w: unknown block type %q (known: %s)", ErrInvalidBlock, block.Type, strings.Join(BlockTypes, ", "))
	}
	if len(block.Body) == 0 {
		return fmt.Errorf("%w: %s block has an empty body", ErrInvalidBlock, block.Type)
	}

	schemas, err := Schemas()
	if err != nil {
		return err
	}
	schema, ok := schemas[block.Type]
	if !ok {
		return fmt.Errorf("%w: no schema for block type %q", ErrInvalidBlock, block.Type)
	}

	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(block.Body))
	if err != nil {
		return fmt.Errorf("%w: %s block is not valid JSON: %w", ErrInvalidBlock, block.Type, err)
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("%w: %s: %s", ErrInvalidBlock, block.Type, schemaMessage(err))
	}

	return validateCrossField(block)
}

// ValidateBlocks checks a whole message body: every block, the count limit, and
// the total size.
func ValidateBlocks(blocks []Block) error {
	if len(blocks) > BlocksPerMessage {
		return fmt.Errorf("%w: %d blocks in one message, at most %d are allowed",
			ErrInvalidBlock, len(blocks), BlocksPerMessage)
	}
	for i, block := range blocks {
		if err := ValidateBlock(block); err != nil {
			return fmt.Errorf("block %d: %w", i, err)
		}
	}
	return nil
}

// validateCrossField applies the invariants JSON Schema cannot state: that a
// series is as long as the categories it is drawn against, that a table row
// matches its columns, and that the size limits hold.
func validateCrossField(block Block) error {
	switch block.Type {
	case BlockTable:
		var table struct {
			Columns []string   `json:"columns"`
			Rows    [][]string `json:"rows"`
		}
		if err := json.Unmarshal(block.Body, &table); err != nil {
			return fmt.Errorf("%w: table: %w", ErrInvalidBlock, err)
		}
		for i, row := range table.Rows {
			if len(row) != len(table.Columns) {
				return fmt.Errorf("%w: table row %d has %d cells but there are %d columns",
					ErrInvalidBlock, i, len(row), len(table.Columns))
			}
		}

	case BlockChart:
		var chart struct {
			Spec struct {
				Kind       string   `json:"kind"`
				Categories []string `json:"categories"`
				Series     []struct {
					Name string    `json:"name"`
					Data []float64 `json:"data"`
				} `json:"series"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(block.Body, &chart); err != nil {
			return fmt.Errorf("%w: chart: %w", ErrInvalidBlock, err)
		}
		switch chart.Spec.Kind {
		case "bar", "line", "area":
			for _, series := range chart.Spec.Series {
				if len(series.Data) != len(chart.Spec.Categories) {
					return fmt.Errorf("%w: chart series %q has %d points but there are %d categories",
						ErrInvalidBlock, series.Name, len(series.Data), len(chart.Spec.Categories))
				}
			}
		}

	case BlockMermaid:
		var diagram struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(block.Body, &diagram); err != nil {
			return fmt.Errorf("%w: mermaid: %w", ErrInvalidBlock, err)
		}
		if len(diagram.Code) > MermaidMaxBytes {
			return fmt.Errorf("%w: mermaid code is %d bytes, at most %d are allowed",
				ErrBlockTooLarge, len(diagram.Code), MermaidMaxBytes)
		}

	case BlockHTML:
		var document struct {
			ContentRef string `json:"content_ref"`
		}
		if err := json.Unmarshal(block.Body, &document); err != nil {
			return fmt.Errorf("%w: html: %w", ErrInvalidBlock, err)
		}
		// The reference must be a content key, so a block can never point at an
		// attachment or at anything outside the content prefix.
		if err := ValidateContentRef(document.ContentRef); err != nil {
			return err
		}

	case BlockDraft:
		var draft struct {
			DraftID string `json:"draft_id"`
		}
		if err := json.Unmarshal(block.Body, &draft); err != nil {
			return fmt.Errorf("%w: draft: %w", ErrInvalidBlock, err)
		}
		if _, err := uuid.Parse(draft.DraftID); err != nil {
			return fmt.Errorf("%w: draft_id %q is not a uuid", ErrInvalidBlock, draft.DraftID)
		}

	case BlockText:
		var text struct {
			Markdown string `json:"markdown"`
		}
		if err := json.Unmarshal(block.Body, &text); err != nil {
			return fmt.Errorf("%w: text: %w", ErrInvalidBlock, err)
		}
		if len(text.Markdown) > TextMaxBytes {
			return fmt.Errorf("%w: text is %d bytes, at most %d are allowed",
				ErrBlockTooLarge, len(text.Markdown), TextMaxBytes)
		}
	}

	return nil
}

// ContentRefPrefix is where sandboxed documents live in object storage.
//
// They are namespaced apart from attachments because they are served from a
// different origin under a different policy: an attachment is downloaded, a
// content document is executed inside a sandboxed iframe.
const ContentRefPrefix = "content"

// EncodeContentRef turns a storage key into the opaque reference a block
// carries.
//
// The reference is the key rather than a database id, so the content origin
// resolves it with one storage read: no tenant scope, no row, and nothing to
// leak if the reference is guessed.
func EncodeContentRef(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

// DecodeContentRef reads a reference back into its storage key.
func DecodeContentRef(reference string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(reference))
	if err != nil {
		return "", fmt.Errorf("%w: content reference is not readable", ErrInvalidBlock)
	}

	key := string(decoded)
	if err := ValidateContentRef(reference); err != nil {
		return "", err
	}
	return key, nil
}

// ValidateContentRef checks that a reference names a content object and nothing
// else.
func ValidateContentRef(reference string) error {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(reference))
	if err != nil {
		return fmt.Errorf("%w: content reference is not readable", ErrInvalidBlock)
	}

	path := string(key)
	if !strings.HasPrefix(path, ContentRefPrefix+"/") {
		return fmt.Errorf("%w: content reference does not point at a content document", ErrInvalidBlock)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("%w: content reference escapes its prefix", ErrInvalidBlock)
	}
	return nil
}

// schemaMessage renders a validation failure as one line, which is what fits in
// an event payload the agent reads.
//
// It walks to the first leaf cause rather than printing the tree: the leaf names
// the offending value and the path to it, which is the part an agent can act on,
// while the tree is unreadable inside a chat message.
func schemaMessage(err error) string {
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return err.Error()
	}

	leaf := validation
	for len(leaf.Causes) > 0 {
		leaf = leaf.Causes[0]
	}

	location := "(root)"
	if len(leaf.InstanceLocation) > 0 {
		location = "/" + strings.Join(leaf.InstanceLocation, "/")
	}

	if leaf.ErrorKind == nil {
		return location
	}
	return location + ": " + leaf.ErrorKind.LocalizedString(schemaPrinter)
}

func knownBlockType(name string) bool {
	for _, known := range BlockTypes {
		if known == name {
			return true
		}
	}
	return false
}

// itoa avoids importing strconv in the schema literals above, which keeps the
// documents readable as data.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

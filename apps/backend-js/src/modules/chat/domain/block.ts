/**
 * Block validation.
 *
 * The block schemas live here, one document per block type, and every block is
 * checked before it is stored: an invalid chart specification therefore never
 * reaches a renderer, and the agent gets a message that names the offending path
 * so it can correct itself.
 *
 * Ported 1:1 from internal/modules/chat/domain/block.go. The Go side compiles
 * these documents with `santhosh-tekuri/jsonschema`; the checks below apply the
 * same constraints by hand, in the same order, because the documents are fixed
 * and a schema engine would be a dependency this port does not need. See the
 * report's gaps for the one behavioural difference (the wording of a failure).
 */

import {
  BlockChart,
  BlockDraft,
  BlockHTML,
  BlockMermaid,
  BlockTable,
  BlockText,
  BlockTypes,
  BlocksPerMessage,
  ChartMaxPoints,
  ChartMaxSeries,
  ErrBlockTooLarge,
  ErrInvalidBlock,
  HTMLMaxBytes,
  MermaidMaxBytes,
  TableMaxColumns,
  TableMaxRows,
  TextMaxBytes,
  WrappedError,
  type Block,
} from "./conversation.ts";

/** The JSON Schema documents, kept as data so the constraints stay auditable. */
export const blockSchemas: Record<string, string> = {
  [BlockText]: `{
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

  [BlockTable]: `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"required": ["type", "columns", "rows"],
	"properties": {
		"type": {"const": "table"},
		"title": {"type": "string"},
		"columns": {
			"type": "array",
			"minItems": 1,
			"maxItems": ${TableMaxColumns},
			"items": {"type": "string"}
		},
		"rows": {
			"type": "array",
			"maxItems": ${TableMaxRows},
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

  [BlockDraft]: `{
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

  [BlockChart]: `{
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
					"maxItems": ${ChartMaxPoints},
					"items": {"type": "string"}
				},
				"series": {
					"type": "array",
					"minItems": 1,
					"maxItems": ${ChartMaxSeries},
					"items": {
						"type": "object",
						"required": ["data"],
						"properties": {
							"name": {"type": "string"},
							"data": {
								"type": "array",
								"maxItems": ${ChartMaxPoints},
								"items": {"type": "number"}
							}
						}
					}
				},
				"slices": {
					"type": "array",
					"minItems": 1,
					"maxItems": ${ChartMaxSeries},
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
					"maxItems": ${ChartMaxPoints},
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

  [BlockMermaid]: `{
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

  [BlockHTML]: `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"required": ["type", "content_ref", "byte_size"],
	"properties": {
		"type": {"const": "html"},
		"title": {"type": "string"},
		"content_ref": {"type": "string", "minLength": 1},
		"byte_size": {"type": "integer", "minimum": 1, "maximum": ${HTMLMaxBytes}},
		"caption": {"type": "string"}
	},
	"additionalProperties": true
}`,
};

/** Where sandboxed documents live in object storage. */
export const ContentRefPrefix = "content";

/**
 * Turns a storage key into the opaque reference a block carries.
 *
 * The reference is the key rather than a database id, so the content origin
 * resolves it with one storage read: no tenant scope, no row, and nothing to
 * leak if the reference is guessed.
 */
export function EncodeContentRef(key: string): string {
  return Buffer.from(key, "utf8").toString("base64url");
}

/**
 * Reads a reference back into its storage key.
 *
 * The reference is a capability, so a value that is not a readable content key
 * is refused before anything is fetched.
 */
export function DecodeContentRef(reference: string): string {
  const key = decodeContentKey(reference);
  if (key === null) {
    throw new WrappedError(ErrInvalidBlock, "content reference is not readable");
  }
  ValidateContentRef(reference);
  return key;
}

/**
 * Checks that a reference names a content object and nothing else.
 *
 * This is what stops a reference from naming an attachment, or from walking out
 * of the content prefix with `..`.
 */
export function ValidateContentRef(reference: string): void {
  const key = decodeContentKey(reference);
  if (key === null) {
    throw new WrappedError(ErrInvalidBlock, "content reference is not readable");
  }
  if (!key.startsWith(ContentRefPrefix + "/")) {
    throw new WrappedError(ErrInvalidBlock, "content reference does not point at a content document");
  }
  if (key.includes("..")) {
    throw new WrappedError(ErrInvalidBlock, "content reference escapes its prefix");
  }
}

/**
 * Base64url-decodes a reference strictly, or null when it is not a readable
 * value. Node's decoder is lenient about invalid characters and padding, so the
 * shape is checked first.
 */
function decodeContentKey(reference: string): string | null {
  const trimmed = reference.trim();
  if (trimmed === "" || !/^[A-Za-z0-9_-]+$/.test(trimmed) || trimmed.length % 4 === 1) {
    return null;
  }
  return Buffer.from(trimmed, "base64url").toString("utf8");
}

/** True when the name is one of the six block types. */
function knownBlockType(name: string): boolean {
  return BlockTypes.includes(name);
}

/** Builds the error a caller sees, prefixed with the block type. */
function invalid(type: string, detail: string): WrappedError {
  return new WrappedError(ErrInvalidBlock, `${type}: ${detail}`);
}

/** A JSON object, or null when the value is not one. */
function objectOf(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

/** The JSON Schema type of a value, for the failure message. */
function typeName(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "array";
  if (Number.isInteger(value)) return "integer";
  return typeof value;
}

/**
 * Validates one block against the schema of its type and applies the rules a
 * schema cannot express.
 *
 * The error names the offending path, because that message is what the repair
 * loop sends back to the agent that produced the block.
 */
export function ValidateBlock(block: Block): void {
  if (!block.type) {
    throw new WrappedError(ErrInvalidBlock, "block has no type");
  }
  if (!knownBlockType(block.type)) {
    throw new WrappedError(
      ErrInvalidBlock,
      `unknown block type ${JSON.stringify(block.type)} (known: ${BlockTypes.join(", ")})`,
    );
  }
  if (block.body === undefined || block.body === null) {
    throw new WrappedError(ErrInvalidBlock, `${block.type} block has an empty body`);
  }

  const document = objectOf(block.body);
  if (document === null) {
    throw invalid(block.type, "block is not a JSON object");
  }

  switch (block.type) {
    case BlockText:
      validateText(document);
      break;
    case BlockTable:
      validateTable(document);
      break;
    case BlockDraft:
      validateDraft(document);
      break;
    case BlockChart:
      validateChart(document);
      break;
    case BlockMermaid:
      validateMermaid(document);
      break;
    case BlockHTML:
      validateHTML(document);
      break;
  }
}

/** Checks a whole message body: every block, the count limit, and the size. */
export function ValidateBlocks(blocks: Block[]): void {
  if (blocks.length > BlocksPerMessage) {
    throw new WrappedError(
      ErrInvalidBlock,
      `${blocks.length} blocks in one message, at most ${BlocksPerMessage} are allowed`,
    );
  }
  blocks.forEach((block, index) => {
    try {
      ValidateBlock(block);
    } catch (err) {
      // The sentinel is preserved so the handler can still answer with the
      // status the block's own failure names, and the index is prepended the
      // way Go's `%w` wrapping does.
      throw new WrappedError(err instanceof WrappedError ? err.sentinel : ErrInvalidBlock, `block ${index}: ${(err as Error).message}`);
    }
  });
}

/** Requires a string field, reporting the path when it is absent or mistyped. */
function requireString(document: Record<string, unknown>, type: string, key: string): string {
  const value = document[key];
  if (value === undefined) {
    throw invalid(type, `/${key}: required`);
  }
  if (typeof value !== "string") {
    throw invalid(type, `/${key}: expected string, got ${typeName(value)}`);
  }
  return value;
}

/** Requires an array field, reporting the path when it is absent or mistyped. */
function requireArray(document: Record<string, unknown>, type: string, key: string): unknown[] {
  const value = document[key];
  if (value === undefined) {
    throw invalid(type, `/${key}: required`);
  }
  if (!Array.isArray(value)) {
    throw invalid(type, `/${key}: expected array, got ${typeName(value)}`);
  }
  return value;
}

/** Reports a string array's length bounds and item types. */
function checkStringArray(
  type: string,
  value: unknown,
  path: string,
  minItems: number,
  maxItems: number,
): string[] {
  if (!Array.isArray(value)) {
    throw invalid(type, `${path}: expected array, got ${typeName(value)}`);
  }
  if (value.length < minItems) {
    throw invalid(type, `${path}: at least ${minItems} item(s) required`);
  }
  if (value.length > maxItems) {
    throw invalid(type, `${path}: at most ${maxItems} items are allowed`);
  }
  value.forEach((item, index) => {
    if (typeof item !== "string") {
      throw invalid(type, `${path}/${index}: expected string, got ${typeName(item)}`);
    }
  });
  return value as string[];
}

/** Checks an optional string field. */
function checkOptionalString(document: Record<string, unknown>, type: string, key: string): void {
  const value = document[key];
  if (value !== undefined && typeof value !== "string") {
    throw invalid(type, `/${key}: expected string, got ${typeName(value)}`);
  }
}

/** The text block: Markdown and an optional title. */
function validateText(document: Record<string, unknown>): void {
  const markdown = requireString(document, BlockText, "markdown");
  checkOptionalString(document, BlockText, "title");

  if (Buffer.byteLength(markdown, "utf8") > TextMaxBytes) {
    throw new WrappedError(
      ErrBlockTooLarge,
      `text is ${Buffer.byteLength(markdown, "utf8")} bytes, at most ${TextMaxBytes} are allowed`,
    );
  }
}

/** The table block: columns, rows, and the row/column agreement. */
function validateTable(document: Record<string, unknown>): void {
  checkOptionalString(document, BlockTable, "title");

  const columns = checkStringArray(
    BlockTable,
    requireArray(document, BlockTable, "columns"),
    "/columns",
    1,
    TableMaxColumns,
  );

  const rows = requireArray(document, BlockTable, "rows");
  if (rows.length > TableMaxRows) {
    throw invalid(BlockTable, `/rows: at most ${TableMaxRows} items are allowed`);
  }
  rows.forEach((row, index) => {
    checkStringArray(BlockTable, row, `/rows/${index}`, 0, Number.MAX_SAFE_INTEGER);
    if ((row as string[]).length !== columns.length) {
      throw invalid(
        BlockTable,
        `table row ${index} has ${(row as string[]).length} cells but there are ${columns.length} columns`,
      );
    }
  });

  const align = document.align;
  if (align !== undefined) {
    if (!Array.isArray(align)) {
      throw invalid(BlockTable, `/align: expected array, got ${typeName(align)}`);
    }
    align.forEach((value, index) => {
      if (value !== "left" && value !== "right" && value !== "center") {
        throw invalid(BlockTable, `/align/${index}: expected one of left, right, center`);
      }
    });
  }
}

/** The draft block: an approval card that names a draft. */
function validateDraft(document: Record<string, unknown>): void {
  const draftId = requireString(document, BlockDraft, "draft_id");
  if (!UUID_RE.test(draftId)) {
    throw invalid(BlockDraft, `draft_id ${JSON.stringify(draftId)} is not a uuid`);
  }

  checkOptionalString(document, BlockDraft, "action_kind");
  checkOptionalString(document, BlockDraft, "title");
  checkOptionalString(document, BlockDraft, "summary");
  checkOptionalString(document, BlockDraft, "blocked_reason");
  checkOptionalString(document, BlockDraft, "not_before");

  const status = document.status;
  if (status !== undefined && !DRAFT_STATUSES.includes(status as string)) {
    throw invalid(BlockDraft, "/status: expected one of pending, approved, revise, sent, canceled");
  }

  const revision = document.revision;
  if (revision !== undefined && !Number.isInteger(revision)) {
    throw invalid(BlockDraft, `/revision: expected integer, got ${typeName(revision)}`);
  }

  const fields = document.fields;
  if (fields !== undefined) {
    if (!Array.isArray(fields)) {
      throw invalid(BlockDraft, `/fields: expected array, got ${typeName(fields)}`);
    }
    fields.forEach((pair, index) => {
      checkStringArray(BlockDraft, pair, `/fields/${index}`, 2, 2);
    });
  }
}

/** The chart block: a spec the renderer draws directly. */
function validateChart(document: Record<string, unknown>): void {
  checkOptionalString(document, BlockChart, "title");

  if (document.spec === undefined) {
    throw invalid(BlockChart, "/spec: required");
  }
  const spec = objectOf(document.spec);
  if (spec === null) {
    throw invalid(BlockChart, `/spec: expected object, got ${typeName(document.spec)}`);
  }

  const kind = spec.kind;
  if (kind === undefined) {
    throw invalid(BlockChart, "/spec/kind: required");
  }
  if (!CHART_KINDS.includes(kind as string)) {
    throw invalid(BlockChart, "/spec/kind: expected one of bar, line, area, pie, scatter");
  }

  checkOptionalString(spec, BlockChart, "title");
  checkOptionalString(spec, BlockChart, "x_label");
  checkOptionalString(spec, BlockChart, "y_label");
  checkOptionalString(spec, BlockChart, "unit");
  if (spec.stacked !== undefined && typeof spec.stacked !== "boolean") {
    throw invalid(BlockChart, `/spec/stacked: expected boolean, got ${typeName(spec.stacked)}`);
  }

  const categories =
    spec.categories === undefined
      ? null
      : checkStringArray(BlockChart, spec.categories, "/spec/categories", 1, ChartMaxPoints);

  const series = spec.series === undefined ? null : checkSeries(spec.series, kind as string);
  if (spec.slices !== undefined) {
    checkSlices(spec.slices);
  }
  if (spec.points !== undefined) {
    checkPoints(spec.points);
  }

  const kindName = kind as string;
  if (kindName === "bar" || kindName === "line" || kindName === "area") {
    if (categories === null) {
      throw invalid(BlockChart, "/spec/categories: required");
    }
    if (series === null) {
      throw invalid(BlockChart, "/spec/series: required");
    }
    for (const entry of series) {
      if (categories.length !== entry.data.length) {
        throw invalid(
          BlockChart,
          `chart series ${JSON.stringify(entry.name)} has ${entry.data.length} points but there are ${categories.length} categories`,
        );
      }
    }
  }
  if (kindName === "pie" && spec.slices === undefined) {
    throw invalid(BlockChart, "/spec/slices: required");
  }
  if (kindName === "scatter" && spec.points === undefined) {
    throw invalid(BlockChart, "/spec/points: required");
  }
}

/** One chart series: a name and its points. */
function checkSeries(value: unknown, kind: string): Array<{ name: string; data: number[] }> {
  if (!Array.isArray(value)) {
    throw invalid(BlockChart, `/spec/series: expected array, got ${typeName(value)}`);
  }
  if (value.length < 1) {
    throw invalid(BlockChart, "/spec/series: at least 1 item(s) required");
  }
  if (value.length > ChartMaxSeries) {
    throw invalid(BlockChart, `/spec/series: at most ${ChartMaxSeries} items are allowed`);
  }

  const named = kind === "bar" || kind === "line" || kind === "area";
  const out: Array<{ name: string; data: number[] }> = [];

  value.forEach((entry, index) => {
    const series = objectOf(entry);
    if (series === null) {
      throw invalid(BlockChart, `/spec/series/${index}: expected object, got ${typeName(entry)}`);
    }
    if (named && series.name === undefined) {
      throw invalid(BlockChart, `/spec/series/${index}/name: required`);
    }
    if (series.name !== undefined && typeof series.name !== "string") {
      throw invalid(BlockChart, `/spec/series/${index}/name: expected string, got ${typeName(series.name)}`);
    }
    if (series.data === undefined) {
      throw invalid(BlockChart, `/spec/series/${index}/data: required`);
    }
    if (!Array.isArray(series.data)) {
      throw invalid(BlockChart, `/spec/series/${index}/data: expected array, got ${typeName(series.data)}`);
    }
    if (series.data.length > ChartMaxPoints) {
      throw invalid(BlockChart, `/spec/series/${index}/data: at most ${ChartMaxPoints} items are allowed`);
    }
    series.data.forEach((point, pointIndex) => {
      if (typeof point !== "number" || !Number.isFinite(point)) {
        throw invalid(
          BlockChart,
          `/spec/series/${index}/data/${pointIndex}: expected number, got ${typeName(point)}`,
        );
      }
    });

    out.push({ name: (series.name as string) ?? "", data: series.data as number[] });
  });

  return out;
}

/** The slices of a pie chart. */
function checkSlices(value: unknown): void {
  if (!Array.isArray(value)) {
    throw invalid(BlockChart, `/spec/slices: expected array, got ${typeName(value)}`);
  }
  if (value.length < 1) {
    throw invalid(BlockChart, "/spec/slices: at least 1 item(s) required");
  }
  if (value.length > ChartMaxSeries) {
    throw invalid(BlockChart, `/spec/slices: at most ${ChartMaxSeries} items are allowed`);
  }
  value.forEach((entry, index) => {
    const slice = objectOf(entry);
    if (slice === null) {
      throw invalid(BlockChart, `/spec/slices/${index}: expected object, got ${typeName(entry)}`);
    }
    if (typeof slice.name !== "string") {
      throw invalid(BlockChart, `/spec/slices/${index}/name: expected string`);
    }
    if (typeof slice.value !== "number" || !Number.isFinite(slice.value)) {
      throw invalid(BlockChart, `/spec/slices/${index}/value: expected number`);
    }
  });
}

/** The points of a scatter plot. */
function checkPoints(value: unknown): void {
  if (!Array.isArray(value)) {
    throw invalid(BlockChart, `/spec/points: expected array, got ${typeName(value)}`);
  }
  if (value.length < 1) {
    throw invalid(BlockChart, "/spec/points: at least 1 item(s) required");
  }
  if (value.length > ChartMaxPoints) {
    throw invalid(BlockChart, `/spec/points: at most ${ChartMaxPoints} items are allowed`);
  }
  value.forEach((entry, index) => {
    const point = objectOf(entry);
    if (point === null) {
      throw invalid(BlockChart, `/spec/points/${index}: expected object, got ${typeName(entry)}`);
    }
    if (typeof point.x !== "number" && typeof point.x !== "string") {
      throw invalid(BlockChart, `/spec/points/${index}/x: expected number or string`);
    }
    if (typeof point.y !== "number" || !Number.isFinite(point.y)) {
      throw invalid(BlockChart, `/spec/points/${index}/y: expected number`);
    }
  });
}

/** The Mermaid block: source a renderer turns into a diagram. */
function validateMermaid(document: Record<string, unknown>): void {
  checkOptionalString(document, BlockMermaid, "title");
  const code = requireString(document, BlockMermaid, "code");
  if (code.length === 0) {
    throw invalid(BlockMermaid, "/code: at least 1 character required");
  }

  const diagram = document.diagram;
  if (diagram !== undefined && !MERMAID_DIAGRAMS.includes(diagram as string)) {
    throw invalid(
      BlockMermaid,
      "/diagram: expected one of erd, flowchart, sequence, gantt, state, mindmap, class, pie, other",
    );
  }

  const size = Buffer.byteLength(code, "utf8");
  if (size > MermaidMaxBytes) {
    throw new WrappedError(
      ErrBlockTooLarge,
      `mermaid code is ${size} bytes, at most ${MermaidMaxBytes} are allowed`,
    );
  }
}

/** The HTML block: a reference to a document the content origin serves. */
function validateHTML(document: Record<string, unknown>): void {
  checkOptionalString(document, BlockHTML, "title");
  checkOptionalString(document, BlockHTML, "caption");

  const reference = requireString(document, BlockHTML, "content_ref");
  if (reference.length === 0) {
    throw invalid(BlockHTML, "/content_ref: at least 1 character required");
  }

  const byteSize = document.byte_size;
  if (byteSize === undefined) {
    throw invalid(BlockHTML, "/byte_size: required");
  }
  if (!Number.isInteger(byteSize)) {
    throw invalid(BlockHTML, `/byte_size: expected integer, got ${typeName(byteSize)}`);
  }
  if ((byteSize as number) < 1 || (byteSize as number) > HTMLMaxBytes) {
    throw invalid(BlockHTML, `/byte_size: must be between 1 and ${HTMLMaxBytes}`);
  }

  // The reference must be a content key, so a block can never point at an
  // attachment or at anything outside the content prefix.
  ValidateContentRef(reference);
}

const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;
const DRAFT_STATUSES = ["pending", "approved", "revise", "sent", "canceled"];
const CHART_KINDS = ["bar", "line", "area", "pie", "scatter"];
const MERMAID_DIAGRAMS = [
  "erd",
  "flowchart",
  "sequence",
  "gantt",
  "state",
  "mindmap",
  "class",
  "pie",
  "other",
];

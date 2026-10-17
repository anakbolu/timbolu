# Porting contract — Go → Bun/TypeScript

This directory is a **1:1 port** of `apps/backend` (Go) and the parts of
`packages/` it imports. The goal is a drop-in replacement: the existing
Next.js frontend (`apps/frontend`) and the WhatsApp gateway must keep working
without a single change on their side.

Read this file completely before writing code.

---

## 1. Non-negotiable rules

1. **Mirror the Go tree.** One TypeScript file per Go file, same relative path,
   same name in camelCase or the original name:
   - `internal/modules/chat/service/messages.go` → `src/modules/chat/service/messages.ts`
   - `internal/modules/chat/domain/block.go` → `src/modules/chat/domain/block.ts`
   - `internal/platform/llm/openai/client.go` → `src/platform/llm/openai/client.ts`

   Do **not** merge two Go files into one TS file, and do not invent files the Go
   side does not have. If a Go file is only types, the TS file is only types.

2. **Skip `*_test.go`.** Tests are out of scope for this rewrite. Never port a
   `_test.go` file. Never invent a `.test.ts`.

3. **JSON field names are frozen.** Every Go struct has `json:"..."` tags. Those
   strings are the wire format the frontend already reads. Copy them exactly —
   `snake_case`, `omitempty` behaviour included. When a Go field has no tag, Go
   emits the Go field name (`PascalCase`); reproduce that exactly.

   ```
   Go:   EmailVerified bool `json:"email_verified"`
   TS:   email_verified: boolean
   ```

4. **Status codes, messages and error codes are frozen.** The frontend branches
   on `error.code` and shows `message`. Copy the literal strings from the Go
   handler. Indonesian messages stay Indonesian.

5. **Never guess.** If the Go source is ambiguous, read the migration SQL
   (`src/migrations/*.up.sql`) or the frontend's TypeScript types in
   `apps/frontend/src/lib/api.ts`. If still ambiguous, write a `TODO(port):`
   comment naming exactly what is unclear — never invent behaviour.

6. **Do not touch other modules.** You own the files you were assigned. Do not
   edit `src/platform/*`, `src/container/*`, `src/cmd/*`, `package.json`, or
   another module's directory. If you need a change there, write it in your
   report instead.

7. **No `any` where the Go type is known.** Use the real shape. `any` is only
   acceptable for genuinely dynamic JSON (LLM payloads, tool arguments).

8. **Do not add dependencies.** Everything needed is already installed. If a Go
   dependency has no JS equivalent, port the small piece of behaviour yourself
   (this is expected for e.g. `ledongthuc/pdf`, `go-qrcode`, Duitku signing).

---

## 2. The stack, and what to use for what

| Go | TypeScript |
| --- | --- |
| Fiber v2 router | **Hono** (`hono`, `hono/bun`) |
| `fiber.Ctx` | `AppContext` from `src/platform/context.ts` |
| `pgx` / `sqlc` | **postgres.js** tagged templates |
| `go-redis` | **ioredis** |
| `golang-jwt` | **jose** (already wrapped in `src/platform/authn`) |
| `golang.org/x/crypto/argon2` | **@node-rs/argon2** (wrapped in `src/platform/authn`) |
| `zap` | **pino** (`src/platform/logger.ts`) |
| `Temporal Go SDK` | **@temporalio/**`client`/`worker`/`workflow`/`activity` |
| `minio-go` | **minio** |
| `santhosh-tekuri/jsonschema` | **zod** (hand-written schemas) |
| `google/uuid` | `crypto.randomUUID()` |
| `gofiber/contrib/websocket` | `hono/bun` `upgradeWebSocket` |

### Already built — import, never re-implement

| Import | Provides |
| --- | --- |
| `src/platform/response.ts` | `ok`, `created`, `success`, `successWithMeta`, `okWithMeta`, `error`, `unavailable` |
| `src/platform/errors.ts` | `AppError`, `badRequest`, `unauthorized`, `forbidden`, `notFound`, `conflict`, `unprocessable`, `tooManyRequests`, `notConfigured`, `internal` |
| `src/platform/authn/index.ts` | `hashPassword`, `verifyPassword`, `generateToken`, `hashToken`, `safeEqual`, `Issuer`, `Claims` |
| `src/platform/database/index.ts` | `Database` (`inScope`, `inScopeRead`, `ping`), `setWorkspace`, `Scope`, `Sql`, `Tx` |
| `src/platform/logger.ts` | `Logger`, `createLogger`, `redactQuery`, `redactHeaders` |
| `src/platform/context.ts` | `AppEnv`, `AppContext`, `currentUserId`, `currentEmail`, `role`, `scope`, `workspaceUUID`, `logger`, `requestId`, `traceId`, `isUuid` |

---

## 3. Handler pattern

A Go handler is a struct with methods that take `*fiber.Ctx`. The TS port is a
factory returning a Hono sub-app, plus a `routes()` function that mounts it —
mirroring the Go `Routes(group, handler)` shape.

```ts
// src/modules/order/handler/handler.ts
import { Hono } from "hono";
import type { AppEnv, AppContext } from "../../../platform/context.ts";
import { scope, currentUserId } from "../../../platform/context.ts";
import { ok, created } from "../../../platform/response.ts";
import { notFound } from "../../../platform/errors.ts";
import type { OrderService } from "../service/service.ts";

export class Handler {
  constructor(private readonly service: OrderService, private readonly log: Logger) {}
  // one method per Go method, same name in camelCase
  list = async (c: AppContext) => { ... }
}

/** Mirrors Go `func Routes(group fiber.Router, h *Handler)`. */
export function routes(group: Hono<AppEnv>, h: Handler): void {
  group.get("/orders", h.list);
  group.post("/orders/:orderID/cancel", h.cancel);
}
```

**Reading a request body.** Fiber's `c.BodyParser(&req)` is:

```ts
const body = await c.req.json().catch(() => null);
if (!body) throw badRequest("invalid request body", "invalid_body");
```

Validate with zod where the Go side validates, and return the same message and
code when it fails.

**Reading a path parameter.** `c.Params("orderID")` → `c.req.param("orderID")`.
Validate UUIDs with `isUuid` before using them; the Go handlers parse and return
400 on a malformed id — match the message.

**Reading a query parameter.** `c.Query("days", "30")` → `c.req.query("days") ?? "30"`.

**Writing a response.** `response.OK(c, "msg", data)` → `return ok(c, "msg", data)`.

**A service error.** Go returns `err` and the container's error handler renders
it. Here, throw `AppError`; the global handler renders the same envelope.

---

## 4. Repository pattern

sqlc generated a method per named query. Port each to a function that takes a
`Tx` (or `Sql` when the Go query is not scoped) and uses a tagged template.
Keep the SQL text identical — the column aliases in the query are what the Go
scan reads, and the JSON the handler emits is built from those columns.

```go
// name: GetOrder
SELECT id, workspace_id, status, total_micros FROM orders WHERE id = $1
```
```ts
export async function getOrder(tx: Tx, id: string): Promise<OrderRow | null> {
  const rows = await tx<OrderRow[]>`
    SELECT id, workspace_id, status, total_micros FROM orders WHERE id = ${id}
  `;
  return rows[0] ?? null;
}
```

Rules:
- `$1` → `${param}`. Never string-interpolate into SQL.
- A query that can return nothing returns `T | null`, not `T | undefined`.
- Dynamic filters (the Go code builds these with `strings.Builder` or squirrel)
  → postgres.js `sql(...)` fragment composition. See `sql` helper docs; use
  `tx.unsafe()` only when the fragment list is empty.
- `pgx.ErrNoRows` → `null`.
- `ON CONFLICT ... RETURNING` works the same; read `rows[0]`.

**Scoping.** A repository function that the Go service calls through
`pool.InScope(...)` receives the `Tx` the caller opened. The service opens it:

```ts
return this.db.inScope(scope, async (tx) => {
  const order = await getOrder(tx, id);
  ...
});
```

---

## 5. Types: what maps to what

| Go | TS |
| --- | --- |
| `uuid.UUID` | `string` |
| `time.Time` | `Date` |
| `*time.Time` | `Date \| null` |
| `time.Duration` | `number` (milliseconds) |
| `int`, `int64` | `number` |
| `float64` | `number` |
| `[]T` | `T[]` |
| `map[string]T` | `Record<string, T>` |
| `T` (pointer, nilable) | `T \| null` |
| `interface{}` | `unknown` (narrow before use) |
| `json.RawMessage` | `unknown` |

**Serialising a `time.Time`.** Go marshals it as RFC3339. A `Date` serialises
to ISO-8601 through `JSON.stringify`, which the frontend parses identically.
When a Go field is a `time.Time` in a response struct, keep the `Date` and let
JSON.stringify handle it. When the Go code formats a time into a *string* field
(`Format(time.RFC3339)`), use `.toISOString()`.

**`omitempty`.** Go drops a zero value. In TS, a field typed `T | null` must be
omitted (not sent as `null`) when the Go tag says `omitempty`. Build the response
object conditionally:

```ts
const out: any = { id: row.id, status: row.status };
if (row.completed_at) out.completed_at = row.completed_at;
```

---

## 6. Temporal

Go workflow code is deterministic and must stay that way in TS:

- `workflow.Now(ctx)` → `workflow.Now()` from `@temporalio/workflow`
- `workflow.Sleep(ctx, d)` → `sleep(d)`
- `workflow.ExecuteActivity(ctx, act, args)` → `proxyActivities<typeof activities>({...})`
- `workflow.SideEffect` / `workflow.NewTimer` → the TS equivalents
- **No I/O in workflow code** — no `fetch`, no `sql`, no `Date.now()`, no
  `Math.random()`. Everything side-effecting belongs in an activity.
- Workflow and activity names must match the Go registration names exactly:
  a running deployment's history replays by name.

`activity.Info` → `Context.current().info`. Heartbeats → `Context.current().heartbeat()`.

---

## 7. Report format (required)

When you finish, print a manifest. One line per Go file you ported:

```
<go path> -> <ts path> | ported
<go path> | skipped: test file
```

Then a section `## Gaps` listing anything you could not port faithfully, each
with the reason. If there are none, write `none`. Be honest here — a gap that is
reported is a gap I can fix; a gap that is hidden is a bug in production.

Finally run `bun run typecheck` in `apps/backend-js` and paste the result. Your
work is not done while it fails on a file you wrote.

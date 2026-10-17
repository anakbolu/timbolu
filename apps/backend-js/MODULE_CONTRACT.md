# Module porting contract

Read `CONTRACT.md` first — it governs everything. This file adds the rules that
only apply to `src/modules/**`.

---

## 1. What each Go directory becomes

| Go | TS | Notes |
| --- | --- | --- |
| `modules/<m>/domain/*.go` | `src/modules/<m>/domain/*.ts` | one file per Go file, types + interfaces only |
| `modules/<m>/domain/ports.go` | `src/modules/<m>/domain/ports.ts` | the interfaces the service depends on |
| `modules/<m>/domain/service.go` | `src/modules/<m>/domain/service.ts` | the service interface the handler calls |
| `modules/<m>/domain/mocks/*.go` | **skip** | gomock test support. Never port, always list as `skipped: generated mock`. |
| `modules/<m>/repository/postgres.go` | `src/modules/<m>/repository/postgres.ts` | the repository implementation |
| `modules/<m>/repository/convert.go` | `src/modules/<m>/repository/convert.ts` | row → domain mapping |
| `modules/<m>/repository/sqlcgen/*.go` | **folded** into `repository/postgres.ts` | sqlc generated Go from the SQL. In TS the query *is* the function; keep the SQL text and the query name as a comment above each function. List as `folded`. |
| `modules/<m>/service/*.go` | `src/modules/<m>/service/*.ts` | one file per Go file |
| `modules/<m>/handler/*.go` | `src/modules/<m>/handler/*.ts` | one file per Go file |
| `modules/<m>/adapter/*.go` | `src/modules/<m>/adapter/*.ts` | a cross-module adapter (this module implementing another module's port) |
| `modules/<m>/activity/*.go` | `src/modules/<m>/activity/*.ts` | Temporal activities |
| `modules/<m>/workflow/*.go` | `src/modules/<m>/workflow/*.ts` | Temporal workflows |
| `modules/<m>/temporal/*.go` | `src/modules/<m>/temporal/*.ts` | |

## 2. Cross-module imports

The Go architecture rule is: *a module may only import another module's
`domain`*. Enforce the same in TS — never import another module's `service`,
`repository`, or `handler`. The cross-module need is satisfied by an adapter
(`modules/<m>/adapter/*.ts`) that lives in the module which *owns* the port.

## 3. Wiring is not yours

Do **not** write `src/container/**` or `src/cmd/**`. Your job is that the module
compiles and its exports are correct. The wiring agent mounts your routes.

Expose exactly these from each module so wiring is mechanical:

```ts
// src/modules/<m>/handler/handler.ts
export class Handler { constructor(service: Service, log: Logger) {...} }
export function routes(group: Hono<AppEnv>, h: Handler): void {...}
// plus the extra group functions the Go handler file declares:
//   protectedRoutes, tenantRoutes, managerRoutes, ownerRoutes,
//   callbackRoutes, webhookRoutes, streamRoutes, contentRoutes,
//   catalogRoutes, verifiedRoutes, userRoutes, linkStreamRoutes
// Each mirrors the Go function of the same name, lower-cased first letter.
```

```ts
// src/modules/<m>/service/service.ts
export class Service { constructor(deps...) {...} }   // the concrete service
export interface ServicePort { ... }                  // what the handler calls
```

## 4. Repositories take a `Tx` or the `Database`

```ts
export class PostgresRepository implements Repository {
  constructor(private readonly db: Database) {}
  async get(scope: Scope, id: string): Promise<Order | null> {
    return this.db.inScopeRead(scope, async (tx) => {
      const rows = await tx<OrderRow[]>`SELECT ... WHERE id = ${id}`;
      return rows[0] ? toOrder(rows[0]) : null;
    });
  }
}
```

A repository method that the Go code calls *inside* an existing transaction takes
`tx: Tx` instead of opening one. Read the Go service to see which it is — do not
guess.

## 5. Handlers: exact response parity

For every handler method, the Go source ends in one of:

```go
return response.OK(c, "message", data)
return response.Created(c, "message", data)
return response.Success(c, 200, "message", data)
return response.Error(c, 400, "message", "code", details)
return response.Unavailable(c, "message", "code")
```

Map to `ok` / `created` / `success` / `error` / `unavailable` from
`src/platform/response.ts`, keeping the **message string and the code string
verbatim**. These strings are the frontend's contract and are frequently
Indonesian — copy them exactly, including capitalisation and punctuation.

When the Go handler returns a bare `fiber.NewError(404, "not found")`, throw
`notFound("not found")`; the envelope will carry `code: "http_error"` exactly as
the Go error handler does.

## 6. Temporal: names are frozen

Every workflow and activity is registered by name. Copy the Go registration
strings exactly — a mismatch means a running deployment's history cannot replay
and in-flight tasks are lost. In your report, list every workflow type name and
activity name you registered.

Workflow code must be deterministic: no `Date.now()`, no `Math.random()`, no
`fetch`, no SQL. Use `workflow.Now()`, `workflow.Sleep()`, `proxyActivities()`.

## 7. Report format

Same as `CONTRACT.md` section 7, plus:

```
## Exports
<module> handler: <comma-separated exported functions>
<module> service: <class + interface names>
## Workflow/activity names
<name> — <workflow|activity> — <file>
```

# API contract — identity and tenancy (EPIC 2)

Base URL: `{API_URL}/api`. Every response uses the envelope
`{"success":bool,"message":string,"data":…,"error":{…}}`.

## Authentication

| Endpoint | Auth | Body | Success | Errors |
| --- | --- | --- | --- | --- |
| `POST /auth/register` | — | `{email, password}` | `201` account | `400` invalid input / weak password, `409` email taken |
| `POST /auth/verify-email` | — | `{token}` | `200` account | `400` token invalid, expired, or used |
| `POST /auth/resend-verification` | — | `{email}` | `200` always | — |
| `POST /auth/login` | — | `{email, password}` | `200` session | `401` bad credentials, `429` too many attempts |
| `POST /auth/refresh` | cookie | `{token}` optional | `200` session | `400` token invalid or revoked |
| `POST /auth/logout` | cookie | — | `200` always | — |
| `POST /auth/password/forgot` | — | `{email}` | `200` always | — |
| `POST /auth/password/reset` | — | `{token, password}` | `200` | `400` token invalid / weak password |
| `GET /auth/google/start` | — | — | `302` to Google | `503` not configured |
| `GET /auth/google/callback` | — | `?code&state` | `302` to `{FRONTEND_URL}/login\|onboarding` | redirects with `?google_error=1` |
| `GET /auth/me` | bearer | — | `200` account | `401` |

**Session payload**

```json
{
  "access_token": "…",
  "access_expires_at": "2026-10-09T12:15:00Z",
  "refresh_expires_at": "2026-11-08T12:00:00Z",
  "user": {
    "id": "…", "email": "owner@example.com",
    "email_verified": true, "onboarded": true,
    "has_password": true, "has_google": false
  }
}
```

The refresh token is **not** in the body: it is set as an `httpOnly` cookie named
`bolu_refresh`, scoped to `/api/auth`, `SameSite=Lax`, `Secure` outside local
development.

Two endpoints answer identically whether or not the address exists
(`resend-verification`, `password/forgot`), so they cannot be used to discover
accounts.

## Tenancy

`X-Workspace-Id` (or `?workspace_id=`) selects the active workspace. The value
must be a UUID, and the caller must be a member, otherwise the answer is `403`.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /workspaces` | bearer | any | Every workspace the account belongs to, with its role |
| `POST /workspaces/onboard` | bearer + verified | any | Creates workspace + owner + Tim Bolu + Tim Hore; **idempotent** |
| `POST /invitations/accept` | bearer + verified | any | Joins the invited workspace; the signed-in email must match |
| `GET /workspaces/current` | bearer + tenant | any | The active workspace |
| `PATCH /workspaces/current` | bearer + tenant | owner | Name, business field, timezone |
| `GET /workspaces/current/teams` | bearer + tenant | any | Tim Bolu and Tim Hore |
| `GET /workspaces/current/members` | bearer + tenant | any | Members with roles |
| `PATCH /workspaces/current/members/:userID` | bearer + tenant | owner, admin | Change a role |
| `DELETE /workspaces/current/members/:userID` | bearer + tenant | owner, admin | Remove a member |
| `GET /workspaces/current/invitations` | bearer + tenant | any | Pending invitations |
| `POST /workspaces/current/invitations` | bearer + tenant | owner, admin | `{email, role}` |
| `DELETE /workspaces/current/invitations/:invitationID` | bearer + tenant | owner, admin | Revoke |

## Role matrix

| Action | member | admin | owner |
| --- | --- | --- | --- |
| Read workspace, teams, members, invitations | ✅ | ✅ | ✅ |
| Invite a member (role `member`/`admin`) | ❌ | ✅ | ✅ |
| Invite an owner | ❌ | ❌ | ✅ |
| Change a role / remove a member | ❌ | ✅ (not an owner) | ✅ |
| Grant or revoke the owner role | ❌ | ❌ | ✅ |
| Change workspace settings | ❌ | ❌ | ✅ |
| Remove the last owner | ❌ | ❌ | ❌ |
| Remove yourself | ❌ | ❌ | ❌ |

## Agent registry (EPIC 3)

Every workspace starts with the six Bolu copied from the seeded templates, in
**Tim Bolu**. Tim Hore starts empty and is filled by the user.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /teams` | bearer + tenant | any | Tim Bolu and Tim Hore with their Bolu |
| `GET /agents` | bearer + tenant | any | Flat registry list |
| `GET /agents/templates` | bearer | any | The six seeded profiles |
| `GET /agents/tool-catalog` | bearer | any | Every known tool with its label |
| `GET /agents/integrations` | bearer + tenant | any | The workspace's connected apps |
| `GET /agents/:id` | bearer + tenant | any | One Bolu with its derived status and the reason |
| `POST /agents` | bearer + tenant | owner, admin | `{team_id, name?, template_key?, copy_from?, persona?, role?}` |
| `PATCH /agents/:id` | bearer + tenant | owner, admin | Profile: name, role, persona, tone, shape, color, tools, default_model |
| `PATCH /agents/:id/status` | bearer + tenant | owner, admin | `{status: "active" \| "resting"}` |
| `DELETE /agents/:id` | bearer + tenant | owner, admin | Soft delete; refused while the Bolu has a running task |
| `GET /agents/:id/grants` | bearer + tenant | any | The integrations this Bolu may use |
| `PUT /agents/:id/grants` | bearer + tenant | owner, admin | `{integration_id, permission: "read" \| "read_write"}` |
| `DELETE /agents/:id/grants/:integrationID` | bearer + tenant | owner, admin | Revoke |
| `GET /agents/:id/tools` | bearer + tenant | any | The effective tool list |

### Display status is derived, never stored

Only the rest switch is a column (`agents.status`: `active` | `resting`). The
status the UI shows is computed from the tasks and drafts that reference the
Bolu:

| Display | Condition |
| --- | --- |
| `working` | at least one task is `queued` or `running` |
| `waiting` | no task in flight, but a draft is `pending` |
| `idle` | nothing in flight, nothing waiting, switch on |
| `resting` | the switch is off (outranks everything else) |

A resting Bolu never accepts new tasks; the ones already running are left to
finish.

### Tool labels decide approval

`tool_catalog` labels every tool `read`, `write_internal`, or `write_external`.
The effective tool list is the intersection of what the Bolu asks for and the
integrations it was granted:

- no grant → no tools at all for that integration;
- a `read` grant → only the tools labelled `read`;
- a `read_write` grant → every tool of that integration.

Revoking a grant removes the tools on the next read, which is what makes the
guardrail take effect on the next task.

## Model gateway (EPIC 4)

A workspace stores its providers with a fallback order; a Bolu may pin one,
bring its own endpoint, or override only the model. The effective chain is
resolved per request: **Bolu override → workspace chain → platform default**,
where the platform default is used only when the workspace configured nothing.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /llm/providers` | bearer + tenant | any | Providers in fallback order, redacted |
| `GET /llm/adapters` | bearer | any | The wire formats this deployment can serve |
| `POST /llm/providers` | bearer + tenant | owner, admin | `{name, adapter, base_url?, model, api_key?, priority?, max_tokens?, context_tokens?, is_default?, enabled?}` |
| `PUT /llm/providers/:id` | bearer + tenant | owner, admin | Same body; an empty `api_key` keeps the stored secret, `clear_api_key: true` removes it |
| `DELETE /llm/providers/:id` | bearer + tenant | owner, admin | Removes it from the chain |
| `POST /llm/providers/:id/test` | bearer + tenant | owner, admin | Calls the provider once and returns its capabilities; the call is recorded |
| `GET /llm/usage?days=30` | bearer + tenant | any | Per-day totals: calls, tokens, cache tokens, cost (micro-rupiah) |
| `GET /agents/:id/model` | bearer + tenant | any | The Bolu's override, never the key itself |
| `PUT /agents/:id/model` | bearer + tenant | owner, admin | `{provider_id?, adapter?, base_url?, model?, api_key?, max_tokens?}` |

What the gateway guarantees:

- **The secret never travels back.** A response says `has_api_key`, never the
  key. It is sealed with AES-256-GCM before it reaches the database, so a dump
  does not leak credentials.
- **Fallback is narrow.** A rate limit, an outage, a provider-side quota, or a
  model that cannot serve the request moves to the next provider. A malformed
  request returns at once. Streaming falls back only before the first byte.
- **No call is unrecorded.** One call writes exactly one `usage_ledger` row
  naming the provider and model that answered. A cost that cannot be written
  returns `usage_not_recorded` and is not retryable, because retrying would bill
  the call twice.
- **The quota is checked before the provider is called.** The live counter is
  Redis; when Redis is unavailable the check sums the ledger, so an outage cannot
  hand out a second allowance.

## Conversations (EPIC 5)

A message body is a list of typed blocks, not a string. Six types exist —
`text`, `table`, `draft`, `chart`, `mermaid`, `html` — and each is validated
against a JSON Schema before it is stored, so a renderer never receives a shape
the backend did not check.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /conversations` | bearer + tenant | any | Threads, most recently active first |
| `POST /conversations/direct` | bearer + tenant | any | `{agent_id}`; returns the 1:1 thread, creating it once |
| `POST /conversations/groups` | bearer + tenant | owner, admin | `{title, agent_ids, user_ids?}` |
| `GET /conversations/:id` | bearer + tenant | any | One thread with its participants |
| `POST /conversations/:id/participants` | bearer + tenant | owner, admin | `{agent_ids?, user_ids?}`; groups only |
| `GET /conversations/:id/messages` | bearer + tenant | any | `?cursor=&limit=`; newest first, cursor pagination |
| `POST /conversations/:id/messages` | bearer + tenant | any | `{text, reply?, agent_id?, attachment_ids?}` |
| `POST /conversations/:id/attachments` | bearer + tenant | any | multipart `file`, optional `message_id` |
| `GET /attachments/:id` | bearer + tenant | any | The bytes, served inline with `nosniff` |
| `GET /events` | bearer + tenant | any | `?after_id=&limit=`; the replay a reconnect makes |
| `GET /events/stream` | bearer + tenant (query token) | any | Server-Sent Events; `Last-Event-ID` resumes |
| `GET /events/socket` | bearer + tenant (query token) | any | WebSocket; the primary transport |
| `GET /content/:reference` | none | — | One sandboxed document, from the content origin |

A message with `reply=true` becomes a durable task (see *Task runtime*): the chat
module writes the placeholder the answer fills, opens the task, and records the
task id on that message, so a client follows the answer from the message rather
than watching the whole thread. A dispatch that cannot be opened marks the
placeholder `failed` instead of leaving it streaming forever.

### The block contract

| Type | Required | Notes |
| --- | --- | --- |
| `text` | `markdown` | Rendered without raw HTML |
| `table` | `columns`, `rows` | A row must match the column count |
| `draft` | `draft_id` | A UUID; the card shows the status and links to the dashboard |
| `chart` | `spec.kind` | `bar`/`line`/`area` need `categories` and `series`; `pie` needs `slices`; `scatter` needs `points`. A series must be as long as its categories |
| `mermaid` | `code` | Validated in the frontend with `mermaid.parse()`; a failure drives the repair loop |
| `html` | `content_ref`, `byte_size` | The reference names a content object; the size is capped at 500 KB |

An id that is absent is an **empty string** on the wire, never
`00000000-0000-0000-0000-000000000000`: `encoding/json` cannot omit a
`uuid.UUID`, so the API renders ids as strings and a client reads an empty one as
"no author".

Limits: 64 blocks per message, 40 table columns, 2000 rows, 12 chart series, 2000
points, 256 KB of text, 64 KB of Mermaid, 500 KB of HTML, 25 MB per attachment,
8000 characters per typed message, 12 participants per group.

### The stream

An event is written to `activity_events` **before** it is published to the Redis
channel `ws:{workspace_id}`, so the row is the record and the channel is the fast
path. Types: `message.new`, `message.updated`, `task.started`, `task.step`,
`task.running`, `task.waiting_approval`, `task.finished`, `draft.created`,
`draft.decided`, `agent.state`, `router.decision`, `routine.run`,
`notification.pending`.

A reconnecting client names the last event it saw and receives exactly what it
missed, in order: over HTTP with `?after_id=`, or on the stream with the
`Last-Event-ID` header (which a browser's EventSource sends by itself) or
`?last_event_id=`. An event id that appears twice is dropped by the client's
cursor, so a reconnect cannot duplicate a message.

`agent.state` carries `{agent_id, state, reason?, task_id?, draft_id?}` where
`state` is `working`, `thinking`, `waiting`, `idle`, or `resting`. **No
coordinate is stored anywhere**: the office view computes a position from the
state.

### The content origin

`GET /content/:reference` serves one agent-written HTML document. The reference
is the storage key, base64url encoded, so it is opaque and resolves in one
storage read with no database lookup and no session. The response carries:

- `Content-Security-Policy: default-src 'none'; script-src 'unsafe-inline' …;
  connect-src 'none'; form-action 'none'; frame-ancestors <app origin>;
  base-uri 'none'`

`frame-ancestors` names the application's own origin (`CONTENT_FRAME_ANCESTORS`,
defaulting to `FRONTEND_URL`). It cannot be `'self'`: the content origin is a
different host by design, so a self-only policy would refuse the frame.
- `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`

The client frames it with `sandbox="allow-scripts"` and no `allow-same-origin`,
so the document has an opaque origin and cannot read the application's cookies,
storage, or DOM. `connect-src 'none'` is what stops a script inside it from
reaching the network at all.

## Task runtime (EPIC 6)

A task is a durable unit of work: a Bolu thinks, calls the tools it was granted,
and records every round. It runs as a Temporal workflow, so it survives a worker
restart, waits for an approval for hours without holding a connection, and stops
on a bound with a reason a person can read.

```
POST /api/tasks            — not exposed: a task is opened by chat, a routine,
                             a webhook, or another task, never by a browser
GET  /api/tasks            — the workspace history, newest first
GET  /api/tasks/:id        — one task
GET  /api/tasks/:id/steps  — the recorded rounds, in order
GET  /api/tasks/:id/children — the tasks this one handed work to
POST /api/tasks/:id/cancel — stop a task that has not finished
```

All of them need a bearer token and an active workspace; the routes answer 503
with `task_not_configured` when the runtime is not wired.

`GET /api/tasks` filters: `status`, `live=true` (queued, running, or waiting),
`agent_id`, `parent_task_id`, `limit` (default 50, max 200). An unknown status is
refused with `invalid_input` rather than ignored.

A task payload carries `trigger` (`chat`, `routine`, `webhook`, `handoff`),
`status`, and a derived `health`:

| `health` | Stored status | Meaning |
| --- | --- | --- |
| `running` | `queued`, `running` | Working, and its heartbeat is fresh |
| `waiting` | `waiting_approval` | Parked on a human decision |
| `stuck` | `queued`, `running` | No heartbeat for five minutes: the worker died |

`health` is derived from `heartbeat_at`, never stored: the status alone cannot
tell a working task from one whose worker was evicted, because both read
`running`.

### What one task may spend

| Bound | Default | Effect |
| --- | --- | --- |
| `TASK_MAX_STEPS` | 12 | Rounds of the model-and-tools loop |
| `TASK_MAX_TOKENS` | 120000 | Input and output tokens together, per task |
| `TASK_MAX_HANDOFF_DEPTH` | 2 | How deep a chain of handoffs may nest |
| `TASK_MAX_TOOL_RESULT_BYTES` | 16384 | A longer tool answer is truncated |
| `LLM_TOKENS_PER_PERIOD` | 0 (unlimited) | Tokens per workspace per billing period |
| `PLAN_DAILY_COST_MICROS` | 0 (unlimited) | Cost per workspace per day, in micro-rupiah |

The last two are the workspace's, not one task's, and they are asked at two
boundaries: the task asks them at a clean round boundary, and the gateway asks the
same policy before every model call. The first is what stops a task with a reason
the user reads; the second is what stops a round from crossing the bound in the
middle of it.

A task that hits a bound finishes as `failed` with a `stopped_reason` in
Indonesian, rather than hanging or being silently truncated. A workspace that hits
one has the call refused with `quota_exceeded` (429).

### Approvals and handoffs

A tool labelled `write_external` is never executed directly: it becomes a draft,
the task is parked as `waiting_approval`, and the workflow waits for the
decision. An approval continues the round; a revision comes back to the model as
a failed tool result, so it can try again. A deployment with no approval gate
configured refuses the action instead of taking it.

`handoff` is a tool the runtime performs itself: it opens a child task for
another Bolu and waits for its answer, which the parent then uses as the tool
result. A refusal — a chain past the depth ceiling, a Bolu that is resting, a
name that matches nobody — is returned to the model as a failed result rather
than failing the task. A child task is always in the same workspace.

## Error codes

| Code | Status | Meaning |
| --- | --- | --- |
| `invalid_request` | 400 | Malformed body |
| `invalid_input` | 400 | A field is wrong (blank name, unknown timezone, bad address) |
| `weak_password` | 400 | Password policy not met |
| `invalid_token` | 400 | Token unknown, expired, or already used |
| `invalid_invitation` | 400 | Invitation unknown, expired, or already accepted |
| `unauthenticated` | 401 | Missing, malformed, expired, or revoked access token |
| `invalid_credentials` | 401 | Wrong email or password |
| `forbidden` | 403 | Role does not allow the action |
| `not_a_member` | 403 | Caller is not a member of the named workspace |
| `email_not_verified` | 403 | The account must verify its email first |
| `invitation_email_mismatch` | 403 | Sign in with the invited address |
| `user_not_found` | 404 | No such account |
| `workspace_not_found` | 404 | No such workspace |
| `member_not_found` | 404 | No such member |
| `email_taken` | 409 | Address already registered |
| `last_owner` | 409 | A workspace must keep at least one owner |
| `cannot_remove_yourself` | 409 | Use leave instead |
| `invitation_pending` | 409 | An invitation for this address is already pending |
| `too_many_attempts` | 429 | Login throttle |
| `auth_not_configured` / `workspace_not_configured` | 503 | Module not wired (no database) |
| `google_not_configured` | 503 | Google client not configured |
| `agent_not_found` | 404 | No such Bolu in this workspace |
| `team_not_found` | 404 | No such team in this workspace |
| `integration_not_found` | 404 | No such integration, or no such grant |
| `invalid_status` | 400 | Status is not `active` or `resting` |
| `invalid_permission` | 400 | Permission is not `read` or `read_write` |
| `agent_busy` | 409 | The Bolu still has a running task |
| `agent_limit_reached` | 409 | The plan's Bolu allowance is used up |
| `provider_not_found` | 404 | No such provider in this workspace |
| `provider_exists` | 409 | A provider with that name already exists |
| `no_provider` | 409 | The workspace has no provider, so no model can be called |
| `unsupported` | 400 | The chosen model cannot serve the request (tools, vision) |
| `context_too_long` | 400 | The prompt does not fit the model's context |
| `rate_limited` | 429 | The provider asked us to slow down |
| `quota_exceeded` | 429 | The workspace used up its token allowance |
| `provider_unavailable` | 502 | The provider is down or unreachable |
| `usage_not_recorded` | 500 | The call succeeded but its cost could not be written |
| `llm_not_configured` | 503 | Module not wired (no database) |
| `conversation_not_found` | 404 | No such conversation in this workspace |
| `message_not_found` | 404 | No such message in this workspace |
| `attachment_not_found` | 404 | No such attachment in this workspace |
| `task_not_found` | 404 | No such task in this workspace |
| `unknown_trigger` | 400 | A client may not open a task with that trigger |
| `task_limit_reached` | 409 | The task hit a step, token, cost, or depth bound |
| `task_not_configured` | 503 | Module not wired (no database) |
| `agent_not_in_conversation` | 400 | That Bolu is not a participant here |
| `no_responder` | 409 | No Bolu in the group can answer (all resting, or none configured) |
| `invalid_block` | 400 | A block does not satisfy its schema |
| `block_too_large` | 413 | A block exceeds its size limit |
| `file_too_large` | 413 | An attachment exceeds 25 MB |
| `storage_not_configured` | 503 | Object storage is not configured |
| `chat_not_configured` | 503 | Module not wired (no database) |

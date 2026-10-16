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
    "has_password": true, "has_google": false,
    "role": "user"
  }
}
```

`user.role` is the **platform** role, not the workspace one: `admin` reaches
`/admin`, `user` is everybody else. The screen uses it to decide whether to offer
the link, and the API checks it again on every admin request. See
[Platform role and the operator surface](#platform-role-and-the-operator-surface).

The refresh token is **not** in the body: it is set as an `httpOnly` cookie named
`bolu_refresh`, scoped to `/api/auth`, `SameSite=Lax`, `Secure` outside local
development.

Two endpoints answer identically whether or not the address exists
(`resend-verification`, `password/forgot`), so they cannot be used to discover
accounts.

**Email verification is a deployment switch.** With `AUTH_REQUIRE_EMAIL_VERIFICATION=true` (the
default) `register` creates a pending account and emails a link; `register` returns
`email_verified:false` and the account cannot use onboarding or accept an invitation until the link
is consumed. With the flag `false`, `register` marks the address verified at once, sends no mail, and
returns `email_verified:true` — so a client can sign in straight after registering. A sign-in by an
account that was created before the switch is healed the same way. The flag is read from the
environment, never from a request.

## Tenancy

`X-Workspace-Id` (or `?workspace_id=`) selects the active workspace. The value
must be a UUID, and the caller must be a member, otherwise the answer is `403`.

The workspace profile is more than a name: `language` (`id`, `en`, or `both`),
`work_hours_start` and `work_hours_end` (`HH:MM` in the workspace's `timezone`),
and `work_days` (ISO weekday numbers, 1 = Monday) are the values the approval
gate reads when it decides whether an approved action may leave now. `PATCH`
replaces the whole profile, so a caller that omits a field omits it from the
workspace — the Pengaturan screen therefore sends every field on every save. A
window that cannot be parsed, a start equal to its end, and a week with no
working day in it are all refused with `invalid_input`, because the gate would
otherwise hold every outbound action forever.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /workspaces` | bearer | any | Every workspace the account belongs to, with its role |
| `POST /workspaces/onboard` | bearer + verified | any | Creates workspace + owner + Tim Bolu + Tim Hore; **idempotent** |
| `POST /invitations/accept` | bearer + verified | any | Joins the invited workspace; the signed-in email must match |
| `GET /workspaces/current` | bearer + tenant | any | The active workspace |
| `PATCH /workspaces/current` | bearer + tenant | owner | Name, business field, timezone, language, working hours and days |
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

## Platform role and the operator surface

Two different things are called a role, and confusing them is the mistake this
section exists to prevent.

`members.role` is the **workspace** role — `owner`, `admin`, `member` — granted by
whoever owns that workspace, and it answers *what may this person do inside this
workspace*. The matrix above is that one.

`users.role` is the **platform** role — `user`, `admin` — and it answers *may this
person see the deployment*. It is a property of the account rather than of a
membership, so an account that belongs to no workspace at all still has one.

| Endpoint | Auth | Notes |
| --- | --- | --- |
| `GET /admin/overview` | bearer + platform admin | Counts of accounts: total, admins, users, verified, onboarded, Google, new in 7 days |
| `GET /admin/users` | bearer + platform admin | `?q=&role=&limit=&offset=`; meta carries `total`, `limit`, `offset` |
| `POST /admin/users/:userID/role` | bearer + platform admin | `{role}` where role is `user` or `admin` |

**Every account is a plain user.** The column defaults to `user`, so every account
that existed when the migration ran, and every account a registration or an
invitation creates afterwards, is one. Nothing in the product grants the platform
role: the first admin is made by one statement on the database —

```sql
UPDATE users SET role = 'admin' WHERE lower(email) = 'someone@example.com';
```

— and every admin after that is promoted from `/admin/users` by an admin.

**The role is read from the account, not from the token.** The guard behind these
routes asks the auth service for the account on every request, so a demotion takes
effect on the next call rather than when the access token expires. A promotion is
visible on `/auth/me` immediately, which is what lets the workspace sidebar offer
the link without a second endpoint.

**The surface asks for no workspace.** It is registered on a path prefix rather
than through a group, so no tenant middleware and no subscription guard reaches
it: an account with no workspace is still administrable. Three refusals are the
whole reason the module has a service:

| Refusal | Status | Code |
| --- | --- | --- |
| The caller is not a platform admin | `403` | `forbidden` |
| No such account | `404` | `user_not_found` |
| The account is the last admin | `409` | `last_admin` |
| The caller is demoting their own account | `409` | `self_demotion` |

The last-admin rule is enforced by the `UPDATE` statement itself — it demotes only
when a second admin exists — rather than by reading a count and then writing.
Two admins demoting each other at the same moment would each read a count of two
and each write, leaving the deployment with nobody able to reach the surface; a
single statement cannot be split that way.

**The boundary is `users`.** Every read in the module is a read of the account
table, and the only tenant table it touches is `members`, which it counts and does
not read. Bolus, tasks, conversations and money all stay behind the workspace
policies, so a platform admin administers the deployment without becoming a reader
of every customer's data.

## Agent registry (EPIC 3)

Every workspace starts with **no Bolu**: onboarding creates Tim Bolu and Tim Hore
and nothing else. The first Bolu is made by a person on the screen that says the
team is empty, which is also where the six seeded profiles are offered as starting
points. A team full of strangers somebody did not ask for is harder to understand
than an empty one.

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
| `PUT /agents/order` | bearer + tenant | owner, admin | The sidebar's layout: `{teams: [{team_id, agent_ids}]}`, every live Bolu exactly once. Answers with the stored teams |
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

### The sidebar's order is stored, and per team

`agents.sort_order` counts from zero inside its team, and `GET /teams` returns each
team's Bolu in it. A drag sends `PUT /agents/order` with the layout as it should
read — every team, and every live Bolu exactly once — rather than one move, because
the whole list is what makes the write idempotent and what keeps two tabs from
interleaving into an order that matches neither of them. The layout is checked
against the registry first: a body that forgets a Bolu, names one twice, or names a
team or Bolu outside the workspace is refused whole (`order_incomplete`, 400), so
there is no partial arrangement. A Bolu created afterwards lands at the end of its
team's list.

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
| `GET /llm/usage/report?days=30&limit=25` | bearer + tenant | any | One window read three ways: `totals`, `by_agent`, `by_task`. `format=csv` returns the same read as a file |
| `GET /tasks/attention?limit=50` | bearer + tenant | any | The tasks that need a person — `failed`, `stuck`, `waiting` — each with a `temporal_url` |
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

### Where the usage numbers come from

`GET /llm/usage/report` reads one window three ways, and the three are shown
together because the difference between them is the answer:

- **`totals`** is every call the workspace made in the window, including the ones
  made outside any task — a memory extraction, an embedding, a routing decision.
- **`by_agent`** is the same spend grouped by the Bolu that made it, dearest first,
  with the name joined so a screen says which bot rather than which id.
- **`by_task`** is the same spend grouped by the task that caused it, dearest first.

So the total is larger than the sum of the per-task rows, and that gap is a fact
about the product rather than a rounding error: work happens outside tasks. The
screen shows it as "di luar tugas" rather than hiding it, because a person who adds
up the rows and gets less than the total deserves to know why.

`format=csv` returns the same read as a rectangular table with one header row and a
`bagian` column naming the section (`total`, `per_bolu`, `per_tugas`). One shape,
because a file with two-field preamble rows and nine-field data rows is one a strict
reader refuses — the standard library's own refuses it, with "wrong number of
fields". The numbers are written as raw micro-rupiah *and* as rupiah with two
decimals, always with a dot: the export is machine-read, and a locale-dependent
separator would make the column text in half the countries it is opened in.

## Data policy and erasure (EPIC 13)

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `POST /workspaces/current/erasure` | bearer + tenant | owner | Removes the workspace and everything in it. Answers with `{workspace_id, objects, rows}` |
| `GET /workspaces/current/erasure` | bearer + tenant | owner | The record of what was removed, or `erasure_not_found` |

The person who asked is taken from the session, never from the body: who requested an
erasure is part of the record, and a caller who could name somebody else would be
writing that record. What the erasure covers, in the order it happens:

1. **Object storage** — both of the chat module's prefixes (`attachments`, `content`)
   are walked and emptied. Sandboxed HTML documents have no row at all, so a
   database-driven deletion would leave them behind with nothing pointing at them.
2. **Rows** — `DELETE FROM workspaces`, which cascades to every table that references
   it: conversations, messages, attachments, memories and their vectors, tasks and
   their steps, drafts, policies, audit lines, and the retention queue.
3. **Evidence** — a row in `data_erasures` carrying the workspace id, its name, who
   asked, when, and the counts. It has no foreign key, because it outlives what it
   describes.

The two failure modes are distinct and worth telling apart when something goes
wrong: `erasure_unavailable` (503) means this deployment cannot complete an erasure
— the record, the workspace deletion, or the object walk is not wired — and the
request is refused rather than half-done; `erasure_failed` (500) means the attempt
ran and stopped, with the reason on the record, and **nothing left half-removed**,
because the objects are only removed when they can be, and the workspace is only
deleted when they are.

The published policy is `docs/kebijakan-data.md`; the reasoning, the processors, and
the region decisions are in `docs/adr/0002-kepatuhan-uu-pdp.md`.

### An erasure is the one path past the trail

`audit_logs` is append-only, enforced by a trigger: an entry can be neither rewritten nor deleted, and
the only statement that may remove one is a workspace erasure — which says so for the length of its own
transaction (`app.audit_erase`, set with `SET LOCAL` inside the transaction that deletes). That is what
keeps "nobody can change the trail" and "a person can have their data deleted" from being two rules that
contradict each other, and it is a rule the database holds rather than one every caller remembers.

The consequence worth knowing when reading this module: **a workspace that ever recorded a sensitive
action cannot be erased without that flag**, because the `DELETE FROM workspaces` cascades into the trail
and is refused. The flag is set by the workspace repository's own erasure, not by a caller.


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

An upload is checked before anything is written, and the check is on both halves
of the file: the extension must be one the deployment allows
(`ATTACHMENT_ALLOWED_EXTENSIONS`, which can only narrow the module's own list of
images, PDFs, office documents, and text) and the first bytes must be that type,
so an executable is refused whatever it is named. A refused file is `415
file_type_not_allowed` and nothing is created. An upload sent without
`message_id` opens the message it belongs to in the same transaction that stores
the file, so a store that fails leaves neither a message nor an event behind, and
the only event a client sees is `message.new` carrying the file.

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

`draft.created` and `draft.decided` carry `{draft_id, task_id, agent_id,
action_kind, status, title, summary, revision, not_before, blocked}`. The card is
rewritten in the thread at the same time, so a reload and a live client agree.

A reconnecting client names the last event it saw and receives exactly what it
missed, in order: over HTTP with `?after_id=`, or on the stream with the
`Last-Event-ID` header (which a browser's EventSource sends by itself) or
`?last_event_id=`. An event id that appears twice is dropped by the client's
cursor, so a reconnect cannot duplicate a message.

`agent.state` carries `{agent_id, state, reason?, task_id?, draft_id?}` where
`state` is `working`, `thinking`, `waiting`, `idle`, or `resting`. **No
coordinate is stored anywhere**: a client that draws the crew derives a position
from the state.

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

### What the model reads from an attachment

A message's files are part of the prompt, not only of the screen. Before a model
call, the attachments of the turns being sent are rendered into the user turn they
belong to:

| The file | What the model receives |
| --- | --- |
| Text, CSV, JSON, Markdown, XML, YAML, HTML source | A section: `[Lampiran: name (type, size) #nonce]`, the text, `[/Lampiran #nonce]` — the nonce is eight random hex characters, one per file |
| PDF | The same section, holding the extracted text of every page |
| Picture whose bytes sniff as an image | `[Gambar name (type, size)]` in the text, plus the bytes as vision input |
| Anything else (Office documents, archives, audio) | `[Lampiran name (type, size): isi tidak dibaca — tipe ini belum didukung]` |
| A file that could not be read | `[Lampiran name tidak bisa dibaca: <reason>]` — a missing object, a corrupt PDF, a picture the active model cannot see |

The stored `content_type` is never trusted for the decision: the bytes are
sniffed, so a `.txt` that holds a PNG is sent as a picture.

An attachment is data, never instruction. Each section carries its own random
nonce, so a file cannot contain the marker that ends it; a filename is stripped of
line breaks and brackets, and text inside a file that spells a marker is rewritten
(`[/Lampiran` → `[/ Lampiran`, `[Lampiran` → `[ Lampiran`) with the section's nonce
replaced by `#`. Both system prompts tell the model the same thing: what sits
between the markers is untrusted file content, and instructions found inside it
are never to be followed.

What one request may carry is bounded, and every bound is configurable:

| Bound | Default | Effect |
| --- | --- | --- |
| `ATTACHMENT_CONTEXT_MAX_FILE_BYTES` | 10 MiB | A larger file is named, not read; checked before the object is fetched |
| `ATTACHMENT_CONTEXT_MAX_CHARS_PER_FILE` | 20000 | A longer text is cut, with a marker saying how much was shown |
| `ATTACHMENT_CONTEXT_MAX_CHARS_TOTAL` | 60000 | The budget for one request, spent newest message first |
| `ATTACHMENT_CONTEXT_MAX_IMAGES` | 4 | Pictures sent as vision input |
| `ATTACHMENT_CONTEXT_MAX_IMAGE_BYTES` | 5 MiB | A larger picture is named, not sent |

The budget is spent **newest first**, because a task answers the message that
opened it: an old thread full of documents degrades to names before the file the
question is about does. A file the budget or a bound excludes still appears in the
prompt, so the model can say which file it could not read rather than inventing an
answer.

A chain whose model cannot see pictures is retried once without them, and the
answer says so (`ChunkNotice`): a deployment with no vision provider loses the
pictures, not the reply.

Inside a durable task the workflow's state carries only
`{id, filename, content_type, size}` for each file; the `CallModel` activity
resolves the bytes. A replayed workflow therefore never holds a file, and a
Temporal history stays small.

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
GET  /api/tasks/attention  — the work that needs a person, with a Temporal link
POST /api/tasks/:id/cancel — stop a task that has not finished
```

All of them need a bearer token and an active workspace; the routes answer 503
with `task_not_configured` when the runtime is not wired.

`GET /api/tasks` filters: `status`, `live=true` (queued, running, or waiting),
`agent_id`, `parent_task_id`, `limit` (default 50, max 200). An unknown status is
refused with `invalid_input` rather than ignored.

`GET /api/tasks/attention` is one question — "what is waiting on me?" — rather
than three, because a person who asked for the stuck, the parked, and the failed
separately would read three lists that disagreed about their own paging. It reads
the failures and the live tasks, and keeps only the live ones that are not going to
move on their own: a task that is `running` is doing its job and is not on the list.
The order is failures, then stuck, then waiting — a person with an approval to make
and a failure to look at should make the decision first, because the parked task is
holding work that is otherwise done.

Each entry carries `attention` (`failed`, `stuck`, `waiting`) and a
`temporal_url` that opens the task's workflow in this deployment's Temporal UI.
The link is built by the server, from `TEMPORAL_UI_BASE_URL` and the namespace:
the deployment is the only thing that knows which Temporal it has, and a link the
browser assembled from configuration it does not have would work on a laptop and
break in production. It is absent when the deployment named no UI, and on a task
that never reached the engine — a link to a workflow that does not exist would be
worse than no link.

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

## Approval and drafts (EPIC 7)

An action that reaches outside Bolu never runs on its own. A tool labelled
`write_external` becomes a **draft**, the task parks as `waiting_approval`, and a
person decides.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /drafts` | bearer + tenant | any | `?status=&agent_id=&action_kind=&task_id=&limit=`; newest first |
| `GET /drafts/:id` | bearer + tenant | any | One draft, with the payload the card shows |
| `GET /drafts/:id/revisions` | bearer + tenant | any | The trail, oldest first |
| `POST /drafts/:id/approve` | bearer + tenant | any | `{note?}`; carries the action out |
| `POST /drafts/:id/revise` | bearer + tenant | any | `{note}`; a note is required |
| `POST /drafts/:id/cancel` | bearer + tenant | any | `{note?}` |
| `GET /approval-policies` | bearer + tenant | any | The policy in force for every external action kind |
| `PUT /approval-policies` | bearer + tenant | owner, admin | `{action_kind, mode, max_amount}` |
| `GET /audit` | bearer + tenant | any | `?limit=`; newest first |

Deciding is open to **every member**, not only a managing role: the person who
notices a wrong invoice is not always the owner, and a draft nobody can approve is
a draft nobody will. Changing a policy does need a managing role, because it
changes what may leave the workspace unattended.

### Draft status

| Status | Meaning |
| --- | --- |
| `pending` | Waiting for a person |
| `approved` | Allowed, and either sent or held until the workspace opens |
| `revise` | Sent back to the agent, which produces a new draft |
| `sent` | The action left |
| `canceled` | Nobody will act on it |

A decision is recorded **once**: only a `pending` draft can be decided, so a
duplicate signal changes nothing and answers `draft_already_decided` (409).

### Policies

An action kind with **no stored policy is `always_draft`**, enforced in the
service rather than assumed. Onboarding seeds an explicit conservative row for
every external kind, so the screen always shows something to edit.

| Mode | Effect |
| --- | --- |
| `always_draft` | A person decides, every time (the default) |
| `auto` | Carried out unattended, but only while the value is within `max_amount` |

`max_amount` is in rupiah. An action whose amount cannot be read is treated as
unbounded, so a ceiling cannot be satisfied by a payload that never states one.
An auto-send is still recorded: the draft row is written and marked sent, so
"what did the agents send" has an answer even when nobody was asked.

### Working hours

An action approved outside the workspace's hours is **held**, not sent: the draft
carries `blocked_reason` and `not_before`, the task parks as `waiting_approval`,
and the workflow waits on a durable timer. The hours are `work_hours_start` /
`work_hours_end` in the workspace's `timezone`, read at the moment the decision is
carried out, so changing either recomputes the hold.

### The card

A draft reaches the thread it came from as a `draft` block, which is rewritten
after a decision. The block carries `draft_id`, `action_kind`, `title`,
`summary`, `status`, `fields`, and — when it is held — `blocked_reason` and
`not_before`. The `draft.created` and `draft.decided` events carry the same
payload on the stream.

## Memory and knowledge (EPIC 10)

Two things are stored and they are not the same thing. A **memory** is something a
Bolu was told or worked out: short, written by a person or by extraction, and sent
in the prompt on every call. A **knowledge item** is one chunk of a document the
workspace owns: long, retrieved by similarity to the task at hand.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /memories` | bearer + tenant | any | `?agent_id=&layer=&q=&history=&limit=`; pinned first |
| `POST /memories` | bearer + tenant | any | `{content, kind, agent_id?, pinned?}` |
| `PATCH /memories/:id` | bearer + tenant | any | `{content?, kind?, pinned?, agent_id?, move_to_main?}` |
| `DELETE /memories/:id` | bearer + tenant | any | Permanent: the row and its vector |
| `GET /knowledge` | bearer + tenant | any | `?status=&source=&q=&agent_id=`; failed chunks included |
| `POST /knowledge` | bearer + tenant | any | `{source, text, agent_id?}`; chunks, embeds, stores |
| `DELETE /knowledge/:id` | bearer + tenant | any | Permanent |
| `PATCH /agents/:id/memory-extraction` | bearer + tenant | owner, admin | `{enabled}`; automatic extraction for one Bolu |

Every member reads and edits memory, the way every member reads and decides a
draft: the person who notices a wrong fact is not always the owner. The
extraction switch needs a managing role because it changes what a Bolu learns on
its own.

### The two layers

| Layer | `agent_id` | Who uses it |
| --- | --- | --- |
| Main memory | `null` | Every Bolu, in every call |
| Private memory | a Bolu id | That Bolu only |

`layer=main` and `agent_id=main` are both accepted on a read; `"main"` is also
what a move sends when a memory goes back to the workspace layer.

A search filters by layer as strictly as it filters by workspace: a private
memory that reached another Bolu's prompt would be as wrong as one that reached
another workspace. A search that crosses a workspace is refused at the database,
by the same Row Level Security policy every other tenant table carries.

### What travels in a prompt

The main memory joins the **cached prefix** — persona, instructions, main memory —
because it is the same text on every round. It is capped by `MainTokens`, pinned
memories first, and a memory that does not fit is skipped whole rather than cut in
the middle; if nothing fits, the newest one is truncated, because a Bolu that
remembers nothing is the worst outcome of the three.

The retrieved half — a Bolu's private memory and the closest document chunks —
travels separately, because it changes with the question. Each retrieved chunk is
wrapped in a marker that says it is data:

```
<data sumber="katalog.pdf">
Ongkir Bandung 15 ribu
</data>
```

The closing sequence is escaped inside the content, so a document that contains
`</data>` cannot end the block early and have the rest of its text read as the
workspace's own words.

### What is never stored

Card numbers, passwords, one-time codes, and identity or account numbers are
refused before the row exists, for all three doors: a direct request, the
extractor, and a document being ingested. A refusal is a `422` with
`sensitive_content`, and the memory screen says which pattern was found.

A delete is permanent. There is no soft-delete column, because a memory that
"forgot" but still sits in the table is not forgotten: the row goes, its vector
goes with it, and the retrieval record that named it goes too. The audit trail
records that something was deleted and what layer it was in, and never copies the
content: a memory is deleted because it should not be there, and writing it into
another table would keep it.

### Extraction

After a conversation goes quiet for `MEMORY_QUIET_SECONDS`, a small model reads
the last `MEMORY_EXTRACTION_MESSAGES` turns and proposes what is worth keeping.
General workspace facts go to the main memory, role-specific ones to the Bolu's
own. A proposal that is too short (`"oke"`, `"makasih"`) is dropped, one that
carries a secret is refused, and one that is close enough to an existing memory
replaces it: the older row is kept as history with `superseded_by` set, so a
corrected fact has exactly one active version and its old version stays readable.

The switch is per Bolu and defaults on. Turning it off stops the extractor, not
the feature: a person can still ask for something to be remembered.

### Automatic memory from a conversation

`Send` with `reply: true` is the only message path a memory request goes through.
The reply to a message that asks to remember is written by the memory module
itself — the same module that stored the row — so the confirmation cannot name
something that does not exist. It costs no tokens.

## Routines and scheduling (EPIC 11)

A **routine** is work a Bolu does on its own. It has a cadence, an instruction, and
a history of what it did. A **routine run** is one execution: it opens exactly one
task, and the task's own one-line outcome is what the history shows.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /routines` | bearer + tenant | any | `?agent_id=&enabled=`; each row carries its next run times |
| `POST /routines` | bearer + tenant | any | `{agent_id, name, kind, minute, hour_start, hour_end, day_of_week, trigger_source, instruction, enabled}`; registers the schedule |
| `GET /routines/:id` | bearer + tenant | any | One routine, with its next run times |
| `PATCH /routines/:id` | bearer + tenant | any | Same body; re-registers the schedule |
| `DELETE /routines/:id` | bearer + tenant | any | Removes the routine and its schedule; the run history stays |
| `POST /routines/:id/pause` | bearer + tenant | any | Pauses the schedule; the routine stays listed |
| `POST /routines/:id/resume` | bearer + tenant | any | Resumes the same schedule |
| `GET /routines/:id/runs` | bearer + tenant | any | `?limit=`; this routine's history, newest first |
| `GET /routine-runs` | bearer + tenant | any | `?agent_id=&routine_id=&status=&limit=`; the workspace's history |
| `POST /routines/preview` | bearer + tenant | any | `{kind, minute, hour_start, hour_end, day_of_week}`; the next times, for a form |

Every member reads and edits routines: a routine is the team's work, and the person
who notices it is running at the wrong hour is not always the owner.

### The four cadences

| `kind` | Uses | Runs |
| --- | --- | --- |
| `daily` | `hour_start`, `minute` | Once a day at that wall-clock time |
| `hourly_range` | `hour_start`, `hour_end`, `minute` | Every hour inside the window, inclusive of both ends |
| `weekly` | `day_of_week` (ISO 1–7), `hour_start`, `minute` | Once a week on that weekday |
| `trigger` | `trigger_source` (an event, `app.thing`) | When that event arrives. **No schedule is registered** |

A trigger routine gets no Temporal Schedule at all: a cron expression for something
that happens when it happens would fire on an empty queue and spend tokens on
nothing. It is started by the delivery itself — the hub hands the event to the
routines watching it as soon as it is queued, and the run claims the delivery in
the same step, so two routines watching one event cannot both act on it.

### One schedule per routine

`schedule_id` is derived from the routine's own id (`routine-<uuid>`), so registering
a routine twice leaves exactly one schedule — the database enforces it with a unique
index, not the service's carefulness. Deleting a routine deletes its schedule and
keeps its runs: `routine_runs.routine_id` is dropped to null and the routine's name
was snapshotted when the run was written, so the history still reads as a line.

### The workspace's clock, not the routine's

A routine has no timezone of its own. Every schedule is registered in
`workspace.tz`, so changing the workspace's timezone moves every future run of every
routine — and the times a screen shows are computed from the same place, so what a
list promises is what the engine fires. Changing the zone or the working window
re-registers the schedules rather than recomputing a cached time, because there is
no cached time.

### What one run costs

Each run performs a cheap check before anything else: a routine watching events
reads its queue, and a routine whose Bolu is resting stops there. Either way the run
is recorded as `skipped` with the reason, and **no model is called** — the product's
largest saving, and one that is counted rather than claimed: `usage_ledger` gains no
row.

A run's status is `running` while its task is in flight, and the task's own outcome
finishes it: `succeeded` with the task's one-line summary, or `failed` with the
task's own reason ("model gagal menjawab…") rather than a step count. A run that
could not open a task at all is `failed` with the reason recorded, and the schedule
keeps firing — one bad run must not stop a routine forever.

## WhatsApp (EPIC 9)

A **number** is a WhatsApp line this workspace linked by scanning a code. It is a
connection like any other — it appears in the hub, a Bolu is granted it, and an
action on it passes through the approval gate — with one difference: there is no
token. The session lives in the `wa-gateway` process, which is the only thing that
stays connected to WhatsApp.

| Endpoint | Auth | Role | Notes |
| --- | --- | --- | --- |
| `GET /whatsapp/numbers` | bearer + tenant | any | The workspace's numbers, with their state and how many messages an hour they may send |
| `POST /whatsapp/numbers` | bearer + tenant | any | `{phone, label?, agent_id}`; records the number and who answers it. Links nothing yet |
| `GET /whatsapp/numbers/:id` | bearer + tenant | any | One number |
| `PATCH /whatsapp/numbers/:id` | bearer + tenant | any | `{label?, agent_id?, hourly_limit?}`; each field is its own statement |
| `DELETE /whatsapp/numbers/:id` | bearer + tenant | any | Ends the session, keeps the message history |
| `POST /whatsapp/numbers/:id/terms` | bearer + tenant | any | Records that the warning was accepted. Required before a link |
| `POST /whatsapp/numbers/:id/link` | bearer + tenant | any | Starts a pairing; answers the first state to show |
| `GET /whatsapp/links/:linkID` | bearer + tenant | any | One attempt, for a screen that reloaded |
| `GET /whatsapp/links/:linkID/socket` | token + tenant in query | any | The QR, over a WebSocket. See below |
| `POST /whatsapp/links/:linkID/pair-code` | bearer + tenant | any | `{phone}`; the alternative for a phone that cannot scan |
| `DELETE /whatsapp/links/:linkID` | bearer + tenant | any | Abandons an attempt |
| `GET /whatsapp/numbers/:id/messages` | bearer + tenant | any | `?chat=&limit=`; what customers wrote, newest first |
| `GET /whatsapp/numbers/:id/outbox` | bearer + tenant | any | `?status=&limit=`; what this workspace asked to send |
| `GET /whatsapp/messages/:id/media` | bearer + tenant | any | One attachment's bytes, `inline` with its own content type |

### The five states a number is in

| `status` | `linked` | Meaning |
| --- | --- | --- |
| `unlinked` | false | A row somebody created and did not finish. It is where every number starts |
| `linking` | false | An attempt is in progress |
| `connected` | true | A live session |
| `disconnected` | true | A session that exists and is offline. It reconnects on its own when it can |
| `needs_relink` | true | WhatsApp refused the session. Waiting does not fix it, and the owners are emailed |

### Why the QR travels on its own socket

A QR is a **capability**: whoever scans it links the number. The workspace event
stream is delivered to every member, so a code sent there would let a colleague
link a number into somebody else's workspace. The attempt therefore has its own
socket, authenticated as the person who started it, and it closes the moment the
attempt ends.

The polling read (`GET /whatsapp/links/:linkID`) stays as the way a screen
recovers after a reload or a dropped connection. The frame carries the QR as a
**matrix** — rows of `"1"` and `"0"` — rather than as an image, because the browser
has no QR encoder and an `<img>` cannot send an `Authorization` header.

### What leaves, and how fast

Nothing in this module decides that a message should go out. A message arrives
through the hub: the model calls `whatsapp.send_message`, the approval gate holds
it as a draft, and an approval queues it through the integration executor.

What the module does decide is the pace, and it is a pace rather than a limit:

- messages wait at least a few seconds, drawn at random, before the first of a
  batch leaves — a batch that leaves in the instant it was approved reads as a
  machine posting;
- the typing indicator is shown before each send, not only the first;
- an hourly ceiling per number, counted from the messages that actually left
  rather than from a counter that can drift. A number that reaches it **keeps its
  queue**: the messages are pushed to later rather than parked as failed, and the
  owners are told they are paced rather than lost.

### One number, one owner

A WhatsApp session driven by two processes sends every message twice, which is how
a number gets restricted. A gateway instance therefore holds a **lease** on a
number in Redis before it connects one, renews it on an interval, and stands down
immediately when a renewal fails — because a failed renewal means a successor is
already connecting. A clean shutdown releases its leases, so a rolling restart
hands over in milliseconds rather than waiting out the TTL.

The fleet finds its numbers through `whatsapp_directory`, which is the one table in
this codebase without Row Level Security. The migration says why: the gateway is a
fleet worker rather than a tenant, and it has to discover which tenants have a
number before it can open any workspace's scope at all. What makes that safe is
what the table holds and how the rest is read — it is a routing pair and nothing
else, and every session blob it leads to is sealed with its own workspace's key and
bound to that workspace id as associated data, so a directory row naming the wrong
tenant produces an unopenable blob rather than another tenant's session.

### The session is sealed, and the cache is not

The device state — the noise key, the identity key, the signed pre-key, the signal
sessions, the pre-keys, and the app-state keys — is held in memory and persisted as
one document sealed with the workspace's own data key, the same key ring the hub
seals tokens with. A blob cannot be opened under another tenant's key, and cannot
be replayed against another tenant even if the row is copied.

What is deliberately **not** persisted is the cache: contacts, chat settings,
message secrets, privacy tokens, and the outgoing-event buffer. WhatsApp re-sends
those when a connection comes up, and keeping them would grow the sealed blob with
data nothing here reads.

## Order and catalogue (EPIC 14)

```
GET    /api/orders/products               the catalogue (?q=, ?include_inactive=, ?limit=)
POST   /api/orders/products               add a product (owner, admin)
PUT    /api/orders/products/:id           edit a product (owner, admin)
DELETE /api/orders/products/:id           remove a product (owner, admin)
GET    /api/orders                        the orders, newest first (?customer_phone=, ?limit=)
GET    /api/orders/:id                    one order with its lines
POST   /api/orders/:id/cancel             mark it as not fulfilled (owner, admin)
```

The workspace's own data: the products it sells, the customers it talks to, and the orders it
took. It is the first module whose tables are the workspace's business rather than a copy of
something that lives at a provider, and the first whose tools belong to the workspace rather than
to a connected account.

`meta.count` on a list endpoint is the number of rows **in this page**, not the size of the result
set — a client that wants a total reads the rows it asked for, or pages until `has_more` is false.
That is the same shape the other list endpoints use, and it is worth knowing before writing a counter
against `?limit=1`.

### Prices travel in rupiah and are stored in micro-rupiah

A screen writes and reads `price_idr` — 15000 is Rp 15.000 — and the column holds micro-rupiah,
the unit `plans`, `usage_ledger`, and the cost report already use. One unit for money means an
invoice total and a model's cost are the same number of the same thing, which is what makes them
comparable; the conversion happens once, in the handler.

A line keeps its own copy of what was agreed — the description, the unit, and the price — and
still references the catalogue entry. Changing a product's price therefore does not rewrite an
order that already exists, while "which catalogue entry was this" stays answerable. The line's
total is written by the database as a generated column, and the order's total from its own lines,
so a total cannot disagree with the quantity and price beside it.

### An order is recorded by a Bolu, never by a client

There is no endpoint that inserts an order, and that is deliberate: an order is a record of what a
customer asked for, which is why a task reads a message and records it. A client that could insert
one directly could claim an order that never happened. Cancelling is the one write, because a
person deciding not to fulfil an order is exactly the kind of decision the product puts in a
human's hands.

The tool that records one takes a product name and a quantity and **no price**: the price comes
from the catalogue, inside the same transaction that writes the order. A model therefore cannot
put a number it invented on an invoice — there is nowhere to put it. A product the catalogue does
not have refuses the whole order, carries the closest entries with their prices, and writes
nothing; a quantity that is not positive is refused before the first insert, naming the line.

### The catalogue is a managing role's business

Reading it is every member's, because the person answering "what do we sell this for" is not
always the owner. Writing it needs a managing role, the way the provider keys do: a price is what
every later quote to every customer is based on. Duplicate names are refused case-insensitively —
`Kopi Arabika` and `kopi arabika` are the same product to everybody except a database — with
`duplicate_product`.

## The plan a Bolu keeps for itself (EPIC 15)

Every other table in this schema is about work somebody gave a Bolu. This one is about work a Bolu
gave itself: a short list of tasks, each with optional subtasks, that it writes down before it starts
and ticks off as it goes. It exists because a multi-step job otherwise lives entirely inside one model
round, and a Bolu that restarts — or simply runs out of a step allowance — has no record of where it
had got to.

```
GET    /api/checklists                     the workspace's plans, newest first (?agent_id=, ?status=, ?limit=)
```

That is the whole HTTP surface, and it is read-only. A plan is written, ticked, and thrown away by the
Bolu through five tools:

| Tool | Label | What it does |
| --- | --- | --- |
| `checklist.create` | `write_internal` | Writes the plan: a title and a list of tasks, each with optional subtasks |
| `checklist.check` | `write_internal` | Ticks one task or subtask, by id |
| `checklist.remove` | `write_internal` | Drops one task, and its subtasks with it |
| `checklist.clear` | `write_internal` | Throws the whole plan away |
| `checklist.read` | `read` | Reads the plan back: the ids, the titles, and what is already ticked |

None of the five needs a connected account — `needs_connection` is false on their catalogue rows — so
they are offered to any Bolu whose profile asks for them, the same way the catalogue tools are. Every
template carries them, and the migration appended them to every Bolu that already existed: a notebook
reaches nothing outside the workspace, and a Bolu that cannot write down what it is doing is the state
this module exists to end. A workspace that does not want it can still edit a Bolu's tools on the Bolu
screen.

### One plan at a time

A Bolu holds exactly one active plan, and a second `checklist.create` is refused — and the refusal
names what is still open. The rule is what makes the list a goal rather than a scratchpad: a model
with three open lists has three things it is "working on" and no answer to "what are you doing".

It is a partial unique index on `(workspace_id, agent_id) WHERE status = 'active'`, not a check the
service remembers to make — a second writer would not have the service's memory. The refusal carries
the open tasks, and the open subtasks of each, because that list is the one thing that tells the model
what to do next: "you still have a plan" on its own leaves it nothing to act on.

`checklist.clear` is the escape hatch the rule needs. A model that wrote a plan the job turned out not
to need would otherwise be holding it until every line was ticked, which is a rule that punishes
writing anything down.

### Completion is derived, never declared

A plan is `done` when every top-level task is ticked, and the tick that closes the last one settles the
plan in the same transaction — so a plan whose work is finished cannot be left open with the Bolu
unable to start the next one. There is no tool that marks a plan finished: a status a model could set
while items were open would be a status that lies about what happened.

A task with unfinished subtasks cannot be ticked at all. The service refuses it with the subtasks that
are open, and a trigger on `checklist_items` refuses it again underneath — so the state cannot exist
even if a future writer forgets to ask. A subtask cannot have subtasks of its own: the plan is a list a
person scans, not a project tree.

Removing the last open task settles the plan too, rather than leaving an empty one open. The model
removed the work because it decided it was not to be done, and that is an ending.

### A plan is the Bolu's, and a client only reads it

There is no endpoint that writes a plan, for the reason there is no endpoint that inserts an order: a
client that could tick a line could make a Bolu believe work was done that nobody did. The Bolu is the
only thing that moves its list; a screen shows it.

A plan is a list a person reads, so it is bounded — twenty tasks, ten subtasks each, two hundred
characters a title — and the bounds are the service's, refused with the place the mistake was in
("tugas ke-3 belum punya judul") rather than as a driver error.

`GET /api/checklists` returns each plan with `done_count`, `total_count`, and its items nested one
level deep, so a screen that shows progress reads it rather than counting for itself.

## Error codes

| Code | Status | Meaning |
| --- | --- | --- |
| `invalid_request` | 400 | Malformed body |
| `invalid_input` | 400 | A field is wrong (blank name, unknown timezone, bad address) |
| `weak_password` | 400 | Password policy not met: at least 8 characters, mixing upper- and lowercase, with a digit |
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
| `checklist_not_configured` | 503 | Module not wired (no database) |
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
| `draft_not_found` | 404 | No such draft in this workspace |
| `draft_already_decided` | 409 | The draft already has an answer; the first stands |
| `draft_not_approved` | 409 | The action was attempted without an approval |
| `unknown_mode` | 400 | Mode is not `always_draft` or `auto` |
| `approval_not_configured` | 503 | Nothing can carry the action out yet (EPIC 8) |
| `agent_not_in_conversation` | 400 | That Bolu is not a participant here |
| `no_responder` | 409 | No Bolu in the group can answer (all resting, or none configured) |
| `invalid_block` | 400 | A block does not satisfy its schema |
| `block_too_large` | 413 | A block exceeds its size limit |
| `file_too_large` | 413 | An attachment exceeds 25 MB |
| `file_type_not_allowed` | 415 | The upload is not a type this deployment allows |
| `storage_not_configured` | 503 | Object storage is not configured |
| `chat_not_configured` | 503 | Module not wired (no database) |
| `memory_not_found` | 404 | No such memory or chunk in this workspace |
| `sensitive_content` | 422 | The content is a secret and will not be remembered |
| `memory_unavailable` | 503 | No provider can embed, so nothing can be stored |
| `routine_not_found` | 404 | No such routine in this workspace |
| `routine_unavailable` | 503 | No scheduler is reachable, so nothing can be registered then fired |
| `whatsapp_not_found` | 404 | No such number or message in this workspace |
| `terms_not_accepted` | 409 | The warning about an unsupported route was not accepted yet |
| `already_linked` | 409 | This phone number is already linked, here or in another workspace |
| `no_agent` | 409 | The number has no Bolu assigned, so an inbound message has no reader |
| `not_connected` | 409 | The number is not linked, or is offline |
| `link_expired` | 410 | The pairing ran out of time; ask for a new code |
| `whatsapp_unavailable` | 503 | No gateway is configured for this deployment |
| `media_not_found` | 404 | The message carries no attachment, or its bytes are not stored |
| `billing_not_found` | 404 | No such plan, pack, invoice, or subscription in this workspace |
| `no_budget` | 402 | The workspace's package does not allow the reply to be opened |
| `invoice_settled` | 409 | The invoice already has an outcome; the first stands |
| `unverifiable_payment` | 400 | A payment notification that cannot be trusted; retrying cannot help |
| `billing_unavailable` | 503 | No payment provider is configured for this deployment |
| `billing_not_configured` | 503 | Module not wired (no database) |
| `duplicate_product` | 409 | A product with that name is already in the catalogue |
| `order_not_configured` | 503 | Module not wired (no database) |

## Billing, packages, and quota (EPIC 12)

```
GET    /api/billing/subscription          the package, the usage, what is owed
GET    /api/billing/plans                 the packages this deployment sells
POST   /api/billing/subscription          choose another package
GET    /api/billing/invoices              the invoice history (?status=, ?limit=)
POST   /api/billing/invoices/:id/pay      ask the provider for a way to pay
POST   /api/billing/topups                buy tokens inside the current period
POST   /api/billing/webhooks/:provider    the provider's payment notification (public)
```

Every member reads and every member pays. That is deliberate: the person who notices the
workspace has gone read-only is not always the owner, and the money arriving is what fixes
it.

### What a workspace may spend is derived, never remembered

The package grants tokens per period, plus what the workspace bought on top. What is *used*
is the live counter in Redis, reconciled with `usage_ledger` — and when the two disagree, the
larger one wins. That direction is the point: a Redis that lost its keys must not hand a
workspace its month again, and a ledger write that has not landed must not either.

A stored "remaining" would be a number that can be wrong, and a wrong allowance is either a
workspace billed for tokens it never used or one served for free. So `entitlement` is
computed from the subscription's own times on every read, and the screen reads that rather
than computing one of its own.

### Two bounds, asked at two boundaries

The period's token allowance stops a workspace from spending a month's tokens; a package's
daily cost ceiling stops one runaway task from spending a month in an afternoon. Both are
asked by the model gateway before every call *and* by the task runtime at a clean round
boundary, so a task stops with a reason rather than halfway through a call.

`kuota token habis` refuses a model call and a new task; the task already running finishes.
The message names what was used and what the package grants, because "out of quota" alone
tells nobody whether they are one token over or a month over.

### Thresholds are reported once per period

At 80% and at 100% of the allowance the workspace is told — by a line in its event stream
and by email. A period that jumps from 70% to 105% between two passes reports **both**, in
order: the 80% message is the one that gives somebody time to buy a top-up, and sending only
the last threshold would announce a workspace that has already stopped.

The marks are in `quota_periods` (`warning_80_sent_at`, `warning_100_sent_at`) because the
state itself is derived: without a marker the sweep would repeat the same sentence every
pass.

### The way out is always open

Past the period and past the grace window, the workspace is **read-only**: reads and exports
keep working, and writes and new work are refused with `402 no_budget`. Nothing is deleted
and nothing is hidden.

The billing endpoints are exempt from that guard, and the exemption is not a detail. A
workspace that cannot pay would be read-only forever if the one call that fixes it were
refused too — which is a trap rather than a policy. So the guard is registered on each route
group rather than inside each service, and the billing routes are outside it.

### One invoice per period, however often it is asked for

The invoice for the next period is issued when the current one ends, and a partial unique
index on `(subscription_id, period_start, kind)` where the status is `pending` is what makes
a repeated pass, a screen, and a retried sweep write one row between them. The statement is
`ON CONFLICT DO NOTHING` rather than raising: the answer a second writer wants is the invoice
that exists, and a failed statement would abort the transaction the recovery read runs in.

It is partial on `pending` so that an **expired** invoice is re-issuable: a workspace whose
payment window closed must be able to be sent a new one, and a total index would refuse it a
row for a month it still owes. What was expired stays as history.

### The provider's reference and the merchant order id

Duitku posts its notification as a urlencoded form naming `merchantOrderId`, so that is what
an invoice records as its provider reference — **not** Duitku's own internal `reference`. The
order id is derived from the invoice rather than random, so a transaction whose response was
lost is the same transaction when it is tried again: Duitku refuses a repeated order id,
which turns a lost response into a readable refusal instead of a second payment.

The callback is verified with Duitku's own HMAC-SHA256 over
`merchantCode + amount + merchantOrderId`. That recomputation is the whole security of the
route, so it is compared in constant time, and a delivery that fails it is answered **400**
rather than 500 — a provider told "internal error" would keep redelivering a body that can
never verify.

Both directions carry a signature over different strings, which is Duitku's own asymmetry:
the inquiry signs `merchantCode + merchantOrderId + paymentAmount` and the callback signs
`merchantCode + amount + merchantOrderId`.

### A payment is applied once

The settlement is written by a statement guarded on `status = 'pending'`, so a replayed
webhook updates nothing and the caller is answered with the invoice as it already stands.
Providers deliver a notification until somebody acknowledges it, so this has to be the
statement's own guard rather than the caller's memory.

A payment smaller than the invoice does not buy it: the invoice stays outstanding and the
mismatch is recorded, rather than extending a package for less money than it costs.

When a subscription payment lands, the invoice that was just paid is settled and any *other*
outstanding subscription invoice is closed — a period that ended while one was outstanding
can be issued a second one, and somebody who has paid must not be shown an amount for a month
they are now inside. A top-up is never withdrawn: what was bought on top of a package is not
affected by paying for the package.

### A workspace always has exactly one live subscription

The trial package is created **inside the onboarding transaction**, by the workspace module's
repository acting as the billing module's `SubscriptionProvisioner`. A workspace therefore
cannot exist without a package, and "exactly one live subscription" is a fact about the data
rather than a rule every read has to remember. The migration backfilled every workspace that
already existed.

### Changing a package is deferred, and the package bounds real things

A package's caps — Bolu, connections, members — are enforced through a small port in each
module (`agentdomain.PlanLimits`, `integrationdomain.PlanLimits`, `workspacedomain.PlanLimits`),
answered by this module. Unwired means the deployment sells no packages, and nothing is
capped.

The member cap counts **open invitations too**: an invitation is a seat somebody will take,
and counting only the members would let a workspace invite ten people into a package that
allows three — with the last seven failing at acceptance, after being told to expect access.

Changing a package inside a period that was already paid for is **deferred to the next one**.
Taking a bigger package mid-month would either hand out the bigger allowance for free or
charge a proration nobody asked for. A workspace on its trial has nothing to protect — no
money changed hands for the period it is in — so the change applies immediately.

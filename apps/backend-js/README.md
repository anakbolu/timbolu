# backend-js

A Bun + TypeScript port of `apps/backend` (Go). Drop-in replacement: same routes,
same JSON field names, same status and error codes, so `apps/frontend` and the
WhatsApp gateway need no change.

Read [`CONTRACT.md`](CONTRACT.md) before adding to it, and
[`PORTING.md`](PORTING.md) for the file-by-file status.

## Run

```bash
bun install
DATABASE_URL="postgres://employeebot:employeebot_secret@localhost:5432/employeebot" \
REDIS_URL="redis://localhost:6379" \
JWT_SECRET="dev-secret-change-me-in-production" \
HTTP_ADDRESS=":8080" \
bun run src/cmd/api.ts
```

```bash
curl localhost:8080/health
curl localhost:8080/ready
```

Migrations (the same `.sql` files the Go binary embeds):

```bash
DATABASE_URL=... bun run src/cmd/migrate.ts          # up
DATABASE_URL=... bun run src/cmd/migrate.ts --down 1
```

Typecheck:

```bash
bun run typecheck
```

## Layout

```
src/platform/     infrastructure: config, database, authn, crypto, kms, storage,
                  redis, mail, llm adapters, temporal, middleware, oauth, …
src/modules/      one directory per module, mirroring the Go tree exactly:
                    domain/ · repository/ · service/ · handler/
src/container/    the assembly point and the route table
src/cmd/          api.ts, migrate.ts
src/migrations/   the Go migrations, copied verbatim
```

## Two compatibility notes

**Passwords use `@node-rs/argon2`, not `Bun.password`.** Go encodes argon2id with
`base64.RawStdEncoding` (unpadded); `Bun.password.verify` rejects that PHC string,
so existing hashes would stop verifying and every user would be locked out.
`@node-rs/argon2` parses it as written.

**Timestamps and money keep their Go JSON types.** `timestamptz` arrives as a
`Date` and serialises to the same RFC3339 string; `int8` is parsed to `Number` so
money-in-micros and token counts stay numbers rather than strings.

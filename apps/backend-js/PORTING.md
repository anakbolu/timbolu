# Go → TypeScript porting table

Generated 2026-10-17 from the filesystem, not from a checklist.

Every `.go` file under `apps/backend` and `packages/platform` is listed below with its
TypeScript counterpart. `ported` means the file exists and `bun run typecheck` accepts it.

## Summary

| | count |
| --- | --- |
| ported | 168 |
| missing | 43 |
| folded | 53 |
| skipped | 194 |
| **total** | **458** |

`folded` = `sqlc`-generated Go from the SQL; in TypeScript the query *is* the
repository function, so those are absorbed into `repository/postgres.ts`.
`skipped` = test files and generated gomock mocks, out of scope for this rewrite.

## Per group

| group | ported | missing | folded | skipped |
| --- | --- | --- | --- | --- |
| `cmd` | 0 | 4 | 0 | 1 |
| `module/admin` | 4 | 0 | 3 | 4 |
| `module/agent` | 5 | 0 | 3 | 5 |
| `module/approval` | 0 | 6 | 3 | 3 |
| `module/auth` | 5 | 0 | 3 | 11 |
| `module/billing` | 0 | 9 | 3 | 11 |
| `module/chat` | 17 | 0 | 3 | 24 |
| `module/checklist` | 5 | 0 | 3 | 5 |
| `module/health` | 4 | 0 | 3 | 6 |
| `module/integration` | 17 | 0 | 3 | 4 |
| `module/llm` | 12 | 0 | 5 | 19 |
| `module/memory` | 10 | 3 | 3 | 12 |
| `module/order` | 0 | 5 | 3 | 6 |
| `module/retention` | 0 | 6 | 3 | 3 |
| `module/routine` | 8 | 0 | 3 | 12 |
| `module/task` | 16 | 0 | 3 | 11 |
| `module/vcomputer` | 4 | 0 | 0 | 3 |
| `module/whatsapp` | 0 | 10 | 3 | 5 |
| `module/workspace` | 7 | 0 | 3 | 8 |
| `platform/attachtext` | 2 | 0 | 0 | 1 |
| `platform/authn` | 3 | 0 | 0 | 3 |
| `platform/config` | 4 | 0 | 0 | 1 |
| `platform/crypto` | 1 | 0 | 0 | 1 |
| `platform/database` | 3 | 0 | 0 | 1 |
| `platform/kms` | 1 | 0 | 0 | 1 |
| `platform/llm` | 11 | 0 | 0 | 9 |
| `platform/logger` | 3 | 0 | 0 | 2 |
| `platform/mail` | 3 | 0 | 0 | 3 |
| `platform/mcp` | 1 | 0 | 0 | 1 |
| `platform/metrics` | 3 | 0 | 0 | 1 |
| `platform/middleware` | 3 | 0 | 0 | 3 |
| `platform/migrate` | 1 | 0 | 0 | 1 |
| `platform/oauth` | 1 | 0 | 0 | 1 |
| `platform/oauthstate` | 1 | 0 | 0 | 1 |
| `platform/payment` | 1 | 0 | 0 | 1 |
| `platform/pubsub` | 1 | 0 | 0 | 0 |
| `platform/quota` | 1 | 0 | 0 | 1 |
| `platform/ratelimit` | 1 | 0 | 0 | 1 |
| `platform/redis` | 1 | 0 | 0 | 1 |
| `platform/response` | 1 | 0 | 0 | 1 |
| `platform/routine` | 1 | 0 | 0 | 1 |
| `platform/storage` | 2 | 0 | 0 | 1 |
| `platform/telemetry` | 1 | 0 | 0 | 1 |
| `platform/temporal` | 3 | 0 | 0 | 3 |

## File-by-file

| Go | TypeScript | status |
| --- | --- | --- |
| `apps/backend/cmd/agent-worker/main.go` | `apps/backend-js/src/cmd/agent-worker/main.ts` | missing |
| `apps/backend/cmd/api/main.go` | `apps/backend-js/src/cmd/api/main.ts` | missing |
| `apps/backend/cmd/integration-worker/main.go` | `apps/backend-js/src/cmd/integration-worker/main.ts` | missing |
| `apps/backend/cmd/vcomputer-smoke/main.go` | `apps/backend-js/src/cmd/vcomputer-smoke/main.ts` | missing |
| `apps/backend/cmd/vcomputer-smoke/main_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/admin/domain/admin.go` | `apps/backend-js/src/modules/admin/domain/admin.ts` | ported |
| `apps/backend/internal/modules/admin/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/admin/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/admin/handler/handler.go` | `apps/backend-js/src/modules/admin/handler/handler.ts` | ported |
| `apps/backend/internal/modules/admin/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/admin/repository/postgres.go` | `apps/backend-js/src/modules/admin/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/admin/repository/sqlcgen/admin.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/admin/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/admin/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/admin/service/service.go` | `apps/backend-js/src/modules/admin/service/service.ts` | ported |
| `apps/backend/internal/modules/admin/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/agent/domain/agent.go` | `apps/backend-js/src/modules/agent/domain/agent.ts` | ported |
| `apps/backend/internal/modules/agent/domain/mocks/mock_PlanLimits.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/agent/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/agent/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/agent/handler/handler.go` | `apps/backend-js/src/modules/agent/handler/handler.ts` | ported |
| `apps/backend/internal/modules/agent/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/agent/repository/postgres.go` | `apps/backend-js/src/modules/agent/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/agent/repository/sqlcgen/agent.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/agent/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/agent/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/agent/service/limits.go` | `apps/backend-js/src/modules/agent/service/limits.ts` | ported |
| `apps/backend/internal/modules/agent/service/service.go` | `apps/backend-js/src/modules/agent/service/service.ts` | ported |
| `apps/backend/internal/modules/agent/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/approval/domain/approval.go` | `apps/backend-js/src/modules/approval/domain/approval.ts` | missing |
| `apps/backend/internal/modules/approval/domain/service.go` | `apps/backend-js/src/modules/approval/domain/service.ts` | missing |
| `apps/backend/internal/modules/approval/handler/handler.go` | `apps/backend-js/src/modules/approval/handler/handler.ts` | missing |
| `apps/backend/internal/modules/approval/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/approval/repository/postgres.go` | `apps/backend-js/src/modules/approval/repository/postgres.ts` | missing |
| `apps/backend/internal/modules/approval/repository/sqlcgen/approval.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/approval/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/approval/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/approval/service/digest.go` | `apps/backend-js/src/modules/approval/service/digest.ts` | missing |
| `apps/backend/internal/modules/approval/service/metrics_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/approval/service/service.go` | `apps/backend-js/src/modules/approval/service/service.ts` | missing |
| `apps/backend/internal/modules/approval/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/auth/domain/auth.go` | `apps/backend-js/src/modules/auth/domain/auth.ts` | ported |
| `apps/backend/internal/modules/auth/domain/mocks/mock_GoogleProvider.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_LoginThrottle.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_Mailer.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_Passwords.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_StateSigner.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_TokenIssuer.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/mocks/mock_Tokens.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/auth/domain/ports.go` | `apps/backend-js/src/modules/auth/domain/ports.ts` | ported |
| `apps/backend/internal/modules/auth/handler/handler.go` | `apps/backend-js/src/modules/auth/handler/handler.ts` | ported |
| `apps/backend/internal/modules/auth/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/auth/repository/postgres.go` | `apps/backend-js/src/modules/auth/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/auth/repository/sqlcgen/auth.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/auth/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/auth/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/auth/service/service.go` | `apps/backend-js/src/modules/auth/service/service.ts` | ported |
| `apps/backend/internal/modules/auth/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/billing/domain/billing.go` | `apps/backend-js/src/modules/billing/domain/billing.ts` | missing |
| `apps/backend/internal/modules/billing/domain/billing_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/billing/domain/mocks/mock_Activity.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/billing/domain/mocks/mock_Gateway.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/billing/domain/mocks/mock_Notifier.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/billing/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/billing/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/billing/domain/mocks/mock_SpendCounter.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/billing/domain/mocks/mock_UsageReader.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/billing/domain/service.go` | `apps/backend-js/src/modules/billing/domain/service.ts` | missing |
| `apps/backend/internal/modules/billing/handler/handler.go` | `apps/backend-js/src/modules/billing/handler/handler.ts` | missing |
| `apps/backend/internal/modules/billing/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/billing/repository/convert.go` | `apps/backend-js/src/modules/billing/repository/convert.ts` | missing |
| `apps/backend/internal/modules/billing/repository/postgres.go` | `apps/backend-js/src/modules/billing/repository/postgres.ts` | missing |
| `apps/backend/internal/modules/billing/repository/sqlcgen/billing.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/billing/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/billing/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/billing/service/format.go` | `apps/backend-js/src/modules/billing/service/format.ts` | missing |
| `apps/backend/internal/modules/billing/service/payment.go` | `apps/backend-js/src/modules/billing/service/payment.ts` | missing |
| `apps/backend/internal/modules/billing/service/quota.go` | `apps/backend-js/src/modules/billing/service/quota.ts` | missing |
| `apps/backend/internal/modules/billing/service/service.go` | `apps/backend-js/src/modules/billing/service/service.ts` | missing |
| `apps/backend/internal/modules/billing/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/billing/service/sweep_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/domain/block.go` | `apps/backend-js/src/modules/chat/domain/block.ts` | ported |
| `apps/backend/internal/modules/chat/domain/block_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/domain/conversation.go` | `apps/backend-js/src/modules/chat/domain/conversation.ts` | ported |
| `apps/backend/internal/modules/chat/domain/event.go` | `apps/backend-js/src/modules/chat/domain/event.ts` | ported |
| `apps/backend/internal/modules/chat/domain/mocks/mock_AgentDirectory.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_AttachmentRepository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_ConversationRepository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_EventPublisher.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_EventStore.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_MessageRepository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_ObjectStore.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_Responder.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_Router.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/chat/domain/ports.go` | `apps/backend-js/src/modules/chat/domain/ports.ts` | ported |
| `apps/backend/internal/modules/chat/domain/service.go` | `apps/backend-js/src/modules/chat/domain/service.ts` | ported |
| `apps/backend/internal/modules/chat/domain/wire.go` | `apps/backend-js/src/modules/chat/domain/wire.ts` | ported |
| `apps/backend/internal/modules/chat/domain/wire_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/handler/handler.go` | `apps/backend-js/src/modules/chat/handler/handler.ts` | ported |
| `apps/backend/internal/modules/chat/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/handler/stream_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/repository/postgres.go` | `apps/backend-js/src/modules/chat/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/chat/repository/sqlcgen/conversation.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/chat/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/chat/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/chat/service/attachment_context.go` | `apps/backend-js/src/modules/chat/service/attachment_context.ts` | ported |
| `apps/backend/internal/modules/chat/service/attachment_context_repro_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/attachment_context_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/attachment_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/attachment_type.go` | `apps/backend-js/src/modules/chat/service/attachment_type.ts` | ported |
| `apps/backend/internal/modules/chat/service/coverage_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/fakes_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/html_probe_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/memory.go` | `apps/backend-js/src/modules/chat/service/memory.ts` | ported |
| `apps/backend/internal/modules/chat/service/memory_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/mention.go` | `apps/backend-js/src/modules/chat/service/mention.ts` | ported |
| `apps/backend/internal/modules/chat/service/mention_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/reply.go` | `apps/backend-js/src/modules/chat/service/reply.ts` | ported |
| `apps/backend/internal/modules/chat/service/reply_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/chat/service/responder.go` | `apps/backend-js/src/modules/chat/service/responder.ts` | ported |
| `apps/backend/internal/modules/chat/service/router.go` | `apps/backend-js/src/modules/chat/service/router.ts` | ported |
| `apps/backend/internal/modules/chat/service/service.go` | `apps/backend-js/src/modules/chat/service/service.ts` | ported |
| `apps/backend/internal/modules/chat/service/stream.go` | `apps/backend-js/src/modules/chat/service/stream.ts` | ported |
| `apps/backend/internal/modules/chat/service/task_handoff_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/checklist/adapter/adapter.go` | `apps/backend-js/src/modules/checklist/adapter/adapter.ts` | ported |
| `apps/backend/internal/modules/checklist/adapter/adapter_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/checklist/domain/checklist.go` | `apps/backend-js/src/modules/checklist/domain/checklist.ts` | ported |
| `apps/backend/internal/modules/checklist/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/checklist/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/checklist/handler/handler.go` | `apps/backend-js/src/modules/checklist/handler/handler.ts` | ported |
| `apps/backend/internal/modules/checklist/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/checklist/repository/postgres.go` | `apps/backend-js/src/modules/checklist/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/checklist/repository/sqlcgen/checklist.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/checklist/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/checklist/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/checklist/service/service.go` | `apps/backend-js/src/modules/checklist/service/service.ts` | ported |
| `apps/backend/internal/modules/checklist/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/health/domain/health.go` | `apps/backend-js/src/modules/health/domain/health.ts` | ported |
| `apps/backend/internal/modules/health/domain/mocks/mock_Checker.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/health/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/health/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/health/handler/handler.go` | `apps/backend-js/src/modules/health/handler/handler.ts` | ported |
| `apps/backend/internal/modules/health/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/health/repository/postgres.go` | `apps/backend-js/src/modules/health/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/health/repository/postgres_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/health/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/health/repository/sqlcgen/health.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/health/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/health/service/service.go` | `apps/backend-js/src/modules/health/service/service.ts` | ported |
| `apps/backend/internal/modules/health/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/integration/adapters/github/github.go` | `apps/backend-js/src/modules/integration/adapters/github/github.ts` | ported |
| `apps/backend/internal/modules/integration/adapters/github/github_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/integration/adapters/gmail/gmail.go` | `apps/backend-js/src/modules/integration/adapters/gmail/gmail.ts` | ported |
| `apps/backend/internal/modules/integration/adapters/slack/slack.go` | `apps/backend-js/src/modules/integration/adapters/slack/slack.ts` | ported |
| `apps/backend/internal/modules/integration/adapters/slack/slack_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/integration/domain/adapter.go` | `apps/backend-js/src/modules/integration/domain/adapter.ts` | ported |
| `apps/backend/internal/modules/integration/domain/integration.go` | `apps/backend-js/src/modules/integration/domain/integration.ts` | ported |
| `apps/backend/internal/modules/integration/domain/mcp.go` | `apps/backend-js/src/modules/integration/domain/mcp.ts` | ported |
| `apps/backend/internal/modules/integration/domain/mocks/mock_PlanLimits.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/integration/domain/oauth.go` | `apps/backend-js/src/modules/integration/domain/oauth.ts` | ported |
| `apps/backend/internal/modules/integration/domain/webhook.go` | `apps/backend-js/src/modules/integration/domain/webhook.ts` | ported |
| `apps/backend/internal/modules/integration/handler/handler.go` | `apps/backend-js/src/modules/integration/handler/handler.ts` | ported |
| `apps/backend/internal/modules/integration/repository/postgres.go` | `apps/backend-js/src/modules/integration/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/integration/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/integration/repository/sqlcgen/integration.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/integration/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/integration/service/audit.go` | `apps/backend-js/src/modules/integration/service/audit.ts` | ported |
| `apps/backend/internal/modules/integration/service/link.go` | `apps/backend-js/src/modules/integration/service/link.ts` | ported |
| `apps/backend/internal/modules/integration/service/mcp.go` | `apps/backend-js/src/modules/integration/service/mcp.ts` | ported |
| `apps/backend/internal/modules/integration/service/oauth.go` | `apps/backend-js/src/modules/integration/service/oauth.ts` | ported |
| `apps/backend/internal/modules/integration/service/registry.go` | `apps/backend-js/src/modules/integration/service/registry.ts` | ported |
| `apps/backend/internal/modules/integration/service/registry_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/integration/service/service.go` | `apps/backend-js/src/modules/integration/service/service.ts` | ported |
| `apps/backend/internal/modules/integration/service/webhook.go` | `apps/backend-js/src/modules/integration/service/webhook.ts` | ported |
| `apps/backend/internal/modules/llm/domain/gateway.go` | `apps/backend-js/src/modules/llm/domain/gateway.ts` | ported |
| `apps/backend/internal/modules/llm/domain/mocks/mock_AgentStore.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_CostTable.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_Factory.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_Gateway.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_Provider.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_QuotaChecker.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_QuotaCounter.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_QuotaPolicy.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_Resolver.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_SecretBox.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_Store.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_UsageReader.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/mocks/mock_UsageRecorder.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/llm/domain/provider.go` | `apps/backend-js/src/modules/llm/domain/provider.ts` | ported |
| `apps/backend/internal/modules/llm/handler/handler.go` | `apps/backend-js/src/modules/llm/handler/handler.ts` | ported |
| `apps/backend/internal/modules/llm/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/llm/handler/report.go` | `apps/backend-js/src/modules/llm/handler/report.ts` | ported |
| `apps/backend/internal/modules/llm/handler/report_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/llm/repository/postgres.go` | `apps/backend-js/src/modules/llm/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/llm/repository/sqlcgen/agent_override.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/llm/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/llm/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/llm/repository/sqlcgen/provider.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/llm/repository/sqlcgen/usage.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/llm/service/audit.go` | `apps/backend-js/src/modules/llm/service/audit.ts` | ported |
| `apps/backend/internal/modules/llm/service/embed.go` | `apps/backend-js/src/modules/llm/service/embed.ts` | ported |
| `apps/backend/internal/modules/llm/service/gateway.go` | `apps/backend-js/src/modules/llm/service/gateway.ts` | ported |
| `apps/backend/internal/modules/llm/service/gateway_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/llm/service/metrics_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/llm/service/quota.go` | `apps/backend-js/src/modules/llm/service/quota.ts` | ported |
| `apps/backend/internal/modules/llm/service/quota_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/llm/service/resolver.go` | `apps/backend-js/src/modules/llm/service/resolver.ts` | ported |
| `apps/backend/internal/modules/llm/service/service.go` | `apps/backend-js/src/modules/llm/service/service.ts` | ported |
| `apps/backend/internal/modules/llm/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/llm/service/tracing.go` | `apps/backend-js/src/modules/llm/service/tracing.ts` | ported |
| `apps/backend/internal/modules/memory/activity/activity.go` | `apps/backend-js/src/modules/memory/activity/activity.ts` | missing |
| `apps/backend/internal/modules/memory/activity/activity_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/memory/domain/memory.go` | `apps/backend-js/src/modules/memory/domain/memory.ts` | ported |
| `apps/backend/internal/modules/memory/domain/mocks/mock_Auditor.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/memory/domain/mocks/mock_Completer.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/memory/domain/mocks/mock_Conversation.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/memory/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/memory/domain/mocks/mock_Scheduler.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/memory/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/memory/domain/service.go` | `apps/backend-js/src/modules/memory/domain/service.ts` | ported |
| `apps/backend/internal/modules/memory/domain/wire_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/memory/handler/handler.go` | `apps/backend-js/src/modules/memory/handler/handler.ts` | missing |
| `apps/backend/internal/modules/memory/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/memory/repository/convert.go` | `apps/backend-js/src/modules/memory/repository/convert.ts` | ported |
| `apps/backend/internal/modules/memory/repository/postgres.go` | `apps/backend-js/src/modules/memory/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/memory/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/memory/repository/sqlcgen/memory.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/memory/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/memory/service/chunk.go` | `apps/backend-js/src/modules/memory/service/chunk.ts` | ported |
| `apps/backend/internal/modules/memory/service/extract.go` | `apps/backend-js/src/modules/memory/service/extract.ts` | ported |
| `apps/backend/internal/modules/memory/service/filter.go` | `apps/backend-js/src/modules/memory/service/filter.ts` | ported |
| `apps/backend/internal/modules/memory/service/filter_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/memory/service/recall.go` | `apps/backend-js/src/modules/memory/service/recall.ts` | ported |
| `apps/backend/internal/modules/memory/service/remember.go` | `apps/backend-js/src/modules/memory/service/remember.ts` | ported |
| `apps/backend/internal/modules/memory/service/service.go` | `apps/backend-js/src/modules/memory/service/service.ts` | ported |
| `apps/backend/internal/modules/memory/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/memory/workflow/workflow.go` | `apps/backend-js/src/modules/memory/workflow/workflow.ts` | missing |
| `apps/backend/internal/modules/memory/workflow/workflow_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/order/adapter/adapter.go` | `apps/backend-js/src/modules/order/adapter/adapter.ts` | missing |
| `apps/backend/internal/modules/order/adapter/adapter_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/order/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/order/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/order/domain/order.go` | `apps/backend-js/src/modules/order/domain/order.ts` | missing |
| `apps/backend/internal/modules/order/domain/order_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/order/handler/handler.go` | `apps/backend-js/src/modules/order/handler/handler.ts` | missing |
| `apps/backend/internal/modules/order/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/order/repository/postgres.go` | `apps/backend-js/src/modules/order/repository/postgres.ts` | missing |
| `apps/backend/internal/modules/order/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/order/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/order/repository/sqlcgen/order.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/order/service/service.go` | `apps/backend-js/src/modules/order/service/service.ts` | missing |
| `apps/backend/internal/modules/order/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/retention/domain/erasure.go` | `apps/backend-js/src/modules/retention/domain/erasure.ts` | missing |
| `apps/backend/internal/modules/retention/domain/retention.go` | `apps/backend-js/src/modules/retention/domain/retention.ts` | missing |
| `apps/backend/internal/modules/retention/handler/handler.go` | `apps/backend-js/src/modules/retention/handler/handler.ts` | missing |
| `apps/backend/internal/modules/retention/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/retention/repository/postgres.go` | `apps/backend-js/src/modules/retention/repository/postgres.ts` | missing |
| `apps/backend/internal/modules/retention/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/retention/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/retention/repository/sqlcgen/retention.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/retention/service/erasure.go` | `apps/backend-js/src/modules/retention/service/erasure.ts` | missing |
| `apps/backend/internal/modules/retention/service/erasure_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/retention/service/service.go` | `apps/backend-js/src/modules/retention/service/service.ts` | missing |
| `apps/backend/internal/modules/retention/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/routine/activity/activity.go` | `apps/backend-js/src/modules/routine/activity/activity.ts` | ported |
| `apps/backend/internal/modules/routine/activity/activity_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/routine/domain/mocks/mock_Auditor.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/routine/domain/mocks/mock_Engine.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/routine/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/routine/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/routine/domain/mocks/mock_TaskStarter.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/routine/domain/mocks/mock_WorkQueue.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/routine/domain/mocks/mock_WorkspaceClock.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/routine/domain/routine.go` | `apps/backend-js/src/modules/routine/domain/routine.ts` | ported |
| `apps/backend/internal/modules/routine/domain/routine_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/routine/domain/service.go` | `apps/backend-js/src/modules/routine/domain/service.ts` | ported |
| `apps/backend/internal/modules/routine/handler/handler.go` | `apps/backend-js/src/modules/routine/handler/handler.ts` | ported |
| `apps/backend/internal/modules/routine/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/routine/repository/convert.go` | `apps/backend-js/src/modules/routine/repository/convert.ts` | ported |
| `apps/backend/internal/modules/routine/repository/postgres.go` | `apps/backend-js/src/modules/routine/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/routine/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/routine/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/routine/repository/sqlcgen/routine.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/routine/service/service.go` | `apps/backend-js/src/modules/routine/service/service.ts` | ported |
| `apps/backend/internal/modules/routine/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/routine/workflow/workflow.go` | `apps/backend-js/src/modules/routine/workflow/workflow.ts` | ported |
| `apps/backend/internal/modules/routine/workflow/workflow_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/activity/activity.go` | `apps/backend-js/src/modules/task/activity/activity.ts` | ported |
| `apps/backend/internal/modules/task/activity/activity_behavior_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/activity/activity_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/activity/attachments.go` | `apps/backend-js/src/modules/task/activity/attachments.ts` | ported |
| `apps/backend/internal/modules/task/activity/attachments_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/activity/helpers.go` | `apps/backend-js/src/modules/task/activity/helpers.ts` | ported |
| `apps/backend/internal/modules/task/activity/metrics.go` | `apps/backend-js/src/modules/task/activity/metrics.ts` | ported |
| `apps/backend/internal/modules/task/activity/metrics_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/activity/tracing.go` | `apps/backend-js/src/modules/task/activity/tracing.ts` | ported |
| `apps/backend/internal/modules/task/activity/tracing_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/domain/render.go` | `apps/backend-js/src/modules/task/domain/render.ts` | ported |
| `apps/backend/internal/modules/task/domain/render_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/domain/service.go` | `apps/backend-js/src/modules/task/domain/service.ts` | ported |
| `apps/backend/internal/modules/task/domain/task.go` | `apps/backend-js/src/modules/task/domain/task.ts` | ported |
| `apps/backend/internal/modules/task/domain/usage.go` | `apps/backend-js/src/modules/task/domain/usage.ts` | ported |
| `apps/backend/internal/modules/task/handler/attention.go` | `apps/backend-js/src/modules/task/handler/attention.ts` | ported |
| `apps/backend/internal/modules/task/handler/attention_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/handler/handler.go` | `apps/backend-js/src/modules/task/handler/handler.ts` | ported |
| `apps/backend/internal/modules/task/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/repository/postgres.go` | `apps/backend-js/src/modules/task/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/task/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/task/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/task/repository/sqlcgen/task.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/task/service/fakes_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/service/service.go` | `apps/backend-js/src/modules/task/service/service.ts` | ported |
| `apps/backend/internal/modules/task/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/task/service/wire.go` | `apps/backend-js/src/modules/task/service/wire.ts` | ported |
| `apps/backend/internal/modules/task/workflow/prompt.go` | `apps/backend-js/src/modules/task/workflow/prompt.ts` | ported |
| `apps/backend/internal/modules/task/workflow/workflow.go` | `apps/backend-js/src/modules/task/workflow/workflow.ts` | ported |
| `apps/backend/internal/modules/task/workflow/workflow_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/vcomputer/domain/ports.go` | `apps/backend-js/src/modules/vcomputer/domain/ports.ts` | ported |
| `apps/backend/internal/modules/vcomputer/service/client.go` | `apps/backend-js/src/modules/vcomputer/service/client.ts` | ported |
| `apps/backend/internal/modules/vcomputer/service/client_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/vcomputer/temporal/activities.go` | `apps/backend-js/src/modules/vcomputer/temporal/activities.ts` | ported |
| `apps/backend/internal/modules/vcomputer/temporal/activities_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/vcomputer/workflow/smoke.go` | `apps/backend-js/src/modules/vcomputer/workflow/smoke.ts` | ported |
| `apps/backend/internal/modules/vcomputer/workflow/smoke_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/whatsapp/adapter/adapter.go` | `apps/backend-js/src/modules/whatsapp/adapter/adapter.ts` | missing |
| `apps/backend/internal/modules/whatsapp/adapter/adapter_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/whatsapp/handler/handler.go` | `apps/backend-js/src/modules/whatsapp/handler/handler.ts` | missing |
| `apps/backend/internal/modules/whatsapp/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/whatsapp/handler/qr.go` | `apps/backend-js/src/modules/whatsapp/handler/qr.ts` | missing |
| `apps/backend/internal/modules/whatsapp/handler/qr_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/whatsapp/handler/stream.go` | `apps/backend-js/src/modules/whatsapp/handler/stream.ts` | missing |
| `apps/backend/internal/modules/whatsapp/repository/convert.go` | `apps/backend-js/src/modules/whatsapp/repository/convert.ts` | missing |
| `apps/backend/internal/modules/whatsapp/repository/links.go` | `apps/backend-js/src/modules/whatsapp/repository/links.ts` | missing |
| `apps/backend/internal/modules/whatsapp/repository/postgres.go` | `apps/backend-js/src/modules/whatsapp/repository/postgres.ts` | missing |
| `apps/backend/internal/modules/whatsapp/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/whatsapp/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/whatsapp/repository/sqlcgen/whatsapp.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/whatsapp/service/messages.go` | `apps/backend-js/src/modules/whatsapp/service/messages.ts` | missing |
| `apps/backend/internal/modules/whatsapp/service/reads_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/whatsapp/service/service.go` | `apps/backend-js/src/modules/whatsapp/service/service.ts` | missing |
| `apps/backend/internal/modules/whatsapp/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/whatsapp/service/transport.go` | `apps/backend-js/src/modules/whatsapp/service/transport.ts` | missing |
| `apps/backend/internal/modules/workspace/domain/mocks/mock_Mailer.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/workspace/domain/mocks/mock_PlanLimits.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/workspace/domain/mocks/mock_Provisioner.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/workspace/domain/mocks/mock_Repository.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/workspace/domain/mocks/mock_Service.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/workspace/domain/mocks/mock_Tokens.go` | `—` | skipped — generated mock |
| `apps/backend/internal/modules/workspace/domain/ports.go` | `apps/backend-js/src/modules/workspace/domain/ports.ts` | ported |
| `apps/backend/internal/modules/workspace/domain/workspace.go` | `apps/backend-js/src/modules/workspace/domain/workspace.ts` | ported |
| `apps/backend/internal/modules/workspace/handler/handler.go` | `apps/backend-js/src/modules/workspace/handler/handler.ts` | ported |
| `apps/backend/internal/modules/workspace/handler/handler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/modules/workspace/repository/postgres.go` | `apps/backend-js/src/modules/workspace/repository/postgres.ts` | ported |
| `apps/backend/internal/modules/workspace/repository/sqlcgen/db.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/workspace/repository/sqlcgen/models.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/workspace/repository/sqlcgen/workspace.sql.go` | `—` | folded — sqlc-generated; folded into the repository |
| `apps/backend/internal/modules/workspace/service/audit.go` | `apps/backend-js/src/modules/workspace/service/audit.ts` | ported |
| `apps/backend/internal/modules/workspace/service/limits.go` | `apps/backend-js/src/modules/workspace/service/limits.ts` | ported |
| `apps/backend/internal/modules/workspace/service/service.go` | `apps/backend-js/src/modules/workspace/service/service.ts` | ported |
| `apps/backend/internal/modules/workspace/service/service_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/attachtext/attachtext.go` | `apps/backend-js/src/platform/attachtext/attachtext.ts` | ported |
| `apps/backend/internal/platform/attachtext/attachtext_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/attachtext/pdf.go` | `apps/backend-js/src/platform/attachtext/pdf.ts` | ported |
| `apps/backend/internal/platform/authn/password.go` | `apps/backend-js/src/platform/authn/index.ts` | ported — consolidated into one module |
| `apps/backend/internal/platform/authn/password_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/authn/state.go` | `apps/backend-js/src/platform/authn/index.ts` | ported — consolidated into one module |
| `apps/backend/internal/platform/authn/state_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/authn/token.go` | `apps/backend-js/src/platform/authn/index.ts` | ported — consolidated into one module |
| `apps/backend/internal/platform/authn/token_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/anthropic/client.go` | `apps/backend-js/src/platform/llm/anthropic/client.ts` | ported |
| `apps/backend/internal/platform/llm/anthropic/contract_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/anthropic/embed.go` | `apps/backend-js/src/platform/llm/anthropic/embed.ts` | ported |
| `apps/backend/internal/platform/llm/anthropic/embed_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/anthropic/wire.go` | `apps/backend-js/src/platform/llm/anthropic/wire.ts` | ported |
| `apps/backend/internal/platform/llm/doc.go` | `apps/backend-js/src/platform/llm/doc.ts` | ported |
| `apps/backend/internal/platform/llm/llmtest/contract.go` | `—` | skipped — test support only |
| `apps/backend/internal/platform/llm/openai/client.go` | `apps/backend-js/src/platform/llm/openai/client.ts` | ported |
| `apps/backend/internal/platform/llm/openai/contract_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/openai/embed.go` | `apps/backend-js/src/platform/llm/openai/embed.ts` | ported |
| `apps/backend/internal/platform/llm/openai/embed_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/openai/wire.go` | `apps/backend-js/src/platform/llm/openai/wire.ts` | ported |
| `apps/backend/internal/platform/llm/pricing/table.go` | `apps/backend-js/src/platform/llm/pricing/table.ts` | ported |
| `apps/backend/internal/platform/llm/pricing/table_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/registry.go` | `apps/backend-js/src/platform/llm/registry.ts` | ported |
| `apps/backend/internal/platform/llm/registry_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/sse/sse.go` | `apps/backend-js/src/platform/llm/sse/sse.ts` | ported |
| `apps/backend/internal/platform/llm/sse/sse_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/llm/vision/vision.go` | `apps/backend-js/src/platform/llm/vision/vision.ts` | ported |
| `apps/backend/internal/platform/llm/vision/vision_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/mail/async.go` | `apps/backend-js/src/platform/mail/async.ts` | ported |
| `apps/backend/internal/platform/mail/async_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/mail/mail.go` | `apps/backend-js/src/platform/mail/mail.ts` | ported |
| `apps/backend/internal/platform/mail/mail_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/mail/smtp.go` | `apps/backend-js/src/platform/mail/smtp.ts` | ported |
| `apps/backend/internal/platform/mail/smtp_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/mcp/client.go` | `apps/backend-js/src/platform/mcp/client.ts` | ported |
| `apps/backend/internal/platform/mcp/client_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/middleware/helpers_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/middleware/identity.go` | `apps/backend-js/src/platform/middleware/identity.ts` | ported |
| `apps/backend/internal/platform/middleware/request_context.go` | `apps/backend-js/src/platform/middleware/request_context.ts` | ported |
| `apps/backend/internal/platform/middleware/request_context_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/middleware/tracing.go` | `apps/backend-js/src/platform/middleware/tracing.ts` | ported |
| `apps/backend/internal/platform/middleware/tracing_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/migrate/migrate.go` | `apps/backend-js/src/cmd/migrate.ts` | ported — consolidated into one module |
| `apps/backend/internal/platform/migrate/migrate_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/oauth/google/google.go` | `apps/backend-js/src/platform/oauth/google/google.ts` | ported |
| `apps/backend/internal/platform/oauth/google/google_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/oauthstate/state.go` | `apps/backend-js/src/platform/oauthstate/state.ts` | ported |
| `apps/backend/internal/platform/oauthstate/state_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/payment/duitku/duitku.go` | `apps/backend-js/src/platform/payment/duitku/duitku.ts` | ported |
| `apps/backend/internal/platform/payment/duitku/duitku_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/pubsub/pubsub.go` | `apps/backend-js/src/platform/pubsub/pubsub.ts` | ported |
| `apps/backend/internal/platform/quota/counter.go` | `apps/backend-js/src/platform/quota/counter.ts` | ported |
| `apps/backend/internal/platform/quota/counter_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/ratelimit/ratelimit.go` | `apps/backend-js/src/platform/ratelimit/ratelimit.ts` | ported |
| `apps/backend/internal/platform/ratelimit/ratelimit_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/response/response.go` | `apps/backend-js/src/platform/response.ts` | ported — consolidated into one module |
| `apps/backend/internal/platform/response/response_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/routine/temporal/scheduler.go` | `apps/backend-js/src/platform/routine/temporal/scheduler.ts` | ported |
| `apps/backend/internal/platform/routine/temporal/scheduler_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/temporal/logger.go` | `apps/backend-js/src/platform/temporal/logger.ts` | ported |
| `apps/backend/internal/platform/temporal/logger_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/temporal/ping.go` | `apps/backend-js/src/platform/temporal/ping.ts` | ported |
| `apps/backend/internal/platform/temporal/ping_test.go` | `—` | skipped — test file |
| `apps/backend/internal/platform/temporal/temporal.go` | `apps/backend-js/src/platform/temporal/temporal.ts` | ported |
| `apps/backend/internal/platform/temporal/temporal_test.go` | `—` | skipped — test file |
| `packages/platform/config/config.go` | `apps/backend-js/src/platform/config/config.ts` | ported |
| `packages/platform/config/config_test.go` | `—` | skipped — test file |
| `packages/platform/config/dotenv.go` | `apps/backend-js/src/platform/config/dotenv.ts` | ported |
| `packages/platform/config/init.go` | `apps/backend-js/src/platform/config/init.ts` | ported |
| `packages/platform/config/validation.go` | `apps/backend-js/src/platform/config/validation.ts` | ported |
| `packages/platform/crypto/secretbox.go` | `apps/backend-js/src/platform/crypto/secretbox.ts` | ported |
| `packages/platform/crypto/secretbox_test.go` | `—` | skipped — test file |
| `packages/platform/database/database.go` | `apps/backend-js/src/platform/database/index.ts` | ported — consolidated into one module |
| `packages/platform/database/database_test.go` | `—` | skipped — test file |
| `packages/platform/database/scope.go` | `apps/backend-js/src/platform/database/index.ts` | ported — consolidated into one module |
| `packages/platform/database/tenant.go` | `apps/backend-js/src/platform/database/index.ts` | ported — consolidated into one module |
| `packages/platform/kms/kms.go` | `apps/backend-js/src/platform/kms/kms.ts` | ported |
| `packages/platform/kms/kms_test.go` | `—` | skipped — test file |
| `packages/platform/logger/logger.go` | `apps/backend-js/src/platform/logger.ts` | ported — consolidated into one module |
| `packages/platform/logger/logger_test.go` | `—` | skipped — test file |
| `packages/platform/logger/redact.go` | `apps/backend-js/src/platform/logger.ts` | ported — consolidated into one module |
| `packages/platform/logger/redact_test.go` | `—` | skipped — test file |
| `packages/platform/logger/writer.go` | `apps/backend-js/src/platform/logger.ts` | ported — consolidated into one module |
| `packages/platform/metrics/metrics.go` | `apps/backend-js/src/platform/metrics/metrics.ts` | ported |
| `packages/platform/metrics/metrics_test.go` | `—` | skipped — test file |
| `packages/platform/metrics/recorder.go` | `apps/backend-js/src/platform/metrics/recorder.ts` | ported |
| `packages/platform/metrics/scrape.go` | `apps/backend-js/src/platform/metrics/scrape.ts` | ported |
| `packages/platform/redis/redis.go` | `apps/backend-js/src/platform/redis/redis.ts` | ported |
| `packages/platform/redis/redis_test.go` | `—` | skipped — test file |
| `packages/platform/storage/s3.go` | `apps/backend-js/src/platform/storage/s3.ts` | ported |
| `packages/platform/storage/storage.go` | `apps/backend-js/src/platform/storage/storage.ts` | ported |
| `packages/platform/storage/storage_test.go` | `—` | skipped — test file |
| `packages/platform/telemetry/telemetry.go` | `apps/backend-js/src/platform/telemetry/telemetry.ts` | ported |
| `packages/platform/telemetry/telemetry_test.go` | `—` | skipped — test file |

.PHONY: help up down logs build dev frontend backend api worker-logs wa-gateway \
        test test-backend test-gateway test-packages test-integration smoke-task smoke-approval smoke-order measure-orders demo demo-stop gate-report cover cover-backend cover-gateway lint lint-backend lint-gateway archcheck contracts-check design-tokens-check sqlc mocks generate llm-fixtures \
        migrate-up migrate-down migrate-create psql test-backend-js typecheck-backend-js ci

COMPOSE := docker compose
BACKEND := apps/backend
GATEWAY := apps/whatsapp-gw
PACKAGES := packages

# Three modules: `packages/` is the shared code both processes import, and each
# app is its own binary with its own dependency graph. Every gate below therefore
# runs once per module rather than once for the tree.

help: ## Show available targets
	@echo "Bolu monorepo"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

## --- local stack ------------------------------------------------------------

up: ## Start postgres+pgvector, redis, temporal, migrate, api, workers, frontend
	$(COMPOSE) up -d --build

down: ## Stop and remove the stack
	$(COMPOSE) down

logs: ## Follow logs from all services
	$(COMPOSE) logs -f

build: ## Build all images
	$(COMPOSE) build

dev: up logs ## Start the stack, then follow logs

frontend: ## Run the Next.js dev server on the host
	cd apps/frontend && npm run dev

backend: ## Run the Go API on the host
	cd $(BACKEND) && go run ./cmd/api

api: backend ## Alias for backend

wa-gateway: ## Run the WhatsApp gateway on the host (needs DATABASE_URL, REDIS_URL, WA_API_URL)
	cd $(GATEWAY) && go run ./cmd/wa-gateway

worker-logs: ## Follow the two Temporal workers
	$(COMPOSE) logs -f agent-worker integration-worker

psql: ## Open a psql shell on the compose Postgres
	$(COMPOSE) exec postgres psql -U $${POSTGRES_USER:-employeebot} -d $${POSTGRES_DB:-employeebot}

## --- database ---------------------------------------------------------------

migrate-up: ## Apply all migrations to $$DATABASE_URL
	migrate -path $(BACKEND)/migrations -database "$$DATABASE_URL" up

migrate-down: ## Roll every migration back on $$DATABASE_URL
	migrate -path $(BACKEND)/migrations -database "$$DATABASE_URL" down

migrate-create: ## Create a migration pair: make migrate-create NAME=add_widgets
	@test -n "$(NAME)" || (echo "NAME is required" && exit 1)
	migrate create -ext sql -dir $(BACKEND)/migrations -seq $(NAME)

## --- code generation --------------------------------------------------------

sqlc: ## Generate the type-safe query code from migrations + queries, per module
	cd $(BACKEND) && sqlc generate
	cd $(GATEWAY) && sqlc generate

llm-fixtures: ## Re-record the adapter contract fixtures from the live provider (needs OPENAI_API_KEY or ANTHROPIC_API_KEY)
	cd $(BACKEND) && go run ./tools/llmfixtures -adapter $${ADAPTER:-openai}

mocks: ## Generate the domain mocks used by service tests, per module
	cd $(PACKAGES) && mockery --config .mockery.yaml
	cd $(BACKEND) && mockery --config .mockery.yaml

generate: sqlc mocks ## Regenerate every generated file

## --- quality gates ----------------------------------------------------------

test: test-packages test-backend test-gateway ## Run every module's test suite (integration tests need Docker)

test-packages: ## Run the shared module's tests
	cd $(PACKAGES) && CGO_ENABLED=0 go test ./...

test-backend: ## Run the backend's test suite (integration tests need Docker)
	cd $(BACKEND) && CGO_ENABLED=0 go test ./...

test-gateway: ## Run the gateway's test suite (repository tests need Docker)
	cd $(GATEWAY) && CGO_ENABLED=0 go test ./...

test-integration: ## Run only the backend's testcontainers integration tests
	cd $(BACKEND) && CGO_ENABLED=0 go test ./test/integration/... -count=1

smoke-task: ## Run the task runtime, the bound, and the memory pipeline end to end against the compose stack (needs `make up`)
	cd $(BACKEND) && bash scripts/smoke-task.sh

smoke-approval: ## Run the approval gate end to end against the compose stack (needs `make up`)
	cd $(BACKEND) && bash scripts/smoke-approval.sh

smoke-order: ## Run the MVP order flow end to end against the compose stack (needs `make up`)
	cd $(BACKEND) && bash scripts/smoke-order.sh

measure-orders: ## Measure what one order costs, repeated, from usage_ledger (needs `make up`)
	cd $(BACKEND) && bash scripts/measure-orders.sh $(or $(COUNT),20)

demo: ## Start a stack you can click through, with a workspace, catalogue and Bolu already set up
	bash $(BACKEND)/scripts/demo.sh

gate-report: ## The numbers Gerbang 1 is decided on, read from the database
	cd $(BACKEND) && bash scripts/gate-report.sh

demo-stop: ## Stop what `make demo` started
	@lsof -ti ":11441" -ti ":11442" | xargs -r kill 2>/dev/null || true
	@docker compose -f docker-compose.yml -f docker-compose.demo.yml stop api agent-worker integration-worker

cover: cover-backend cover-gateway ## Run the tests and enforce each module's coverage gate (MIN_COVERAGE, default 80)

cover-backend: ## Backend coverage gate
	cd $(BACKEND) && CGO_ENABLED=0 MIN_COVERAGE=$(or $(MIN_COVERAGE),80) ./scripts/coverage.sh

cover-gateway: ## Gateway coverage gate
	cd $(GATEWAY) && CGO_ENABLED=0 MIN_COVERAGE=$(or $(MIN_COVERAGE),80) ./scripts/coverage.sh

lint: lint-backend lint-gateway ## Run golangci-lint per module

lint-backend: ## Lint the backend
	cd $(BACKEND) && golangci-lint run ./...

lint-gateway: ## Lint the gateway
	cd $(GATEWAY) && golangci-lint run ./...

archcheck: ## Enforce the backend's module dependency rules
	cd $(BACKEND) && go run ./tools/archcheck

contracts-check: ## Fail when the event names drift from docs/api.md
	node packages/contracts/scripts/check.mjs

design-tokens-check: ## Fail when tokens.json drifts from the @theme block in globals.css
	node packages/design-tokens/scripts/check.mjs

test-backend-js: ## Run the Bun port's test suite (what production runs)
	cd apps/backend-js && bun test

typecheck-backend-js: ## Type-check the Bun port (tsc --noEmit)
	cd apps/backend-js && bun run typecheck

fmt: ## Format the Go code of every module
	gofmt -w $(PACKAGES)/platform $(PACKAGES)/whatsapp
	cd $(BACKEND) && gofmt -w ./cmd ./internal ./migrations ./tools ./test
	cd $(GATEWAY) && gofmt -w ./cmd ./internal

ci: contracts-check design-tokens-check archcheck lint test cover test-backend-js typecheck-backend-js ## Run the same gates as CI locally

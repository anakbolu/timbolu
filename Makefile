.PHONY: help up down logs build dev frontend backend api worker-logs \
        test test-integration smoke-task cover lint archcheck sqlc mocks generate llm-fixtures \
        migrate-up migrate-down migrate-create psql ci

COMPOSE := docker compose
BACKEND := apps/backend
MODULE  := github.com/robzlabz/employeebot/apps/backend

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

sqlc: ## Generate the type-safe query code from migrations + queries
	cd $(BACKEND) && sqlc generate

llm-fixtures: ## Re-record the adapter contract fixtures from the live provider (needs OPENAI_API_KEY or ANTHROPIC_API_KEY)
	cd $(BACKEND) && go run ./tools/llmfixtures -adapter $${ADAPTER:-openai}

mocks: ## Generate the domain mocks used by service tests
	cd $(BACKEND) && mockery --config .mockery.yaml

generate: sqlc mocks ## Regenerate every generated file

## --- quality gates ----------------------------------------------------------

test: ## Run the test suite (integration tests need Docker)
	cd $(BACKEND) && CGO_ENABLED=0 go test ./...

test-integration: ## Run only the testcontainers integration tests
	cd $(BACKEND) && CGO_ENABLED=0 go test ./test/integration/... -count=1

smoke-task: ## Run the task runtime end to end against the compose stack (needs `make up`)
	cd $(BACKEND) && bash scripts/smoke-task.sh

cover: ## Run the tests and enforce the coverage gate (MIN_COVERAGE, default 80)
	cd $(BACKEND) && CGO_ENABLED=0 MIN_COVERAGE=$(or $(MIN_COVERAGE),80) ./scripts/coverage.sh

lint: ## Run golangci-lint
	cd $(BACKEND) && golangci-lint run ./...

archcheck: ## Enforce the module dependency rules
	cd $(BACKEND) && go run ./tools/archcheck

fmt: ## Format the Go code
	cd $(BACKEND) && gofmt -w ./cmd ./internal ./migrations ./tools ./test

ci: archcheck lint test ## Run the same gates as CI locally

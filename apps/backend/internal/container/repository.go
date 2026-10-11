package container

import (
	"github.com/jackc/pgx/v5/pgxpool"

	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	chatrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/repository"
	healthrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/repository"
	llmrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/repository"
	taskrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/repository"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// Repositories aggregates the data access layer. Modules register their
// repositories here as they are implemented.
type Repositories struct {
	Health    *healthrepo.Repository
	Auth      *authrepo.Repository
	Workspace *workspacerepo.Repository
	Agent     *agentrepo.Repository
	// LLM holds the provider configuration, the usage ledger, and the Bolu model
	// override; LLMAgents is the same connection with the encryption attached,
	// and is filled in by openLLM.
	LLM       *llmrepo.Repository
	LLMAgents *llmrepo.AgentStore
	// Chat holds conversations, messages, attachments, and the activity stream.
	Chat *chatrepo.Repository
	// Task holds the tasks and the steps they recorded. One connection serves
	// both ports, which is why it is one struct.
	Task *taskrepo.Repository
}

// newRepositories builds every repository. When there is no pool the
// repositories are nil, which the services report as "not configured" instead
// of as a failure: the API must stay usable while Postgres is down.
func newRepositories(pool *database.Pool) *Repositories {
	var pgxPool *pgxpool.Pool
	if pool != nil {
		pgxPool = pool.PgxPool()
	}

	repositories := &Repositories{}
	if pgxPool != nil {
		repositories.Health = healthrepo.New(pgxPool)
		repositories.Auth = authrepo.New(pgxPool)
	}
	if pool != nil {
		// The agent repository needs the scoped helpers, so it takes the pool
		// wrapper rather than the raw connection.
		repositories.Agent = agentrepo.New(pool)

		// The workspace repository is handed the agent repository as its
		// provisioner: onboarding copies the Bolu templates inside its own
		// transaction, and this is the only place that knows both modules.
		repositories.Workspace = workspacerepo.New(pool, repositories.Agent)

		// The model gateway works without a database too: it answers "not
		// configured" rather than failing, exactly like the other modules.
		repositories.LLM = llmrepo.New(pool)

		// The chat repository implements four ports: conversations, messages,
		// attachments, and the event store. They share one connection and one
		// tenant scope, so they are one struct.
		repositories.Chat = chatrepo.New(pool)

		// The task runtime implements two ports — tasks and steps — over the
		// same connection and the same tenant scope.
		repositories.Task = taskrepo.New(pool)
	}
	return repositories
}

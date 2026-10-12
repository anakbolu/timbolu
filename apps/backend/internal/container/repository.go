package container

import (
	"github.com/jackc/pgx/v5/pgxpool"

	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	approvalrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/repository"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	chatrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/repository"
	healthrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/repository"
	integrationrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/repository"
	llmrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/repository"
	memoryrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/repository"
	routinerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/repository"
	taskrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/repository"
	whatsapprepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/whatsapp/repository"
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
	// Integration holds the connected accounts, the workspace key ring, and the
	// per-Bolu grants.
	Integration *integrationrepo.Repository
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
	// Approval holds the drafts, the policies, and the audit trail.
	Approval *approvalrepo.Repository
	// Memory holds the two memories and the knowledge base. The agent rows and
	// the audit trail belong to their own modules; this reads and writes only its
	// own tables.
	Memory *memoryrepo.Repository
	// Routine holds the routines and the runs they produced. The schedule id on a
	// routine is the link to the engine; the row is what the product owns.
	Routine *routinerepo.Repository
	// WhatsApp holds the linked numbers, their sealed sessions, the messages they
	// carried, the outbox that paces them, and the fleet directory.
	WhatsApp *whatsapprepo.Repository
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

		// A new workspace is created empty: its two teams, and no Bolu. The first
		// one is made by a person on the screen that says the team is empty.
		repositories.Workspace = workspacerepo.New(pool)

		// And the auth repository as its onboarding marker, for the same reason:
		// owning a workspace and the account's session believing it must be one
		// fact, written in one transaction.
		repositories.Workspace.SetOnboardedMarker(repositories.Auth)

		// The model gateway works without a database too: it answers "not
		// configured" rather than failing, exactly like the other modules.
		repositories.LLM = llmrepo.New(pool)

		// The chat repository implements four ports: conversations, messages,
		// attachments, and the event store. They share one connection and one
		// tenant scope, so they are one struct.
		repositories.Chat = chatrepo.New(pool)

		// Memory keeps the two layers and the knowledge chunks; it reads the
		// extraction switch from the agent rows and writes the audit trail through
		// its own module's repository.
		repositories.Memory = memoryrepo.New(pool)

		// Routines keep the cadence and the run history. The schedules themselves
		// live in the engine, which is why a routine carries the engine's id.
		repositories.Routine = routinerepo.New(pool)

		// WhatsApp keeps the linked numbers, their sealed sessions, the messages
		// they carried, and the outbox that paces them. It also answers the one
		// unscoped read in the codebase — the fleet directory — which the
		// migration explains.
		repositories.WhatsApp = whatsapprepo.New(pool)

		// The task runtime implements two ports — tasks and steps — over the
		// same connection and the same tenant scope.
		repositories.Task = taskrepo.New(pool)

		// The integration hub holds the tokens, the key ring that seals them, and
		// the grants. It is built before the approval gate, which asks it for a
		// token when an approved action is finally carried out.
		repositories.Integration = integrationrepo.New(pool)

		// The approval module implements the draft, policy, revision, and audit
		// ports over one connection and one tenant scope.
		repositories.Approval = approvalrepo.New(pool)
	}
	return repositories
}

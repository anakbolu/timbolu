package container

import (
	admindomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/admin/domain"
	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	approvaldomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	billingdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/billing/domain"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	checklistdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/checklist/domain"
	healthdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/domain"
	healthservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/service"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	memorydomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	orderdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/order/domain"
	retentiondomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/retention/domain"
	routinedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/routine/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/packages/platform/redis"
	whatsappdomain "github.com/robzlabz/employeebot/packages/whatsapp/domain"
)

// Services aggregates the business layer built on top of Repositories. Every
// field is typed as the module's domain interface, so the container is the only
// package that knows the concrete implementations.
type Services struct {
	Health healthdomain.Service
	// Admin is the operator surface: the accounts a deployment has, and the one
	// decision an admin makes about them. It is built by openAdmin, and stays nil
	// when there is no database to read the account table from.
	Admin admindomain.Service
	// Auth and Workspace are filled in by openAuth, which needs the
	// repositories; they stay nil when the database is not configured.
	Auth      authdomain.Service
	Workspace workspacedomain.Service
	Agent     agentdomain.Service
	// LLM is built by openLLM, which needs the encryption key from the config.
	LLM llmdomain.Gateway
	// Chat is built by openChat, which needs the object storage and the gateway.
	Chat chatdomain.Service
	// Task is built by openTask, which needs the registry and the model gateway.
	Task taskdomain.Service
	// Approval is built by openApproval, which needs the drafts, the cards, and
	// the workspace's working hours.
	Approval approvaldomain.Service
	// Integration is built by openIntegration, which needs the key-management key
	// that seals every stored token.
	Integration integrationdomain.Service
	// Memory is built by openMemory, which needs the model gateway to embed with.
	Memory memorydomain.Service
	// Routine is built by openRoutine, which needs the task runtime to hand a run's
	// work to and the engine to schedule it with.
	Routine routinedomain.Service
	// WhatsApp is the linked numbers and who answers them. The API reaches it
	// through the gateway; the gateway process serves it from the same module.
	WhatsApp whatsappdomain.Service
	// Billing is the package a workspace is on, its allowance, and what it owes. It
	// is built before the model gateway, because the gateway's quota policy is this
	// module's answer.
	Billing billingdomain.Service
	// Retention is the content windows: what the deployment removes once it is old
	// enough, and what it keeps. It is built after the chat service, whose attachments
	// it removes, and after the task repository, whose step contents it removes.
	Retention retentiondomain.Service
	// Order is the workspace's own commerce data: the catalogue a person maintains,
	// the customers it talks to, and the orders a Bolu recorded. It is reached both
	// ways — a screen maintains the catalogue over HTTP, and a tool reads it and
	// writes an order — which is why it is a module rather than a route or an
	// adapter.
	Order orderdomain.Service
	// Checklist is the plan a Bolu keeps for itself, built by openChecklist. The
	// integration adapter resolves it when a tool is called, because the adapter is
	// registered with the hub before this module is assembled.
	Checklist checklistdomain.Service
}

// newServices builds every service. Optional dependencies (Redis today) are
// passed as nil when they are not configured.
func newServices(repositories *Repositories, cache *redis.Client) *Services {
	// A nil *Repository wrapped in the domain interface is not nil, so the
	// interfaces are only assigned when the dependency really exists. Otherwise
	// the health service would call Ping on a nil repository and report the
	// database as down instead of not configured.
	var database healthdomain.Repository
	if repositories != nil && repositories.Health != nil {
		database = repositories.Health
	}

	var cacheChecker healthdomain.Checker
	if cache != nil {
		cacheChecker = cache
	}

	return &Services{
		Health: healthservice.New(database, cacheChecker),
	}
}

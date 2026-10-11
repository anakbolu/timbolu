package container

import (
	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
	chatdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/chat/domain"
	healthdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/domain"
	healthservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/health/service"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	taskdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/redis"
)

// Services aggregates the business layer built on top of Repositories. Every
// field is typed as the module's domain interface, so the container is the only
// package that knows the concrete implementations.
type Services struct {
	Health healthdomain.Service
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

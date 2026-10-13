package service

import "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"

// UseAuditor installs the trail writer a model configuration change is recorded through.
//
// It exists for the reason the other modules' installers do: the gateway is assembled
// before the approval module's repository is wired into the container, and all of that
// happens before any request is served. Left unwired, a provider change happens and leaves
// no line.
func UseAuditor(target domain.Gateway, auditor domain.Auditor) {
	if concrete, ok := target.(*Service); ok {
		concrete.deps.Auditor = auditor
	}
}

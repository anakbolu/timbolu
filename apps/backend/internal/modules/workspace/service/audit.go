package service

import "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"

// UseAuditor installs the trail writer a membership change is recorded through.
//
// It exists for the reason UsePlanLimits does: the tenancy services are assembled before
// the approval module's repository is wired into the container, and all of that happens
// before any request is served. A service left unwired records nothing, which is what a
// deployment without the approval module runs with — the change happens and leaves no line.
func UseAuditor(target domain.Service, auditor domain.Auditor) {
	if concrete, ok := target.(*service); ok {
		concrete.auditor = auditor
	}
}

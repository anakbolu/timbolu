package service

import "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"

// UsePlanLimits installs the workspace's package as the registry's cap.
//
// It exists because of the order things are built in: the registry is assembled with
// the rest of the tenancy services, and the billing module that knows what a package
// allows is assembled after them. Everything between the two moments is a process
// starting up, before any request is served, so there is no window in which a caller
// sees a half-wired limit — and a registry left unwired is a registry whose own
// configured cap applies, which is what a deployment without billing runs with.
func UsePlanLimits(target domain.Service, limits domain.PlanLimits) {
	if concrete, ok := target.(*service); ok {
		concrete.limits = limits
	}
}

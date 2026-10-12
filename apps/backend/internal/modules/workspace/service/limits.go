package service

import "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"

// UsePlanLimits installs the workspace's package as the member cap.
//
// It exists for the reason the registry's is: the tenancy services are assembled
// before the billing module that knows what a package allows, and all of that happens
// before any request is served. A workspace service left unwired caps nothing, which
// is what a deployment without billing runs with.
func UsePlanLimits(target domain.Service, limits domain.PlanLimits) {
	if concrete, ok := target.(*service); ok {
		concrete.limits = limits
	}
}

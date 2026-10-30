package service

import "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"

// UseAuditor installs the trail writer a grant change is recorded through.
//
// It exists for the reason the workspace module's does: the hub is assembled before the
// approval module's repository is wired into the container, and all of that happens before
// any request is served. Left unwired, a grant change happens and leaves no line.
func UseAuditor(target domain.Service, auditor domain.Auditor) {
	if concrete, ok := target.(*service); ok {
		concrete.deps.Auditor = auditor
	}
}

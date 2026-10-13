package container

import (
	"context"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/approval/domain"
	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	integrationservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/service"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	llmservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/service"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspaceservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/service"
)

// The audit adapters: three modules that record a sensitive action and none of which owns
// the table it lands in.
//
// The approval module owns `audit_logs`, so it is the module that writes it, and the
// container is the only package allowed to know both sides — which is what these three
// types are for. They are separate rather than one generic adapter because each module
// declares its own entry shape, and the translation between them is exactly where a field
// can be dropped: a trail that silently loses the actor is worse than one that refuses to
// compile.
//
// The service ports are installed after construction rather than passed to `New`, because
// the services are assembled before the approval repository is wired into the container.
// Everything between those two moments is a process starting up, before any request is
// served, so there is no window in which a caller records nothing.

// UseWorkspaceAuditor installs the trail writer on the workspace service.
func (c *Container) UseWorkspaceAuditor() {
	if c == nil || c.Services.Workspace == nil || c.Repositories == nil || c.Repositories.Approval == nil {
		return
	}
	workspaceservice.UseAuditor(c.Services.Workspace, workspaceAuditor{repository: c.Repositories.Approval})
}

// UseIntegrationAuditor installs the trail writer on the integration hub.
func (c *Container) UseIntegrationAuditor() {
	if c == nil || c.Services.Integration == nil || c.Repositories == nil || c.Repositories.Approval == nil {
		return
	}
	integrationservice.UseAuditor(c.Services.Integration, integrationAuditor{repository: c.Repositories.Approval})
}

// UseModelAuditor installs the trail writer on the model gateway.
func (c *Container) UseModelAuditor() {
	if c == nil || c.Services.LLM == nil || c.Repositories == nil || c.Repositories.Approval == nil {
		return
	}
	llmservice.UseAuditor(c.Services.LLM, modelAuditor{repository: c.Repositories.Approval})
}

// workspaceAuditor writes a workspace module entry into the approval module's trail.
type workspaceAuditor struct {
	repository auditRecorder
}

// Record implements workspacedomain.Auditor.
func (a workspaceAuditor) Record(ctx context.Context, scope workspacedomain.Scope, entry workspacedomain.AuditEntry) error {
	return a.repository.RecordAudit(ctx, domain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, domain.AuditEntry{
		Action:      entry.Action,
		ObjectType:  entry.ObjectType,
		ObjectID:    entry.ObjectID,
		ActorUserID: entry.ActorUserID,
		Metadata:    entry.Metadata,
	})
}

// integrationAuditor writes an integration module entry into the same trail.
type integrationAuditor struct {
	repository auditRecorder
}

// Record implements integrationdomain.Auditor.
func (a integrationAuditor) Record(ctx context.Context, scope integrationdomain.Scope, entry integrationdomain.AuditEntry) error {
	return a.repository.RecordAudit(ctx, domain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, domain.AuditEntry{
		Action:      entry.Action,
		ObjectType:  entry.ObjectType,
		ObjectID:    entry.ObjectID,
		ActorUserID: entry.ActorUserID,
		Metadata:    entry.Metadata,
	})
}

// modelAuditor writes a model gateway entry into the same trail.
type modelAuditor struct {
	repository auditRecorder
}

// Record implements llmdomain.Auditor.
func (a modelAuditor) Record(ctx context.Context, scope llmdomain.Scope, entry llmdomain.AuditEntry) error {
	return a.repository.RecordAudit(ctx, domain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, domain.AuditEntry{
		Action:      entry.Action,
		ObjectType:  entry.ObjectType,
		ObjectID:    entry.ObjectID,
		ActorUserID: entry.ActorUserID,
		Metadata:    entry.Metadata,
	})
}

// compile-time checks that each adapter is the port its module declares.
var (
	_ workspacedomain.Auditor   = workspaceAuditor{}
	_ integrationdomain.Auditor = integrationAuditor{}
	_ llmdomain.Auditor         = modelAuditor{}
)

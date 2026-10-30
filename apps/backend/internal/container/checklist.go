package container

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	checklistadapter "github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/adapter"
	checklistdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain"
	checklistservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/service"
	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// openChecklist builds the module that holds the plan a Bolu keeps for itself: what it has to do, and
// what it has already done.
//
// It is the first module whose subject is the Bolu's own work rather than work a person gave it, and
// that is visible in where it is built. Its *adapter* is registered with the integration hub much
// earlier, in `integrationAdapters`, because that is the one place an application is named — and the
// adapter resolves this service when a tool is called rather than holding it, which is what lets the
// two orders hold.
func (c *Container) openChecklist(_ context.Context, _ *config.Config) error {
	if c.Repositories == nil || c.Repositories.Checklist == nil {
		c.Logger.Info("the checklist module is disabled: no database connection")
		return nil
	}

	service := checklistservice.New(checklistservice.Deps{
		Repository: c.Repositories.Checklist,
		Logger:     c.Logger,
	})
	c.Services.Checklist = service

	return nil
}

// adChecklist is the integration adapter for the Bolu's own plan, registered with the hub beside the
// connected applications.
//
// It is registered when a database exists, because that is what decides whether the module can answer
// at all: a deployment without storage offers no checklist tools rather than offering a tool that
// fails when a model calls it.
func adChecklist(c *Container) integrationdomain.Adapter {
	return checklistadapter.New(checklistPlans{container: c})
}

// checklistPlans resolves the checklist module when a tool is called.
//
// The adapter is installed while the hub is assembled, and the module it serves is assembled after it
// — the same shape as the order and WhatsApp adapters, and for the same reason. Holding the service
// here would hold a nil.
type checklistPlans struct {
	container *Container
}

// live is the service this process has, or nil while it is still starting.
func (p checklistPlans) live() checklistdomain.Service {
	if p.container == nil || p.container.Services == nil {
		return nil
	}

	return p.container.Services.Checklist
}

// Read implements the adapter's port.
func (p checklistPlans) Read(ctx context.Context, scope checklistdomain.Scope, agentID uuid.UUID) (checklistdomain.Checklist, error) {
	service := p.live()
	if service == nil {
		return checklistdomain.Checklist{}, fmt.Errorf("%w: modul rencana belum siap", checklistdomain.ErrInvalidInput)
	}

	return service.Read(ctx, scope, agentID)
}

// Create implements the adapter's port.
func (p checklistPlans) Create(ctx context.Context, scope checklistdomain.Scope, plan checklistdomain.NewChecklist) (checklistdomain.Checklist, error) {
	service := p.live()
	if service == nil {
		return checklistdomain.Checklist{}, fmt.Errorf("%w: modul rencana belum siap", checklistdomain.ErrInvalidInput)
	}

	return service.Create(ctx, scope, plan)
}

// Check implements the adapter's port.
func (p checklistPlans) Check(ctx context.Context, scope checklistdomain.Scope, agentID, itemID uuid.UUID) (checklistdomain.Checklist, error) {
	service := p.live()
	if service == nil {
		return checklistdomain.Checklist{}, fmt.Errorf("%w: modul rencana belum siap", checklistdomain.ErrInvalidInput)
	}

	return service.Check(ctx, scope, agentID, itemID)
}

// Remove implements the adapter's port.
func (p checklistPlans) Remove(ctx context.Context, scope checklistdomain.Scope, agentID, itemID uuid.UUID) (checklistdomain.Checklist, error) {
	service := p.live()
	if service == nil {
		return checklistdomain.Checklist{}, fmt.Errorf("%w: modul rencana belum siap", checklistdomain.ErrInvalidInput)
	}

	return service.Remove(ctx, scope, agentID, itemID)
}

// Clear implements the adapter's port.
func (p checklistPlans) Clear(ctx context.Context, scope checklistdomain.Scope, agentID uuid.UUID) error {
	service := p.live()
	if service == nil {
		return fmt.Errorf("%w: modul rencana belum siap", checklistdomain.ErrInvalidInput)
	}

	return service.Clear(ctx, scope, agentID)
}

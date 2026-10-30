package container

import (
	"context"

	adminservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/admin/service"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// openAdmin builds the operator surface's service.
//
// It needs one repository and nothing else: the surface reads the account table,
// which is the deployment's own, so there is no tenant to resolve and no
// provider to talk to. Without a database the service is left unwired and its
// routes answer 503 rather than 404 — the same rule the other modules follow.
func (c *Container) openAdmin(_ context.Context, _ *config.Config) error {
	if c.Repositories == nil || c.Repositories.Admin == nil {
		c.Logger.Warn("the operator surface is not configured: no database")
		return nil
	}

	c.Services.Admin = adminservice.New(adminservice.Deps{Repository: c.Repositories.Admin})
	return nil
}

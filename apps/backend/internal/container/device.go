package container

import (
	"context"

	deviceservice "github.com/anakbolu/timbolu/apps/backend/internal/modules/device/service"
	"github.com/anakbolu/timbolu/packages/platform/config"
)

// openDevice builds the registry of push addresses phones registered.
//
// It depends on nothing but its own repository: the token is an address, and the module neither
// produces the events a push is about nor sends them. The sender added in a later tick will resolve
// this service, which is why the module is assembled even though nothing reads it yet.
func (c *Container) openDevice(_ context.Context, _ *config.Config) error {
	if c.Repositories == nil || c.Repositories.Device == nil {
		c.Logger.Info("the device registry is disabled: no database connection")
		return nil
	}

	c.Services.Device = deviceservice.New(deviceservice.Deps{
		Repository: c.Repositories.Device,
	})

	return nil
}

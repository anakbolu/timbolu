// Package service implements the device registry's use cases: validating a registration and deciding
// what a delete of somebody else's token means.
//
// It depends only on the module's domain contract, so the rules are testable without a database. The
// rules are small on purpose. What a registration carries is an address, and the only things worth
// refusing are a body that is not a token and a platform the app does not run on.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/device/domain"
)

// Deps are the service dependencies.
type Deps struct {
	Repository domain.Repository
}

// Service is the device registry's implementation.
type Service struct {
	repository domain.Repository
}

// New builds the service.
func New(deps Deps) *Service {
	return &Service{repository: deps.Repository}
}

// Compile-time check that the service is the interface it declares.
var _ domain.Service = (*Service)(nil)

// Register validates a registration and stores it.
//
// The repository is an upsert, so registering the same token twice is one row with a newer
// `last_seen_at`. That is not an optimisation: a phone re-registers on every launch, and a registry
// that grew per launch would hand the future sender the same address several times.
func (s *Service) Register(ctx context.Context, scope domain.Scope, request domain.RegisterRequest) (domain.Device, error) {
	if err := requireScope(scope); err != nil {
		return domain.Device{}, err
	}

	cleaned, err := normalise(request)
	if err != nil {
		return domain.Device{}, err
	}

	return s.repository.Register(ctx, scope, cleaned)
}

// Delete removes one of the caller's tokens.
//
// The token is passed through exactly as the client sent it. Trimming it here would turn a different
// token into this one, and the delete is already the narrowest possible statement: the caller's own
// rows only.
func (s *Service) Delete(ctx context.Context, scope domain.Scope, token string) error {
	if err := requireScope(scope); err != nil {
		return err
	}

	return s.repository.Delete(ctx, scope, token)
}

// ----------------------------------------------------------------- validation

// requireScope refuses a call that names no workspace. The route is tenant-scoped, and a device row
// carries the workspace it was registered from; answering without one would be a write nobody asked
// for.
func requireScope(scope domain.Scope) error {
	if scope.UserID == uuid.Nil || scope.WorkspaceID == uuid.Nil {
		return fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	return nil
}

// normalise trims the fields a client controls and refuses the ones that are not an address.
//
// The platform is lowercased rather than compared case-sensitively: "iOS" from a client is the same
// platform as "ios", and refusing it would be a validation error with no product meaning.
func normalise(request domain.RegisterRequest) (domain.RegisterRequest, error) {
	token := strings.TrimSpace(request.Token)
	switch {
	case token == "":
		return domain.RegisterRequest{}, fmt.Errorf("%w: token perangkat belum diisi", domain.ErrInvalidInput)
	case len(token) > domain.TokenMaxLength:
		return domain.RegisterRequest{}, fmt.Errorf(
			"%w: token perangkat lebih dari %d karakter", domain.ErrInvalidInput, domain.TokenMaxLength)
	}

	platform := strings.ToLower(strings.TrimSpace(request.Platform))
	if !knownPlatform(platform) {
		return domain.RegisterRequest{}, fmt.Errorf(
			"%w: platform harus %s atau %s", domain.ErrInvalidInput, domain.PlatformIOS, domain.PlatformAndroid)
	}

	return domain.RegisterRequest{
		Token:      token,
		Platform:   platform,
		AppVersion: strings.TrimSpace(request.AppVersion),
	}, nil
}

func knownPlatform(platform string) bool {
	for _, known := range domain.Platforms {
		if platform == known {
			return true
		}
	}

	return false
}

package service

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
)

// Linking is the other way an application becomes connected.
//
// `Connect` stores what a provider authorised: an access token, a refresh token,
// and the moment they expire. That is not every application. A linked WhatsApp
// number is paired by a phone, and the session it produces lives with the process
// that holds the connection open — so there is nothing for the token columns to
// hold.
//
// What the hub still owns is everything the product reads: the row a screen
// shows, the grant a Bolu holds, and the approval gate an action passes through
// on its way out. Only the credential's home changes, and this is where that is
// said.

// Link records a connection whose credential this deployment holds itself.
func (s *service) Link(ctx context.Context, scope domain.Scope, input domain.LinkInput) (domain.Integration, error) {
	if err := input.Validate(); err != nil {
		return domain.Integration{}, fmt.Errorf("%w: %w", domain.ErrInvalidInput, err)
	}

	// No cipher is needed and none is required: sealing an empty string to look
	// like a token would make the row read as a connection with a credential it
	// never had, and the next person to debug it would hunt for a token that was
	// never issued.
	stored, err := s.deps.Repository.Upsert(ctx, scope, domain.ConnectInput{
		App:          input.App,
		AccountLabel: input.AccountLabel,
		Scopes:       input.Scopes,
	}, domain.StoredToken{})
	if err != nil {
		return domain.Integration{}, err
	}

	s.deps.Logger.Info("integration linked",
		zap.String("app", stored.App),
		zap.String("account", stored.AccountLabel),
	)

	return stored, nil
}

// needsToken reports whether an adapter has to be handed a stored credential.
//
// The default is yes, and that is deliberate: an adapter that says nothing is the
// common case, and defaulting to "no token needed" would let a mistake in one
// adapter run calls without the credential it was supposed to carry.
func needsToken(adapter domain.Adapter) bool {
	if credentialled, ok := adapter.(domain.Credentialled); ok {
		return credentialled.NeedsToken()
	}
	return true
}

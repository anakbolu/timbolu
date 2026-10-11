package container

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	integrationdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/domain"
	integrationservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/integration/service"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/config"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/crypto"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/kms"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/oauthstate"
)

// openIntegration builds the integration hub: the connected accounts, the key
// ring that seals their tokens, and the grants that say which Bolu may use them.
//
// It is assembled before the approval gate because the gate's executor asks it
// for a token when an approved action is finally carried out.
func (c *Container) openIntegration(_ context.Context, cfg *config.Config, o *options) error {
	if c.Repositories == nil || c.Repositories.Integration == nil {
		c.Logger.Warn("the integration hub is disabled: no database connection")
		return nil
	}

	keys, err := kms.NewLocalFromConfig(
		cfg.Kms.MasterKey,
		cfg.Kms.MasterKeyID,
		cfg.Kms.RetiredKeys,
	)
	if err != nil {
		return fmt.Errorf("integration: key management: %w", err)
	}
	if keys == nil {
		c.Logger.Warn("integration tokens are disabled: no KMS_MASTER_KEY configured")
	} else if retired := keys.RetiredIDs(); len(retired) > 0 {
		// Said out loud because it is the state that follows a rotation: the
		// retired keys are what still opens the connections nobody has re-sealed.
		c.Logger.Info("key management is reading retired keys",
			zap.String("active", keys.ActiveKeyID()),
			zap.Strings("retired", retired),
		)
	}

	// The adapters are registered first: they are what declares the tools, their
	// labels, and their schemas, and everything else reads that.
	registry := integrationservice.NewRegistry()
	for _, adapter := range c.integrationAdapters(o) {
		if err := registry.Register(adapter); err != nil {
			// An adapter that declares something the registry refuses is a bug in
			// that adapter. Failing here names it while the process is starting,
			// rather than letting a model call a tool nobody can run.
			return fmt.Errorf("integration: register adapter: %w", err)
		}
	}

	schemas, err := newToolSchemas(registry)
	if err != nil {
		return err
	}

	// The signing key is what makes a callback trustworthy: the value travels
	// through a browser, so anybody can hand one back.
	states, err := oauthstate.New(cfg.Integrations.OAuthStateKey)
	if err != nil {
		return fmt.Errorf("integration: %w", err)
	}

	c.Services.Integration = integrationservice.New(integrationservice.Deps{
		Repository: c.Repositories.Integration,
		Cipher:     c.tokenCipher(keys),
		Registry:   registry,
		Validator:  schemas,
		States:     states,
		StateStore: c.Repositories.Integration,
		// The same codec signs a webhook reference: both values travel outside
		// the app, and both are bound to a purpose so one cannot be spent as the
		// other.
		Refs:   states,
		Queue:  c.Repositories.Integration,
		Logger: c.Logger,
	})

	return nil
}

// integrationAdapters lists the applications this deployment speaks.
//
// It is the only place an application is named. Adding one is adding an adapter
// here; the agent worker, the approval gate, and the tool registry never learn
// that it happened.
func (c *Container) integrationAdapters(o *options) []integrationdomain.Adapter {
	if o != nil && o.adaptersSet {
		return o.adapters
	}
	return nil
}

// tokenCipher seals and opens tokens with the workspace's data key.
//
// It is the only place a data key is resolved, so "decrypt only when used" is
// one function to read. The key itself never leaves this type: callers hand over
// a plaintext and get a ciphertext back.
type tokenCipher struct {
	keys   integrationdomain.KeyManager
	store  integrationdomain.Repository
	logger *zap.Logger
}

// keyManager adapts the platform key-management package to the port the
// integration domain declares.
//
// The two carry the same idea in their own types, and the translation lives here
// rather than in the platform package because a key ring that knew about the
// integration module could not also serve the WhatsApp session in EPIC 9.
type keyManager struct {
	inner *kms.Local
}

func (k keyManager) Wrap(ctx context.Context, workspaceID uuid.UUID, dataKey []byte) (integrationdomain.WrappedKey, error) {
	wrapped, err := k.inner.Wrap(ctx, workspaceID, dataKey)
	if err != nil {
		return integrationdomain.WrappedKey{}, err
	}
	return integrationdomain.WrappedKey{KeyID: wrapped.KeyID, Bytes: wrapped.Bytes}, nil
}

func (k keyManager) Unwrap(ctx context.Context, workspaceID uuid.UUID, wrapped integrationdomain.WrappedKey) ([]byte, error) {
	return k.inner.Unwrap(ctx, workspaceID, kms.Wrapped{KeyID: wrapped.KeyID, Bytes: wrapped.Bytes})
}

func (k keyManager) ActiveKeyID() string { return k.inner.ActiveKeyID() }

// tokenCipher returns the cipher, or nil when no key-management key is
// configured. A nil cipher is not a nil interface: the service checks for it and
// refuses to store a token rather than writing one in clear text.
func (c *Container) tokenCipher(keys *kms.Local) integrationdomain.TokenCipher {
	if keys == nil || c.Repositories == nil || c.Repositories.Integration == nil {
		return nil
	}
	return tokenCipher{keys: keyManager{inner: keys}, store: c.Repositories.Integration, logger: c.Logger}
}

// Seal encrypts a token with the workspace's data key.
func (c tokenCipher) Seal(ctx context.Context, workspaceID uuid.UUID, plaintext string) ([]byte, error) {
	box, err := c.box(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	sealed, err := box.EncryptWith(plaintext, tokenAssociated(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("integration: seal token: %w", err)
	}
	return sealed, nil
}

// Open decrypts a token. This is the only path that opens one.
func (c tokenCipher) Open(ctx context.Context, workspaceID uuid.UUID, ciphertext []byte) (string, error) {
	box, err := c.box(ctx, workspaceID)
	if err != nil {
		return "", err
	}

	// The workspace is bound into the seal, so a ciphertext moved to another
	// workspace's row does not open there.
	plaintext, err := box.DecryptWith(ciphertext, tokenAssociated(workspaceID))
	if err != nil {
		return "", fmt.Errorf("integration: open token: %w", err)
	}
	return plaintext, nil
}

// Rotate re-seals the workspace's data key under the active key.
func (c tokenCipher) Rotate(ctx context.Context, workspaceID uuid.UUID) error {
	scope := integrationdomain.Scope{WorkspaceID: workspaceID}

	stored, err := c.store.DataKey(ctx, scope)
	if err != nil {
		return err
	}
	if stored.KeyID == c.keys.ActiveKeyID() {
		return nil
	}

	dataKey, err := c.keys.Unwrap(ctx, workspaceID, integrationdomain.WrappedKey{
		KeyID: stored.KeyID,
		Bytes: stored.Bytes,
	})
	if err != nil {
		return err
	}

	return c.reseal(ctx, workspaceID, dataKey)
}

// box returns the secret box for a workspace's data key, minting the key the
// first time a workspace seals something.
func (c tokenCipher) box(ctx context.Context, workspaceID uuid.UUID) (*crypto.Box, error) {
	dataKey, err := c.dataKey(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return crypto.New(dataKey)
}

// dataKey resolves the workspace's data key, creating it on first use.
//
// A wrap that names a retired key is re-sealed under the active one on the way
// through. That is what makes a rotation converge: until it runs, the retired key
// still opens the connection, and afterwards the retired key can be dropped.
// Re-sealing is best effort — the data key already opened, so a failure costs the
// convergence and not the call.
func (c tokenCipher) dataKey(ctx context.Context, workspaceID uuid.UUID) ([]byte, error) {
	scope := integrationdomain.Scope{WorkspaceID: workspaceID}

	stored, err := c.store.DataKey(ctx, scope)
	if errors.Is(err, integrationdomain.ErrNotFound) {
		return c.mint(ctx, workspaceID)
	}
	if err != nil {
		return nil, err
	}

	dataKey, err := c.keys.Unwrap(ctx, workspaceID, integrationdomain.WrappedKey{
		KeyID: stored.KeyID,
		Bytes: stored.Bytes,
	})
	if err != nil {
		return nil, err
	}

	if stored.KeyID != c.keys.ActiveKeyID() {
		if err := c.reseal(ctx, workspaceID, dataKey); err != nil {
			c.logger.Warn("a rotated data key could not be re-sealed",
				zap.String("workspace", workspaceID.String()),
				zap.String("stored_key", stored.KeyID),
				zap.String("active_key", c.keys.ActiveKeyID()),
				zap.Error(err),
			)
		}
	}

	return dataKey, nil
}

// mint creates a workspace's data key and stores the wrapped form.
func (c tokenCipher) mint(ctx context.Context, workspaceID uuid.UUID) ([]byte, error) {
	dataKey, err := kms.NewDataKey()
	if err != nil {
		return nil, err
	}

	wrapped, err := c.keys.Wrap(ctx, workspaceID, dataKey)
	if err != nil {
		return nil, err
	}

	if err := c.store.SaveDataKey(ctx, integrationdomain.Scope{WorkspaceID: workspaceID}, integrationdomain.WrappedKey{
		KeyID: wrapped.KeyID,
		Bytes: wrapped.Bytes,
	}); err != nil {
		return nil, err
	}
	return dataKey, nil
}

// reseal wraps a data key under the active key and stores it.
func (c tokenCipher) reseal(ctx context.Context, workspaceID uuid.UUID, dataKey []byte) error {
	wrapped, err := c.keys.Wrap(ctx, workspaceID, dataKey)
	if err != nil {
		return err
	}

	if err := c.store.SaveDataKey(ctx, integrationdomain.Scope{WorkspaceID: workspaceID}, integrationdomain.WrappedKey{
		KeyID: wrapped.KeyID,
		Bytes: wrapped.Bytes,
	}); err != nil {
		return err
	}

	c.logger.Info("a data key was re-sealed under the active key",
		zap.String("workspace", workspaceID.String()),
		zap.String("active_key", wrapped.KeyID),
	)
	return nil
}

// tokenAssociated is the data every token seal is bound to. Binding the
// workspace means a stored ciphertext cannot be moved to another workspace's row
// and opened there.
func tokenAssociated(workspaceID uuid.UUID) []byte {
	return []byte("integration-tokens:" + workspaceID.String())
}

// compile-time checks that the adapters satisfy the ports they are wired to.
var (
	_ integrationdomain.TokenCipher = tokenCipher{}
	_ integrationdomain.KeyManager  = keyManager{}
)

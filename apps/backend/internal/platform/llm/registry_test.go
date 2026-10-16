package llm_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/llm/vision"
)

// picture is a request carrying one, which is what a capability check is asked
// about.
func picture() domain.ChatRequest {
	return domain.ChatRequest{Messages: []domain.Message{{
		Role:   domain.RoleUser,
		Text:   "Stok di foto ini tinggal berapa?",
		Images: []domain.Image{{MediaType: "image/png", Data: "iVBORw0KGgo="}},
	}}}
}

// TestATextOnlyModelRefusesThePictureBeforeTheCall is issue #237's first half: the
// adapter speaks for every model of its wire format, so vision cannot be a
// constant in the registry. A model the deployment knows is text-only must have the
// picture refused here — before a request is spent, and before a provider answers
// 400 — which is what makes the caller's retry without pictures the first attempt
// rather than the fifth.
func TestATextOnlyModelRefusesThePictureBeforeTheCall(t *testing.T) {
	factory := llm.Factory{Vision: vision.New([]string{"echo-text"})}

	for _, adapter := range []string{domain.AdapterOpenAI, domain.AdapterAnthropic} {
		provider, err := factory.Build(domain.ProviderConfig{Adapter: adapter, Model: "echo-text-v2"})
		require.NoError(t, err)
		require.False(t, provider.Capabilities().Vision, "%s: the model cannot see", adapter)
		require.ErrorIs(t, provider.Capabilities().Supports(picture()), domain.ErrUnsupported,
			"%s: the picture is refused before the call", adapter)
	}
}

// TestAModelTheListDoesNotNameIsSentThePicture: the list only takes vision away,
// so a model it does not name keeps the wire format's own default. Getting that
// wrong the other way would leave a person's photo unread by a model that could
// have read it, with nothing but a notice to say so.
func TestAModelTheListDoesNotNameIsSentThePicture(t *testing.T) {
	factory := llm.Factory{Vision: vision.New([]string{"echo-text"})}

	for _, adapter := range []string{domain.AdapterOpenAI, domain.AdapterAnthropic} {
		provider, err := factory.Build(domain.ProviderConfig{Adapter: adapter, Model: "gpt-4o-mini"})
		require.NoError(t, err)
		require.True(t, provider.Capabilities().Vision, "%s: the model is assumed to see", adapter)
		require.NoError(t, provider.Capabilities().Supports(picture()), adapter)
	}
}

// TestAFactoryWithoutAPolicyKeepsTheWireFormatsDefault: a deployment that
// configured no list, and the tests that build a factory directly, must behave as
// the format does rather than losing vision everywhere.
func TestAFactoryWithoutAPolicyKeepsTheWireFormatsDefault(t *testing.T) {
	provider, err := llm.Factory{}.Build(domain.ProviderConfig{Adapter: domain.AdapterOpenAI, Model: "echo-text"})
	require.NoError(t, err)
	require.True(t, provider.Capabilities().Vision)
}

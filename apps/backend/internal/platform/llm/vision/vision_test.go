package vision

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAModelTheListNamesCannotSee: the deployment's list is what saves a wasted
// round trip, so it matches the way a provider names a model — a family entry
// covers its dated variants, and the case does not decide.
func TestAModelTheListNamesCannotSee(t *testing.T) {
	policy := New([]string{" DeepSeek-Chat ", "llama3.1", ""})

	require.False(t, policy.Sees("deepseek-chat"))
	require.False(t, policy.Sees("DeepSeek-Chat-v3"), "a family entry covers its dated variants")
	require.False(t, policy.Sees("llama3.1:8b"), "the provider's own tag is still that model")

	require.True(t, policy.Sees("gpt-4o"), "a model the list does not name is assumed to see")
	require.True(t, policy.Sees("deepseek-reasoner"), "the match is a prefix, not a substring")
}

// TestAnUnconfiguredPolicySeesEverything: a deployment that lists nothing keeps
// the wire format's own default, and a factory built without a policy behaves the
// same rather than taking vision away from every model.
func TestAnUnconfiguredPolicySeesEverything(t *testing.T) {
	require.True(t, New(nil).Sees("echo-text"))
	require.True(t, (*Policy)(nil).Sees("echo-text"))
}

// TestRefusedReadsAProviderRefusingAPicture is the other half of the question: a
// model the list does not name may still be text-only, and then the provider says
// so in its 400. Both adapters read that message the same way, which is why the
// rule lives here.
func TestRefusedReadsAProviderRefusingAPicture(t *testing.T) {
	refusals := []string{
		"This model does not support image input",
		"invalid_request_error: This model does not support image input",
		"Invalid content type. image_url is only supported by certain models.",
		"unsupported image content",
		"this model cannot read images",
		"image content is not allowed for this model",
		"this is a text-only model",
	}
	for _, message := range refusals {
		require.True(t, Refused(message), "a refusal of the picture must be read as one: %s", message)
	}

	malformed := []string{
		"model 'gpt-4o' not found",
		"maximum context length is 8192 tokens",
		"the request body is not valid JSON",
		"temperature must be between 0 and 2",
		"",
	}
	for _, message := range malformed {
		require.False(t, Refused(message),
			"a request that is wrong for every model is not a capability: %s", message)
	}
}

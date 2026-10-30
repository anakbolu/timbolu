package integration

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/llm/vision"
)

// TestATextOnlyModelIsNeverSentThePicture is the configured half of #237: the
// deployment knows the model cannot see, so the picture is refused before the call
// is made. What that buys is a single request instead of a provider refusing the
// same picture on every attempt, and a reply that answers the question in words.
func TestATextOnlyModelIsNeverSentThePicture(t *testing.T) {
	model := newTextOnlyModel(t, "Fotonya tidak bisa kubaca, tapi stoknya 7 pcs.", true)
	rig := newVisionRigWith(t, model, llm.Factory{Vision: vision.New([]string{"echo-text"})})

	answer, notices := rig.askWithAPicture(t)

	require.Contains(t, answer, "7 pcs")
	require.Contains(t, strings.Join(notices, " "), "tidak bisa membaca gambar",
		"the person is told the picture was not read")

	prompts := model.recorded()
	require.Len(t, prompts, 1, "a model known to be text-only costs one call, not a retry")
	require.Zero(t, model.withPictures(), "the picture never leaves the building")
	require.Contains(t, prompts[0], "foto.png", "the model is still told a picture is there")
	require.Contains(t, prompts[0], "tidak bisa dibaca")
}

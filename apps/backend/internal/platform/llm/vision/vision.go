// Package vision decides whether a model can read a picture.
//
// Whether a model sees is a fact about the model, not about the wire format: one
// OpenAI-compatible route serves a model that reads pictures and a text-only one
// alike, and the same is true of Anthropic's. The deployment is what knows which
// of its models is which, so the list of text-only models comes from
// configuration, and everything not on it is assumed to see. A wrong guess in that
// direction costs one refused request that the caller answers without the picture;
// a wrong guess the other way leaves a picture nobody reads, which is worse.
package vision

import "strings"

// Policy answers the vision question for one deployment.
type Policy struct {
	textOnly []string
}

// New builds a policy from the configured models. An entry is matched against the
// model name a provider is configured with, case-insensitively and by prefix, so
// "llama3.1" covers "llama3.1:8b" and a family entry covers its dated variants.
// Blanks are dropped: a list that trails a comma is a typo, not a model.
//
// A list with nothing in it is no policy at all, which is what keeps an
// unconfigured deployment on the wire format's own default.
func New(models []string) *Policy {
	textOnly := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		textOnly = append(textOnly, model)
	}
	if len(textOnly) == 0 {
		return nil
	}

	return &Policy{textOnly: textOnly}
}

// Sees reports whether the model may be sent a picture.
//
// A deployment that configured no list, and a factory built without a policy, see
// everything: the caller is told nothing rather than told wrongly.
func (p *Policy) Sees(model string) bool {
	if p == nil {
		return true
	}

	model = strings.ToLower(strings.TrimSpace(model))
	for _, textOnly := range p.textOnly {
		if strings.HasPrefix(model, textOnly) {
			return false
		}
	}

	return true
}

// Refused reports whether a provider's error message is a refusal of the picture
// rather than a complaint about the request.
//
// This is the half of the question the configuration cannot answer: a model nobody
// listed may still be text-only, and then the provider says so in the 400 it
// returns. Reading that 400 as a malformed request is what makes every retry carry
// the same picture and the reply end as a failure, so both adapters ask here.
func Refused(message string) bool {
	message = strings.ToLower(message)
	for _, marker := range refusalMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}

	return false
}

// refusalMarkers are the phrasings the providers this deployment speaks to use when
// they refuse a picture. They name the picture rather than the request, which is
// what separates "this route cannot" from "this request is wrong".
var refusalMarkers = []string{
	"does not support image",
	"image_url is only supported",
	"unsupported image content",
	"cannot read images",
	"image content is not allowed",
	"text-only model",
}

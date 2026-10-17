/**
 * Whether a model can read a picture.
 *
 * Ported from internal/platform/llm/vision/vision.go. Whether a model sees is a
 * fact about the model, not about the wire format: one [OI]-compatible route
 * serves a model that reads pictures and a text-only one alike. The deployment
 * knows which of its models is which, so the list of text-only models comes from
 * configuration, and everything not on it is assumed to see.
 */

/** Answers the vision question for one deployment. */
export class Policy {
  private readonly textOnly: string[];

  constructor(textOnly: string[]) {
    this.textOnly = textOnly;
  }

  /**
   * Whether the model may be sent a picture. A null policy sees everything: the
   * caller is told nothing rather than told wrongly.
   */
  sees(model: string): boolean {
    if (this === null || this === undefined) return true;
    const name = model.toLowerCase().trim();
    for (const textOnly of this.textOnly) {
      if (name.startsWith(textOnly)) return false;
    }
    return true;
  }
}

/**
 * Builds a policy from the configured models. An entry is matched against the
 * model name case-insensitively and by prefix, so "llama3.1" covers
 * "llama3.1:8b". Blanks are dropped: a list that trails a comma is a typo, not a
 * model. A list with nothing in it is no policy at all, which keeps an
 * unconfigured deployment on the wire format's own default.
 */
export function newPolicy(models: string[]): Policy | null {
  const textOnly: string[] = [];
  for (const raw of models) {
    const model = raw.toLowerCase().trim();
    if (model === "") continue;
    textOnly.push(model);
  }
  if (textOnly.length === 0) return null;
  return new Policy(textOnly);
}

/**
 * The phrasings the providers use when they refuse a picture. They name the
 * picture rather than the request, which is what separates "this route cannot"
 * from "this request is wrong".
 */
const REFUSAL_MARKERS = [
  "does not support image",
  "image_url is only supported",
  "unsupported image content",
  "cannot read images",
  "image content is not allowed",
  "text-only model",
];

/**
 * Whether a provider's error message is a refusal of the picture rather than a
 * complaint about the request.
 */
export function refused(message: string): boolean {
  const lower = message.toLowerCase();
  return REFUSAL_MARKERS.some((marker) => lower.includes(marker));
}

/**
 * Package llm holds the provider adapters (Anthropic-compatible and
 * OpenAI-compatible). Each adapter translates the internal message format into
 * one provider wire format and normalises the streaming events back.
 *
 * The domain contract — Provider, ChatRequest, StreamEvent, Capabilities —
 * lives in internal/modules/llm/domain and is owned by EPIC 4 (#34). Nothing in
 * this package may be imported by another module's domain or service; only the
 * container wires adapters into services.
 *
 * Ported from internal/platform/llm/doc.go. The contract types live in
 * `./registry.ts` in this port; see the port report's gaps.
 */

export {};

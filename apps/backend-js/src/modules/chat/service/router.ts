/**
 * The group router: which Bolu answers a group message.
 *
 * A small model call decides, because "who should handle this" is a judgement
 * about meaning that keywords get wrong in exactly the cases that matter. The
 * call is a routing call, so its cost lands in the usage ledger under its own
 * purpose and is visible in the report rather than hidden inside the answer.
 *
 * The decision is not allowed to fail the message: when the model is unavailable
 * or answers with something that is not a candidate, a deterministic score picks
 * instead and the event says so.
 *
 * Ported 1:1 from internal/modules/chat/service/router.go.
 */

import type { Logger } from "../../../platform/logger.ts";
import type { Gateway } from "../../llm/domain/gateway.ts";
import { PurposeRouting, RoleUser } from "../../llm/domain/provider.ts";
import { ErrNoResponder, isNil, type Conversation, type Message } from "../domain/conversation.ts";
import {
  RouterSourceFallback,
  RouterSourceModel,
  type AgentDirectory,
  type AgentRef,
  type Router,
  type RouterDecision,
  type Scope,
} from "../domain/ports.ts";
import { renderBlocksForModel } from "./attachment_context.ts";

/** The dependencies of the group router. */
export interface RouterDeps {
  gateway: Gateway | null;
  agents: AgentDirectory;
  logger: Logger | null;
}

/** Picks the Bolu that answers a group message. */
export class GroupRouter implements Router {
  constructor(private readonly deps: RouterDeps) {}

  async pick(scope: Scope, conversation: Conversation, message: Message): Promise<RouterDecision> {
    const candidates = await this.candidates(scope, conversation);
    if (candidates.length === 0) {
      throw ErrNoResponder;
    }
    if (candidates.length === 1) {
      return {
        agentId: candidates[0].id,
        reason: `hanya ${candidates[0].name} yang bisa menjawab`,
        source: RouterSourceFallback,
        candidates: 1,
      };
    }

    const question = renderBlocksForModel(message.blocks);

    const asked = await this.askModel(scope, candidates, question);
    if (asked !== null) {
      return asked;
    }

    // The fallback is deliberately simple and explainable: the Bolu whose role
    // shares the most words with the message, and failing that the first active
    // one. It is what runs when the model is unreachable, and it never leaves a
    // group message unanswered.
    const best = scoreByRole(candidates, question);
    return {
      agentId: best.id,
      reason: `dipilih dari perannya (${best.role})`,
      source: RouterSourceFallback,
      candidates: candidates.length,
    };
  }

  /** The Bolu of the conversation that accept new work. */
  private async candidates(scope: Scope, conversation: Conversation): Promise<AgentRef[]> {
    const ids = conversation.participants
      .filter((participant) => !isNil(participant.agentId))
      .map((participant) => participant.agentId);
    if (ids.length === 0) {
      return [];
    }

    const agents = await this.deps.agents.agents(scope, ids);

    // A resting Bolu is not a candidate: the switch is the user's, and answering
    // would contradict it.
    return agents.filter((agent) => this.deps.agents.active(agent));
  }

  /** Asks the model which Bolu should answer. */
  private async askModel(
    scope: Scope,
    candidates: AgentRef[],
    question: string,
  ): Promise<RouterDecision | null> {
    if (this.deps.gateway === null) {
      return null;
    }

    const roster = candidates.map(
      (agent) => `- ${agent.name} (${agent.role}): ${oneLine(agent.persona)}`,
    );

    try {
      // The routing call runs without an agent override, so it uses the
      // workspace's first provider: the cheapest one the workspace chose.
      const response = await this.deps.gateway.chat(undefined, llmScope(scope), {
        model: "",
        system:
          "Kamu router obrolan grup. Pilih satu Bolu yang paling tepat menjawab pesan terakhir. " +
          "Jawab hanya dengan namanya, tanpa penjelasan.",
        retrieved: "",
        messages: [
          {
            role: RoleUser,
            text: `Bolu yang tersedia:\n${roster.join("\n")}\n\nPesan terakhir:\n${question}`,
            images: [],
            toolCalls: [],
            toolResults: [],
          },
        ],
        tools: [],
        toolChoice: "auto",
        toolName: "",
        maxTokens: 16,
        temperature: null,
        stopSequences: [],
        metadata: { workspaceId: scope.workspaceId, agentId: "", taskId: "", purpose: PurposeRouting },
      });

      const chosen = matchCandidate(candidates, response.text);
      if (chosen === null) {
        this.deps.logger?.warn(
          { answer: oneLine(response.text) },
          "router fell back: the model named nobody in the group",
        );
        return null;
      }

      return {
        agentId: chosen.id,
        reason: "dipilih karena paling cocok dengan pesanmu",
        source: RouterSourceModel,
        candidates: candidates.length,
      };
    } catch (err) {
      this.deps.logger?.warn({ err: (err as Error).message }, "router fell back: the model did not answer");
      return null;
    }
  }
}

/** The tenant identity a model call runs as. */
function llmScope(scope: Scope): { userId: string; workspaceId: string; agentId: string } {
  return { userId: scope.userId, workspaceId: scope.workspaceId, agentId: "" };
}

/**
 * Finds the candidate a model answer named.
 *
 * The comparison is on whole words rather than substrings: a model that answers
 * "Biru" must not match a Bolu called "Biru Muda" by accident, and one that
 * answers with a sentence still matches the name inside it.
 */
function matchCandidate(candidates: AgentRef[], answer: string): AgentRef | null {
  const words = wordSet(answer);
  if (words.size === 0) {
    return null;
  }
  for (const candidate of candidates) {
    if (words.has(candidate.name.toLowerCase())) {
      return candidate;
    }
  }
  return null;
}

/**
 * The deterministic fallback: the Bolu whose role and persona share the most
 * words with the message.
 */
function scoreByRole(candidates: AgentRef[], question: string): AgentRef {
  const asked = wordSet(question);

  let best = candidates[0];
  let bestScore = -1;

  for (const agent of candidates) {
    let score = 0;
    for (const word of wordSet(`${agent.role} ${agent.persona}`)) {
      if (asked.has(word)) {
        score++;
      }
    }
    if (score > bestScore) {
      best = agent;
      bestScore = score;
    }
  }

  return best;
}

/**
 * Lowercases a text and keeps its words, which is what both the match and the
 * score compare. Short words carry no signal and match by accident.
 */
function wordSet(text: string): Set<string> {
  const words = new Set<string>();
  for (const field of text.toLowerCase().split(/[^a-z0-9_-]+/)) {
    if (field.length < 3) {
      continue;
    }
    words.add(field);
  }
  return words;
}

/** Collapses a text onto one line. */
function oneLine(text: string): string {
  return text.replace(/[\r\n]+/g, " ").split(/\s+/).filter(Boolean).join(" ");
}

/**
 * Who a message names.
 *
 * A name is only a name after an `@`: a group where a Bolu is called "Biru" must
 * not turn every sentence about biru into an order for it. The match is
 * case-insensitive and whole-word, so "@Biru" reaches Biru while "@Biru Muda"
 * reaches Biru Muda — the longest name wins, which is what keeps a roster with
 * both of them from answering with the wrong one.
 *
 * A Bolu called by name is not filtered by the resting switch: the switch says
 * "no new work", and a person typing its name is addressing it.
 *
 * Ported 1:1 from internal/modules/chat/service/mention.go.
 */

import type { AgentRef } from "../domain/ports.ts";

/**
 * The Bolu a message calls by name, in the order the names appear.
 *
 * The cursor moves by hand because a name that matched is skipped whole: the
 * `@` of a longer name must not be read twice.
 */
export function mentioned(agents: AgentRef[], text: string): AgentRef[] {
  const body = Array.from(text.toLowerCase());
  if (body.length === 0 || agents.length === 0) {
    return [];
  }

  const names = agents.map((agent) => Array.from(agent.name.trim().toLowerCase()));
  const found: AgentRef[] = [];
  const seen = new Set<string>();

  for (let at = 0; at < body.length; ) {
    if (body[at] !== "@") {
      at++;
      continue;
    }

    let best = -1;
    for (let index = 0; index < names.length; index++) {
      const name = names[index];
      if (name.length === 0 || (best >= 0 && name.length <= names[best].length)) {
        continue;
      }
      if (!namesTheBolu(body.slice(at + 1), name)) {
        continue;
      }
      best = index;
    }
    if (best < 0) {
      at++;
      continue;
    }

    if (!seen.has(agents[best].id)) {
      seen.add(agents[best].id);
      found.push(agents[best]);
    }
    at += 1 + names[best].length;
  }

  return found;
}

/**
 * Reports whether the body starts with the name and the name ends where the
 * name ends: "@Biru." calls Biru, "@Biru Muda" does not.
 */
function namesTheBolu(body: string[], name: string[]): boolean {
  if (body.length < name.length) {
    return false;
  }
  for (let index = 0; index < name.length; index++) {
    if (body[index] !== name[index]) {
      return false;
    }
  }
  if (body.length === name.length) {
    return true;
  }
  return !isNameRune(body[name.length]);
}

/**
 * Reports whether a code point continues a name, which is what makes the match
 * whole-word. Go's `unicode.IsLetter` covers the same ranges as this pattern.
 */
function isNameRune(rune: string): boolean {
  return /[\p{L}\p{Nd}]/u.test(rune);
}

"use client";

/**
 * The group icon: the faces of the Bolu in the room.
 *
 * A group is not one character, so it is drawn as its members — two Bolu, two
 * faces; five, four. The positions are the design's: a diagonal pair, a triangle
 * for three, and a 2×2 for four, all inside the square a single Bolu would fill.
 */

import { Users } from "lucide-react";

import { Bolu } from "@/components/bolu/bolu";
import type { Agent, ChatParticipant } from "@/lib/api";
import { allBot } from "@/lib/crew";
import { crewFor, expressionOf, stateOf } from "@/lib/dashboard-view";

/** Face positions inside the design's 44px icon box, by how many are shown. */
const FACE_LAYOUT: Record<number, [number, number][]> = {
  2: [
    [0, 17],
    [18, 0],
  ],
  3: [
    [0, 18],
    [18, 18],
    [9, 0],
  ],
  4: [
    [0, 0],
    [18, 0],
    [0, 18],
    [18, 18],
  ],
};

/** The box the layout is authored in, and the face that sits in it. */
const BOX = 44;
const FACE = 26;

/** The most faces an icon shows; a bigger group is still read at a glance. */
const MAX_FACES = 4;

/**
 * GroupIcon draws a group as up to four of its Bolu, scaled to `size`.
 *
 * A participant whose Bolu is not in `agents` still takes a seat — it is drawn
 * as the fallback face rather than dropped, so the icon keeps the group's size.
 */
export function GroupIcon({
  participants,
  agents,
  size = 36,
  className,
}: {
  participants: ChatParticipant[];
  agents: Agent[];
  /** Square size in pixels. Defaults to 36, the sidebar row's Bolu. */
  size?: number;
  className?: string;
}) {
  const members = participants.filter((participant) => participant.is_agent).slice(0, MAX_FACES);

  // A group can hold people and no Bolu at all. There is no face to draw then,
  // so it falls back to the group glyph rather than an empty square.
  if (members.length === 0) {
    return (
      <div
        className={`flex flex-none items-center justify-center rounded-md bg-bolu-panel text-bolu-body ${className ?? ""}`}
        style={{ width: size, height: size }}
        aria-hidden="true"
      >
        <Users size={Math.round(size / 2)} strokeWidth={2} />
      </div>
    );
  }

  // A group of one is drawn as that one Bolu, filling the box the way a direct
  // thread's face does; the design only lays out the multi-face cases.
  if (members.length === 1) {
    const agent = agents.find((item) => item.id === members[0].agent_id);
    return (
      <Bolu
        className={className}
        member={agent ? crewFor(agent) : allBot("Oren")}
        expression={expressionOf(agent ? stateOf(agent) : "idle")}
        color={agent?.color || undefined}
        size={size}
      />
    );
  }

  const layout = FACE_LAYOUT[members.length] ?? FACE_LAYOUT[2];
  const scale = size / BOX;
  const faceSize = FACE * scale;

  return (
    <div
      className={`relative flex-none ${className ?? ""}`}
      style={{ width: size, height: size }}
      aria-hidden="true"
    >
      {members.map((participant, index) => {
        const agent = agents.find((item) => item.id === participant.agent_id);
        const [x, y] = layout[index];
        return (
          <div
            key={participant.id}
            className="absolute"
            style={{ left: x * scale, top: y * scale, width: faceSize, height: faceSize }}
          >
            <Bolu
              member={agent ? crewFor(agent) : allBot("Oren")}
              expression={expressionOf(agent ? stateOf(agent) : "idle")}
              color={agent?.color || undefined}
              size={faceSize}
            />
          </div>
        );
      })}
    </div>
  );
}

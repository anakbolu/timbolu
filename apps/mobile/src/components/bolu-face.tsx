/**
 * A Bolu, drawn without SVG.
 *
 * The web app renders the character from `packages/bolu-character/characters.ts`
 * through `dom.tsx`; this scaffold has no `react-native-svg` and no cross-package
 * Metro rule, so it approximates the things a list needs — the silhouette, the
 * colour, and now the expression — with plain views. `shape` picks the corner
 * radius, `color` comes from the registry, and `state` is the Bolu's live
 * `agent.state`; without one the face keeps the friendly expression it started
 * with.
 */

import { useEffect, useRef } from "react";
import { Animated, StyleSheet, Text, View } from "react-native";

import type { AgentState } from "@/lib/agent-state";
import { hexToHsl, inkFor, safeBoluColor } from "@/lib/bolu";

/**
 * How each named shape rounds. The numbers are proportions of the avatar's
 * size, so one table serves every rendering size. The extreme values are the
 * point: `kotak` has to read square and `bulat` has to read round even at
 * 28 points.
 */
const SHAPE_RADIUS: Record<string, number> = {
  kacang: 0.42,
  hantu: 0.5,
  gumpal: 0.34,
  awan: 0.46,
  daun: 0.5,
  percik: 0.22,
  bintang: 0.28,
  genangan: 0.16,
  melambai: 0.4,
  tetes: 0.5,
  mochi: 0.38,
  bulat: 0.5,
  kotak: 0.14,
  segitiga: 0.3,
  hati: 0.44,
  kucing: 0.24,
};

type Props = {
  color?: string | undefined;
  shape?: string | undefined;
  size?: number;
  /** Shown on the body when there is no face to draw, e.g. at 28 points. */
  label?: string;
  /**
   * The Bolu's state. Omitted keeps the face this component has always drawn;
   * `idle` is the same expression, so an unknown state degrades to it rather
   * than to a blank body.
   */
  state?: AgentState | undefined;
};

export function BoluFace({ color, shape, size = 44, label, state }: Props) {
  const fill = safeBoluColor(color);
  // The same rule the palette applies to the SVG: a dark body flips the face to
  // a warm off-white. Reading it from the palette keeps the two renderers from
  // disagreeing about which Bolus look "dark".
  const ink = inkFor(hexToHsl(fill)[2]);
  const radius = size * (SHAPE_RADIUS[shape ?? ""] ?? 0.5);
  const eye = Math.max(2, Math.round(size * 0.07));
  const mouthWidth = Math.round(size * 0.26);
  const expression: AgentState = state ?? "idle";

  /**
   * The thinking dot's pulse.
   *
   * One `Animated.Value` for the one expression that moves. Idle, waiting,
   * working, and resting are static, so the loop only exists while a Bolu is
   * thinking — a face nobody is looking at must not keep a timer alive.
   */
  const pulse = useRef(new Animated.Value(1)).current;
  useEffect(() => {
    if (expression !== "thinking") {
      pulse.setValue(1);
      return;
    }
    const loop = Animated.loop(
      Animated.sequence([
        Animated.timing(pulse, { toValue: 0.25, duration: 420, useNativeDriver: true }),
        Animated.timing(pulse, { toValue: 1, duration: 420, useNativeDriver: true }),
      ]),
    );
    loop.start();
    return () => loop.stop();
  }, [expression, pulse]);

  if (label !== undefined && size < 36) {
    return (
      <View style={[styles.body, { width: size, height: size, borderRadius: radius, backgroundColor: fill }]}>
        <Text style={{ color: ink, fontSize: Math.round(size * 0.42), fontWeight: "700" }}>{label}</Text>
      </View>
    );
  }

  // Eyes carry most of the expression at 36-44 points: a focused eye is narrower
  // than a dot, a thinking eye is half-lidded, a waiting eye is wider, and a
  // resting eye is closed into a line. The table keeps those differences in one
  // place instead of repeating two views per state.
  const eyes =
    expression === "working"
      ? { width: Math.round(eye * 1.2), height: Math.max(2, Math.round(eye * 0.8)), borderRadius: Math.round(eye * 0.3) }
      : expression === "thinking"
        ? { width: Math.round(eye * 1.4), height: Math.max(1, Math.round(eye * 0.45)), borderRadius: eye }
        : expression === "waiting"
          ? { width: Math.round(eye * 1.5), height: Math.round(eye * 1.5), borderRadius: eye }
          : expression === "resting"
            ? { width: Math.round(eye * 1.4), height: Math.max(1, Math.round(size * 0.045)), borderRadius: eye }
            : { width: eye, height: eye, borderRadius: eye };

  const mouthTop = Math.round(size * 0.06);

  return (
    <View style={[styles.body, { width: size, height: size, borderRadius: radius, backgroundColor: fill }]}>
      <View style={styles.faceRow}>
        <View style={[eyes, { backgroundColor: ink }]} />
        <View style={[eyes, { backgroundColor: ink }]} />
      </View>

      {expression === "working" ? (
        // Focused: a flat line instead of a smile.
        <View
          style={{
            width: mouthWidth,
            height: Math.max(1, Math.round(size * 0.045)),
            marginTop: mouthTop,
            borderRadius: mouthWidth,
            backgroundColor: ink,
          }}
        />
      ) : expression === "thinking" ? (
        // A single dot where the mouth would be, breathing so it reads as work
        // in progress rather than as a missing mouth.
        <Animated.View
          style={{
            width: Math.round(eye * 0.8),
            height: Math.round(eye * 0.8),
            marginTop: mouthTop,
            borderRadius: eye,
            backgroundColor: ink,
            opacity: pulse,
          }}
        />
      ) : expression === "waiting" ? (
        // Small mouth: the eyes are already doing the waiting.
        <View
          style={{
            width: Math.max(3, Math.round(size * 0.1)),
            height: Math.max(3, Math.round(size * 0.1)),
            marginTop: mouthTop,
            borderRadius: eye,
            backgroundColor: ink,
          }}
        />
      ) : expression === "resting" ? (
        <View
          style={{
            width: Math.round(mouthWidth * 0.6),
            height: Math.max(1, Math.round(size * 0.045)),
            marginTop: mouthTop,
            borderRadius: mouthWidth,
            backgroundColor: ink,
          }}
        />
      ) : (
        // The mouth is a half-round outline: a `senang` smile made of borders,
        // which keeps the face legible without a raster asset. `idle` keeps it.
        <View
          style={{
            width: mouthWidth,
            height: Math.round(mouthWidth * 0.5),
            marginTop: Math.round(size * 0.04),
            borderBottomWidth: Math.max(1, Math.round(size * 0.045)),
            borderColor: ink,
            borderBottomLeftRadius: mouthWidth,
            borderBottomRightRadius: mouthWidth,
          }}
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  body: { alignItems: "center", justifyContent: "center" },
  faceRow: { flexDirection: "row", gap: 6 },
});

/**
 * A Bolu, drawn without SVG.
 *
 * The web app renders the character from `packages/bolu-character/characters.ts`
 * through `dom.tsx`; this scaffold has no `react-native-svg` and no cross-package
 * Metro rule, so it approximates the two things a list needs — the silhouette
 * and the colour — with plain views. `shape` picks the corner radius, `color`
 * comes from the registry, and the face is fixed at its friendly expression
 * until the API carries one.
 */

import { StyleSheet, Text, View } from "react-native";

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
};

export function BoluFace({ color, shape, size = 44, label }: Props) {
  const fill = safeBoluColor(color);
  // The same rule the palette applies to the SVG: a dark body flips the face to
  // a warm off-white. Reading it from the palette keeps the two renderers from
  // disagreeing about which Bolus look "dark".
  const ink = inkFor(hexToHsl(fill)[2]);
  const radius = size * (SHAPE_RADIUS[shape ?? ""] ?? 0.5);
  const eye = Math.max(2, Math.round(size * 0.07));
  const mouthWidth = Math.round(size * 0.26);

  if (label !== undefined && size < 36) {
    return (
      <View style={[styles.body, { width: size, height: size, borderRadius: radius, backgroundColor: fill }]}>
        <Text style={{ color: ink, fontSize: Math.round(size * 0.42), fontWeight: "700" }}>{label}</Text>
      </View>
    );
  }

  return (
    <View style={[styles.body, { width: size, height: size, borderRadius: radius, backgroundColor: fill }]}>
      <View style={styles.faceRow}>
        <View style={{ width: eye, height: eye, borderRadius: eye, backgroundColor: ink }} />
        <View style={{ width: eye, height: eye, borderRadius: eye, backgroundColor: ink }} />
      </View>
      {/* The mouth is a half-round outline: a `senang` smile made of borders,
          which keeps the face legible without a raster asset. */}
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
    </View>
  );
}

const styles = StyleSheet.create({
  body: { alignItems: "center", justifyContent: "center" },
  faceRow: { flexDirection: "row", gap: 6 },
});

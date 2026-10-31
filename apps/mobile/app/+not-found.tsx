import { Pressable, StyleSheet, Text, View } from "react-native";
import { router } from "expo-router";

import { UNKNOWN_LINK_NOTICE } from "@/lib/link-destination";

/**
 * Where a path without a screen file ends up. The contract's rule 4 (a way out,
 * always) is the whole reason this file exists: without it expo-router shows its
 * own developer-facing screen, which tells the person holding the phone nothing
 * about what happened.
 */
export default function NotFoundScreen() {
  return (
    <View style={styles.page}>
      <Text style={styles.title}>{UNKNOWN_LINK_NOTICE}</Text>
      <Pressable
        accessibilityRole="button"
        onPress={() => router.replace("/(tabs)/bolu")}
        style={({ pressed }) => [styles.button, pressed && styles.buttonPressed]}
      >
        <Text style={styles.buttonText}>Kembali ke daftar Bolu</Text>
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  page: {
    flex: 1,
    backgroundColor: "#FBF7F2",
    alignItems: "center",
    justifyContent: "center",
    paddingHorizontal: 32,
    gap: 16,
  },
  title: { fontSize: 17, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  button: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 12 },
  buttonPressed: { opacity: 0.7 },
  buttonText: { color: "#FFFFFF", fontSize: 16, fontWeight: "700" },
});

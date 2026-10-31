import { useCallback, useEffect, useRef, useState } from "react";
import {
  ActivityIndicator,
  Pressable,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { Redirect, router, useLocalSearchParams } from "expo-router";

import { getConversation } from "@/lib/api";
import { useAuth } from "@/lib/auth";

type LoadState =
  | { kind: "loading" }
  | { kind: "unsupported" }
  | { kind: "missing" };

/**
 * Landing screen for `bolu://chat/<conversation_id>` and universal links that
 * carry a conversation id. It resolves the conversation once, then either opens
 * the existing 1:1 Bolu thread, explains that groups are not yet supported,
 * or falls back to a "not found" message — never showing a raw server error.
 */
export default function ThreadScreen() {
  const { status } = useAuth();
  const { conversationID } = useLocalSearchParams<{ conversationID: string }>();
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const started = useRef(false);

  const load = useCallback(async () => {
    if (!conversationID) {
      setState({ kind: "missing" });
      return;
    }

    const result = await getConversation(conversationID);

    if (!result.ok) {
      setState({ kind: "missing" });
      return;
    }

    const conversation = result.data;

    if (conversation.kind === "direct") {
      const agentParticipants = conversation.participants.filter(
        (p) => p.is_agent && p.agent_id,
      );

      if (agentParticipants.length === 1) {
        const agentId = agentParticipants[0].agent_id;
        if (agentId) {
          router.replace(`/chat/${agentId}`);
          return;
        }
      }
    }

    setState({ kind: "unsupported" });
  }, [conversationID]);

  useEffect(() => {
    if (status !== "signed-in") return;
    if (started.current) return;
    started.current = true;
    void load();
  }, [status, load]);

  if (status === "signed-out") {
    return <Redirect href="/login" />;
  }

  if (state.kind === "loading") {
    return (
      <View style={styles.centered}>
        <ActivityIndicator />
      </View>
    );
  }

  const missing = state.kind === "missing";

  return (
    <View style={styles.centered}>
      <Text style={styles.empty}>
        {missing
          ? "Obrolan tidak ditemukan."
          : "Obrolan grup belum bisa dibuka di app."}
      </Text>
      <Text style={styles.emptyHint}>
        Pilih Bolu di bawah untuk melanjutkan.
      </Text>
      <Pressable
        onPress={() => router.replace("/(tabs)/bolu")}
        accessibilityRole="button"
        style={({ pressed }) => [styles.primary, pressed && styles.pressed]}
      >
        <Text style={styles.primaryText}>Kembali ke daftar Bolu</Text>
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  centered: {
    flex: 1,
    alignItems: "center",
    justifyContent: "center",
    paddingHorizontal: 32,
    gap: 10,
    backgroundColor: "#FBF7F2",
  },
  empty: {
    fontSize: 16,
    fontWeight: "600",
    color: "#1E1B2E",
    textAlign: "center",
  },
  emptyHint: {
    fontSize: 13,
    color: "#5A5670",
    textAlign: "center",
  },
  primary: {
    backgroundColor: "#F28C4E",
    borderRadius: 10,
    paddingHorizontal: 18,
    paddingVertical: 12,
  },
  primaryText: {
    color: "#FFFFFF",
    fontWeight: "700",
  },
  pressed: { opacity: 0.6 },
});

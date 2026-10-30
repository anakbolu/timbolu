import { useCallback, useEffect, useState } from "react";
import {
  ActivityIndicator,
  FlatList,
  Pressable,
  RefreshControl,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { Redirect, router } from "expo-router";

import { BoluFace } from "@/components/bolu-face";
import { listAgents } from "@/lib/api";
import type { Agent } from "@/lib/api";
import { useAuth } from "@/lib/auth";

/** The display status, in the app's language. Unknown values are shown as-is. */
const STATUS_LABEL: Record<string, string> = {
  working: "sedang bekerja",
  waiting: "menunggu persetujuan",
  idle: "siap",
  resting: "istirahat",
};

/**
 * The Bolu in this workspace, read from `GET /agents`: the flat registry list,
 * which is what a phone with one scrollable column wants. Tapping one opens the
 * direct thread with it.
 */
export default function BoluScreen() {
  const { status } = useAuth();
  const [agents, setAgents] = useState<Agent[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  const load = useCallback(async (isRefresh: boolean) => {
    if (isRefresh) setRefreshing(true);
    const result = await listAgents();
    if (result.ok) {
      setAgents(result.data);
      setError(null);
    } else {
      setError(result.message);
    }
    setRefreshing(false);
  }, []);

  // Reload when the session changes, not on every render: a sign-out and a
  // sign-in as somebody else must not leave the previous account's Bolu here.
  useEffect(() => {
    if (status === "signed-in") void load(false);
  }, [status, load]);

  if (status === "signed-out") {
    return <Redirect href="/login" />;
  }

  return (
    <View style={styles.page}>
      {agents === null && error === null ? (
        <View style={styles.centered}>
          <ActivityIndicator />
        </View>
      ) : null}

      {error !== null ? (
        <View style={styles.centered}>
          <Text style={styles.error}>{error}</Text>
          <Pressable onPress={() => void load(true)} accessibilityRole="button" style={styles.retry}>
            <Text style={styles.retryText}>Coba lagi</Text>
          </Pressable>
        </View>
      ) : null}

      {agents !== null && agents.length === 0 ? (
        <View style={styles.centered}>
          <Text style={styles.empty}>Belum ada Bolu di workspace ini.</Text>
          <Text style={styles.emptyHint}>Buat yang pertama dari web, lalu tarik ke bawah untuk memuat ulang.</Text>
        </View>
      ) : null}

      {agents !== null && agents.length > 0 ? (
        <FlatList
          data={agents}
          keyExtractor={(agent) => agent.id}
          contentContainerStyle={styles.list}
          refreshControl={<RefreshControl refreshing={refreshing} onRefresh={() => void load(true)} />}
          renderItem={({ item }) => (
            <Pressable
              accessibilityRole="button"
              onPress={() => router.push(`/chat/${item.id}`)}
              style={({ pressed }) => [styles.row, pressed && styles.rowPressed]}
            >
              <BoluFace color={item.color} shape={item.shape} size={44} />
              <View style={styles.rowText}>
                <Text style={styles.name}>{item.name}</Text>
                <Text style={styles.role}>
                  {item.role !== "" ? item.role : "Bolu"}
                  {item.display_status !== "" ? ` · ${STATUS_LABEL[item.display_status] ?? item.display_status}` : ""}
                </Text>
              </View>
              <Text style={styles.chevron}>›</Text>
            </Pressable>
          )}
        />
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  page: { flex: 1, backgroundColor: "#FBF7F2" },
  centered: { flex: 1, alignItems: "center", justifyContent: "center", paddingHorizontal: 32, gap: 10 },
  list: { padding: 16, gap: 10 },
  row: {
    flexDirection: "row",
    alignItems: "center",
    gap: 12,
    backgroundColor: "#FFFFFF",
    borderRadius: 14,
    paddingHorizontal: 14,
    paddingVertical: 12,
  },
  rowPressed: { opacity: 0.7 },
  rowText: { flex: 1, gap: 2 },
  name: { fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  role: { fontSize: 13, color: "#5A5670" },
  chevron: { fontSize: 22, color: "#B7B0A6" },
  error: { color: "#B3261E", fontSize: 14, textAlign: "center" },
  empty: { fontSize: 16, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  emptyHint: { fontSize: 13, color: "#5A5670", textAlign: "center" },
  retry: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 10 },
  retryText: { color: "#FFFFFF", fontWeight: "700" },
});

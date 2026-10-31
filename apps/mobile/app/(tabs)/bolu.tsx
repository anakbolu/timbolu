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
import * as Linking from "expo-linking";

import { BoluFace } from "@/components/bolu-face";
import { listAgents } from "@/lib/api";
import type { Agent } from "@/lib/api";
import { useAuth } from "@/lib/auth";

/**
 * Where a workspace is created. Onboarding is a web flow with no app screen yet,
 * and the address is written out rather than derived from `API_URL`: during
 * development that base is a LAN address the phone's browser could not open.
 */
const ONBOARDING_URL = "https://timbolu.com/onboarding";

/**
 * Where the first Bolu is created. The app has no create-Bolu screen yet, so the
 * page lives on the web; the address is written out for the same reason as
 * `ONBOARDING_URL` — a development `API_URL` is a LAN address a phone's browser
 * could not open.
 */
const NEW_BOLU_URL = "https://timbolu.com/bolu/new";

/** The display status, in the app's language. Unknown values are shown as-is. */
const STATUS_LABEL: Record<string, string> = {
  working: "sedang bekerja",
  waiting: "menunggu persetujuan",
  idle: "siap",
  resting: "istirahat",
};

/**
 * What the tab is showing. "No workspace" is its own case rather than an error
 * or an empty list: it is the one state whose way out is a browser, not a retry
 * of the same request, so it needs its own words and its own button.
 */
type ListState =
  | { kind: "loading" }
  | { kind: "agents"; agents: Agent[] }
  | { kind: "no-workspace" }
  | { kind: "error"; message: string };

/**
 * Initial load, pull-to-refresh, or one of the two retry buttons. "retry" is the
 * no-workspace button and must re-ask `/workspaces`; "retry-agents" is the empty
 * list's button and must only re-ask `/agents`.
 */
type LoadMode = "initial" | "pull" | "retry" | "retry-agents";

/**
 * The Bolu in this workspace, read from `GET /agents`: the flat registry list,
 * which is what a phone with one scrollable column wants. Tapping one opens the
 * direct thread with it.
 */
export default function BoluScreen() {
  const { status } = useAuth();
  const [list, setList] = useState<ListState>({ kind: "loading" });
  const [refreshing, setRefreshing] = useState(false);
  const [checking, setChecking] = useState(false);

  const load = useCallback(async (mode: LoadMode) => {
    if (mode === "pull") setRefreshing(true);
    if (mode === "retry" || mode === "retry-agents") setChecking(true);

    // Only "retry" re-asks `GET /workspaces`. The person pressing it has just
    // been told the workspace is made in a browser, so this run's cached answer
    // is exactly what must not be trusted. Every other mode keeps the workspace
    // it already has: the empty list's retry is about a Bolu made in a browser,
    // not about a workspace that may have appeared.
    const result = await listAgents({ refreshWorkspace: mode === "retry" });

    if (result.kind === "agents") {
      setList({ kind: "agents", agents: result.agents });
    } else if (result.kind === "no-workspace") {
      setList({ kind: "no-workspace" });
    } else {
      setList({ kind: "error", message: result.error.message });
    }
    setRefreshing(false);
    setChecking(false);
  }, []);

  // Reload when the session changes, not on every render: a sign-out and a
  // sign-in as somebody else must not leave the previous account's Bolu here.
  useEffect(() => {
    if (status === "signed-in") void load("initial");
  }, [status, load]);

  if (status === "signed-out") {
    return <Redirect href="/login" />;
  }

  return (
    <View style={styles.page}>
      {list.kind === "loading" ? (
        <View style={styles.centered}>
          <ActivityIndicator />
        </View>
      ) : null}

      {list.kind === "error" ? (
        <View style={styles.centered}>
          <Text style={styles.error}>{list.message}</Text>
          <Pressable
            onPress={() => void load("retry")}
            accessibilityRole="button"
            accessibilityState={{ disabled: checking }}
            disabled={checking}
            style={({ pressed }) => [styles.retry, (pressed || checking) && styles.buttonPressed]}
          >
            <Text style={styles.retryText}>Coba lagi</Text>
          </Pressable>
        </View>
      ) : null}

      {list.kind === "no-workspace" ? (
        <View style={styles.centered}>
          <Text style={styles.empty}>Belum ada ruang usaha.</Text>
          <Text style={styles.emptyHint}>
            Ruang usaha dibuat di web, bukan di app ini. Buka timbolu.com, isi data usahamu dan Bolu
            pertamamu, lalu kembali ke sini dan tekan “Coba lagi”.
          </Text>
          <Pressable
            onPress={() => {
              // A phone with no browser handler rejects; the sentence names the
              // address so it can still be typed on another device.
              void Linking.openURL(ONBOARDING_URL).catch(() => {
                setList({
                  kind: "error",
                  message: `Tidak bisa membuka browser. Buka ${ONBOARDING_URL} di perangkat lain.`,
                });
              });
            }}
            accessibilityRole="button"
            style={styles.primary}
          >
            <Text style={styles.primaryText}>Buat ruang usaha di web</Text>
          </Pressable>
          <Pressable
            onPress={() => void load("retry")}
            accessibilityRole="button"
            accessibilityState={{ disabled: checking }}
            disabled={checking}
            style={({ pressed }) => [styles.secondary, (pressed || checking) && styles.buttonPressed]}
          >
            <Text style={styles.secondaryText}>{checking ? "Memeriksa…" : "Coba lagi"}</Text>
          </Pressable>
        </View>
      ) : null}

      {list.kind === "agents" && list.agents.length === 0 ? (
        <View style={styles.centered}>
          <Text style={styles.empty}>Belum ada Bolu di workspace ini.</Text>
          <Text style={styles.emptyHint}>
            Bolu dibuat di web, bukan di app ini. Buka timbolu.com, buat Bolu pertamamu, lalu kembali
            ke sini dan tekan “Coba lagi”.
          </Text>
          <Pressable
            onPress={() => {
              // A phone with no browser handler rejects; the sentence names the
              // address so it can still be typed on another device.
              void Linking.openURL(NEW_BOLU_URL).catch(() => {
                setList({
                  kind: "error",
                  message: `Tidak bisa membuka browser. Buka ${NEW_BOLU_URL} di perangkat lain.`,
                });
              });
            }}
            accessibilityRole="button"
            style={styles.primary}
          >
            <Text style={styles.primaryText}>Buat Bolu pertama di web</Text>
          </Pressable>
          <Pressable
            onPress={() => void load("retry-agents")}
            accessibilityRole="button"
            accessibilityState={{ disabled: checking }}
            disabled={checking}
            style={({ pressed }) => [styles.secondary, (pressed || checking) && styles.buttonPressed]}
          >
            <Text style={styles.secondaryText}>{checking ? "Memeriksa…" : "Coba lagi"}</Text>
          </Pressable>
        </View>
      ) : null}

      {list.kind === "agents" && list.agents.length > 0 ? (
        <FlatList
          data={list.agents}
          keyExtractor={(agent) => agent.id}
          contentContainerStyle={styles.list}
          refreshControl={<RefreshControl refreshing={refreshing} onRefresh={() => void load("pull")} />}
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
  primary: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 10 },
  primaryText: { color: "#FFFFFF", fontWeight: "700" },
  retry: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 10 },
  retryText: { color: "#FFFFFF", fontWeight: "700" },
  secondary: {
    borderRadius: 10,
    borderWidth: 1,
    borderColor: "#F28C4E",
    paddingHorizontal: 18,
    paddingVertical: 10,
  },
  buttonPressed: { opacity: 0.7 },
  secondaryText: { color: "#F28C4E", fontWeight: "700" },
});

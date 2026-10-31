import { useCallback, useState } from "react";
import {
  ActivityIndicator,
  FlatList,
  Pressable,
  RefreshControl,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { Redirect, router, useFocusEffect } from "expo-router";

import { listAgents, listRoutines, pauseRoutine, resumeRoutine } from "@/lib/api";
import type { Agent, Routine } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { nextRunText, routineStatus } from "@/lib/routine-view";
import { TASK_TONE } from "@/lib/task-view";

/**
 * What the tab is showing. The Bolu list is read alongside the routines only to
 * label rows with who runs them, so its failure does not hide the routines.
 */
type ListState =
  | { kind: "loading" }
  | { kind: "routines"; routines: Routine[]; agents: Map<string, Agent> }
  | { kind: "error"; message: string };

/**
 * The routines a Bolu runs on its own: their next run, their switch, and their
 * history.
 *
 * There is no "jalankan sekarang" button, and none should be added: the API has
 * no route that starts a routine (`docs/api.md` § Routines and scheduling — only
 * `POST /routines/preview`, which computes times for a form). A run begins from
 * the schedule or from the event a trigger routine watches.
 *
 * Pausing replaces one row with the routine the API answered with, rather than
 * reloading the list: the answer carries the new `enabled` and `next_runs`, and a
 * full reload would throw away the rows' scroll position for a fact the server
 * already sent.
 */
export default function RoutinesScreen() {
  const { status } = useAuth();
  const [list, setList] = useState<ListState>({ kind: "loading" });
  const [refreshing, setRefreshing] = useState(false);
  const [retrying, setRetrying] = useState(false);
  /** The routine whose pause/resume is in flight; only one action at a time. */
  const [busyID, setBusyID] = useState<string | null>(null);
  const [rowError, setRowError] = useState<{ id: string; message: string } | null>(null);

  const load = useCallback(async (mode: "focus" | "pull" | "retry") => {
    if (mode === "pull") setRefreshing(true);
    if (mode === "retry") setRetrying(true);

    const [routines, agents] = await Promise.all([listRoutines(), listAgents()]);

    if (routines.ok) {
      const byId = new Map<string, Agent>();
      if (agents.kind === "agents") {
        for (const agent of agents.agents) byId.set(agent.id, agent);
      }
      setList({ kind: "routines", routines: routines.data, agents: byId });
    } else {
      setList({ kind: "error", message: routines.message });
    }

    setRefreshing(false);
    setRetrying(false);
  }, []);

  const toggle = useCallback(async (routine: Routine) => {
    setBusyID(routine.id);
    setRowError(null);

    const result = routine.enabled ? await pauseRoutine(routine.id) : await resumeRoutine(routine.id);

    if (result.ok) {
      const updated = result.data;
      setList((previous) =>
        previous.kind === "routines"
          ? {
              ...previous,
              routines: previous.routines.map((item) => (item.id === updated.id ? updated : item)),
            }
          : previous,
      );
    } else {
      setRowError({ id: routine.id, message: result.message });
    }

    setBusyID(null);
  }, []);

  useFocusEffect(
    useCallback(() => {
      if (status !== "signed-in") return;
      void load("focus");
    }, [status, load]),
  );

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
            accessibilityState={{ disabled: retrying }}
            disabled={retrying}
            style={({ pressed }) => [styles.primary, (pressed || retrying) && styles.pressed]}
          >
            <Text style={styles.primaryText}>{retrying ? "Memuat…" : "Coba lagi"}</Text>
          </Pressable>
          <Pressable
            onPress={() => router.replace("/(tabs)/bolu")}
            accessibilityRole="button"
            style={({ pressed }) => [styles.secondary, pressed && styles.pressed]}
          >
            <Text style={styles.secondaryText}>Kembali ke daftar Bolu</Text>
          </Pressable>
        </View>
      ) : null}

      {list.kind === "routines" && list.routines.length === 0 ? (
        <View style={styles.centered}>
          <Text style={styles.empty}>Belum ada rutinitas.</Text>
          <Text style={styles.emptyHint}>
            Rutinitas dibuat dari timbolu.com. Begitu ada Bolu yang punya jadwal, jadwalnya muncul di
            sini.
          </Text>
        </View>
      ) : null}

      {list.kind === "routines" && list.routines.length > 0 ? (
        <FlatList
          data={list.routines}
          keyExtractor={(routine) => routine.id}
          contentContainerStyle={styles.list}
          refreshControl={<RefreshControl refreshing={refreshing} onRefresh={() => void load("pull")} />}
          renderItem={({ item }) => {
            const view = routineStatus(item);
            const agent = list.agents.get(item.agent_id);
            const busy = busyID === item.id;
            return (
              <View style={styles.row}>
                <Pressable
                  accessibilityRole="button"
                  onPress={() => router.push(`/routine/${item.id}`)}
                  style={({ pressed }) => [styles.rowBody, pressed && styles.pressed]}
                >
                  <View style={styles.rowHead}>
                    <Text style={styles.title} numberOfLines={2}>
                      {item.name !== "" ? item.name : "Rutinitas"}
                    </Text>
                    <Text style={[styles.status, TASK_TONE[view.tone]]}>{view.label}</Text>
                  </View>
                  <Text style={styles.schedule}>{nextRunText(item)}</Text>
                  {agent ? <Text style={styles.agent}>{agent.name}</Text> : null}
                </Pressable>

                <View style={styles.rowFoot}>
                  <Pressable
                    accessibilityRole="button"
                    accessibilityState={{ disabled: busy }}
                    disabled={busy}
                    onPress={() => void toggle(item)}
                    style={({ pressed }) => [styles.action, (pressed || busy) && styles.pressed]}
                  >
                    <Text style={styles.actionText}>
                      {busy ? "Menyimpan…" : item.enabled ? "Jeda" : "Lanjutkan"}
                    </Text>
                  </Pressable>
                  {rowError?.id === item.id ? (
                    <Text style={styles.error}>{rowError.message}</Text>
                  ) : null}
                </View>

                {item.paused_reason ? (
                  <Text style={styles.note}>Dijeda oleh mesin: {item.paused_reason}</Text>
                ) : null}
              </View>
            );
          }}
        />
      ) : null}
    </View>
  );
}

const styles = StyleSheet.create({
  page: { flex: 1, backgroundColor: "#FBF7F2" },
  centered: { flex: 1, alignItems: "center", justifyContent: "center", paddingHorizontal: 32, gap: 10 },
  list: { padding: 16, gap: 10 },
  row: { backgroundColor: "#FFFFFF", borderRadius: 14, paddingHorizontal: 14, paddingVertical: 12, gap: 8 },
  rowBody: { gap: 4 },
  rowHead: { flexDirection: "row", alignItems: "flex-start", justifyContent: "space-between", gap: 8 },
  rowFoot: { flexDirection: "row", alignItems: "center", gap: 10 },
  title: { flex: 1, fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  status: { fontSize: 12, fontWeight: "700" },
  schedule: { fontSize: 13, color: "#5A5670" },
  agent: { fontSize: 12, color: "#5A5670" },
  note: { fontSize: 12, color: "#B4531E" },
  action: {
    borderRadius: 8,
    borderWidth: 1,
    borderColor: "#F28C4E",
    paddingHorizontal: 14,
    paddingVertical: 6,
  },
  actionText: { color: "#F28C4E", fontWeight: "700", fontSize: 13 },
  pressed: { opacity: 0.7 },
  error: { flex: 1, color: "#B3261E", fontSize: 13 },
  empty: { fontSize: 16, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  emptyHint: { fontSize: 13, color: "#5A5670", textAlign: "center" },
  primary: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 10 },
  primaryText: { color: "#FFFFFF", fontWeight: "700" },
  secondary: { borderRadius: 10, borderWidth: 1, borderColor: "#F28C4E", paddingHorizontal: 18, paddingVertical: 10 },
  secondaryText: { color: "#F28C4E", fontWeight: "700" },
});

import { useCallback, useEffect, useState } from "react";
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { Redirect, router, useLocalSearchParams } from "expo-router";

import { getRoutine, listAgents, listRoutineRuns } from "@/lib/api";
import type { Agent, Routine, RoutineRun } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { nextRunText, runDetail, runStatus, routineStatus } from "@/lib/routine-view";
import { TASK_TONE } from "@/lib/task-view";

type LoadState =
  | { kind: "loading" }
  | { kind: "routine"; routine: Routine; runs: RoutineRun[] | null; agent: Agent | null }
  | { kind: "missing" }
  | { kind: "error"; message: string };

/** How many runs the history asks for; the API's own cap is larger. */
const RUN_LIMIT = 20;

/**
 * One routine: its cadence, whether it is on, and its 20 latest runs.
 *
 * The routine and its runs are read separately. A history that failed to load
 * still leaves a screen that answers "what is this and when does it run", which
 * is what someone opening a routine usually came for; `null` keeps that apart
 * from "it has never run", which is an empty list.
 *
 * Nothing is retried on a 404: the API answers `routine_not_found` for an absent
 * id and for another workspace's id alike, so a "Coba lagi" there would promise
 * a change that cannot come.
 */
export default function RoutineScreen() {
  const { status } = useAuth();
  const { routineID } = useLocalSearchParams<{ routineID: string }>();
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  const load = useCallback(async () => {
    if (!routineID) {
      setState({ kind: "missing" });
      return;
    }

    setState({ kind: "loading" });
    const [routine, runs, agents] = await Promise.all([
      getRoutine(routineID),
      listRoutineRuns(routineID, RUN_LIMIT),
      listAgents(),
    ]);

    if (routine.ok) {
      let agent: Agent | null = null;
      if (agents.kind === "agents") {
        agent = agents.agents.find((candidate) => candidate.id === routine.data.agent_id) ?? null;
      }
      setState({ kind: "routine", routine: routine.data, runs: runs.ok ? runs.data : null, agent });
      return;
    }

    // 400 (`invalid_input`, an id that is not a uuid) and 404 are the same
    // screen: the id is a schema detail, not a state the person can act on.
    if (routine.code === "routine_not_found" || routine.status === 404 || routine.status === 400) {
      setState({ kind: "missing" });
      return;
    }
    setState({ kind: "error", message: routine.message });
  }, [routineID]);

  useEffect(() => {
    if (status !== "signed-in") return;
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

  if (state.kind === "missing") {
    return (
      <View style={styles.centered}>
        <Text style={styles.empty}>Rutinitas tidak ditemukan.</Text>
        <Text style={styles.emptyHint}>
          Mungkin sudah dihapus, atau rutinitas ini milik ruang kerja lain.
        </Text>
        <Pressable
          onPress={() => router.replace("/(tabs)/routine")}
          accessibilityRole="button"
          style={({ pressed }) => [styles.primary, pressed && styles.pressed]}
        >
          <Text style={styles.primaryText}>Kembali ke daftar rutinitas</Text>
        </Pressable>
      </View>
    );
  }

  if (state.kind === "error") {
    return (
      <View style={styles.centered}>
        <Text style={styles.error}>{state.message}</Text>
        <Pressable
          onPress={() => void load()}
          accessibilityRole="button"
          style={({ pressed }) => [styles.primary, pressed && styles.pressed]}
        >
          <Text style={styles.primaryText}>Coba lagi</Text>
        </Pressable>
        <Pressable
          onPress={() => router.replace("/(tabs)/routine")}
          accessibilityRole="button"
          style={({ pressed }) => [styles.secondary, pressed && styles.pressed]}
        >
          <Text style={styles.secondaryText}>Kembali ke daftar rutinitas</Text>
        </Pressable>
      </View>
    );
  }

  const { routine, runs, agent } = state;
  const view = routineStatus(routine);

  return (
    <ScrollView style={styles.page} contentContainerStyle={styles.content}>
      <Text style={styles.title}>{routine.name !== "" ? routine.name : "Rutinitas"}</Text>
      <Text style={[styles.status, TASK_TONE[view.tone]]}>{view.label}</Text>
      <Text style={styles.meta}>
        {agent !== null ? `${agent.name} · ` : ""}
        {routine.schedule}
      </Text>
      <Text style={styles.meta}>{nextRunText(routine)}</Text>

      {routine.paused_reason ? (
        <View style={styles.notice}>
          <Text style={styles.noticeText}>Dijeda oleh mesin: {routine.paused_reason}</Text>
        </View>
      ) : null}

      {routine.instruction ? <Text style={styles.summary}>{routine.instruction}</Text> : null}

      <View style={styles.card}>
        <Text style={styles.cardHeading}>20 run terakhir</Text>

        {runs === null ? (
          <>
            <Text style={styles.error}>Riwayatnya tidak bisa dimuat.</Text>
            <Pressable
              onPress={() => void load()}
              accessibilityRole="button"
              style={({ pressed }) => [styles.secondary, pressed && styles.pressed]}
            >
              <Text style={styles.secondaryText}>Muat ulang riwayat</Text>
            </Pressable>
          </>
        ) : runs.length === 0 ? (
          <Text style={styles.emptyHint}>
            {routine.enabled
              ? "Belum pernah berjalan. Run pertama muncul setelah jadwalnya tiba."
              : "Belum pernah berjalan, dan jadwalnya sedang dijeda."}
          </Text>
        ) : (
          runs.map((run) => {
            const runView = runStatus(run);
            const detail = runDetail(run);
            return (
              <View key={run.run_id} style={styles.run}>
                <View style={styles.runHead}>
                  <Text style={[styles.runState, TASK_TONE[runView.tone]]}>{runView.label}</Text>
                  <Text style={styles.runTime}>{runTimeText(run)}</Text>
                </View>
                {detail !== "" ? (
                  <Text style={styles.runDetail} numberOfLines={4}>
                    {detail}
                  </Text>
                ) : null}
                {run.task_id ? (
                  <Pressable
                    accessibilityRole="link"
                    onPress={() => router.push(`/task/${run.task_id}`)}
                    style={({ pressed }) => [styles.link, pressed && styles.pressed]}
                  >
                    <Text style={styles.linkText}>Buka tugasnya</Text>
                  </Pressable>
                ) : null}
              </View>
            );
          })
        )}
      </View>
    </ScrollView>
  );
}

/**
 * When one run happened, in local time. A finished run states its own moment; a
 * run still in flight is named by when it started, because "selesai" would be a
 * fact the server has not reported yet.
 */
function runTimeText(run: Pick<RoutineRun, "started_at" | "finished_at" | "status">): string {
  const iso = run.finished_at ?? run.started_at;
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  const day = at.toLocaleDateString("id-ID", { day: "numeric", month: "long" });
  const time = `${String(at.getHours()).padStart(2, "0")}.${String(at.getMinutes()).padStart(2, "0")}`;
  return run.finished_at ? `${day}, ${time}` : `mulai ${day}, ${time}`;
}

const styles = StyleSheet.create({
  page: { flex: 1, backgroundColor: "#FBF7F2" },
  content: { padding: 16, gap: 10 },
  centered: { flex: 1, alignItems: "center", justifyContent: "center", paddingHorizontal: 32, gap: 10 },
  title: { fontSize: 20, fontWeight: "700", color: "#1E1B2E" },
  status: { fontSize: 14, fontWeight: "700" },
  meta: { fontSize: 13, color: "#5A5670" },
  summary: { fontSize: 15, lineHeight: 21, color: "#1E1B2E" },
  notice: { backgroundColor: "#FFF1E4", borderRadius: 12, padding: 12 },
  noticeText: { fontSize: 13, color: "#6B3A16" },
  card: { backgroundColor: "#FFFFFF", borderRadius: 14, padding: 14, gap: 10 },
  cardHeading: { fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  run: { borderTopWidth: 1, borderTopColor: "#EFE9E1", paddingTop: 10, gap: 4 },
  runHead: { flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 8 },
  runState: { fontSize: 12, fontWeight: "700" },
  runTime: { fontSize: 12, color: "#5A5670" },
  runDetail: { fontSize: 13, color: "#1E1B2E" },
  link: { alignSelf: "flex-start", paddingVertical: 4 },
  linkText: { fontSize: 13, fontWeight: "700", color: "#F28C4E" },
  primary: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 12, alignItems: "center" },
  primaryText: { color: "#FFFFFF", fontWeight: "700" },
  secondary: { borderRadius: 10, borderWidth: 1, borderColor: "#F28C4E", paddingVertical: 12, alignItems: "center" },
  secondaryText: { color: "#F28C4E", fontWeight: "700" },
  pressed: { opacity: 0.6 },
  error: { color: "#B3261E", fontSize: 14, textAlign: "center" },
  empty: { fontSize: 16, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  emptyHint: { fontSize: 13, color: "#5A5670", textAlign: "center" },
});

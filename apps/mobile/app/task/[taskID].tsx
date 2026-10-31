import { useCallback, useEffect, useState } from "react";
import {
  ActivityIndicator,
  Linking,
  Platform,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { Redirect, router, useLocalSearchParams } from "expo-router";

import { API_URL, getTask, listAgents, listTaskSteps } from "@/lib/api";
import type { Agent, Task, TaskStep } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { contentRefs, momentText, stepLabel, stepPayloadText, TASK_TONE, taskState } from "@/lib/task-view";

type LoadState =
  | { kind: "loading" }
  | { kind: "task"; task: Task; steps: TaskStep[] | null; agent: Agent | null }
  | { kind: "missing" }
  | { kind: "error"; message: string };

/**
 * One task: what it did, where it is, and the rounds it took.
 *
 * The two reads are separate on purpose. A task whose steps failed to load is
 * still a task with a state and a reason, which is what a person opening a failed
 * task came for; hiding the whole screen behind the steps' error would answer
 * neither question.
 *
 * Nothing is retried on a 404: the API answers `task_not_found` for an absent id
 * and for another workspace's id alike (`docs/deep-links.md` § Skema `bolu://`,
 * rule 3), so a "Coba lagi" there would promise a change that cannot come.
 */
export default function TaskScreen() {
  const { status } = useAuth();
  const { taskID } = useLocalSearchParams<{ taskID: string }>();
  const [state, setState] = useState<LoadState>({ kind: "loading" });

  const load = useCallback(async () => {
    if (!taskID) {
      setState({ kind: "missing" });
      return;
    }

    setState({ kind: "loading" });
    const [task, steps, agents] = await Promise.all([
      getTask(taskID),
      listTaskSteps(taskID),
      listAgents(),
    ]);

    if (task.ok) {
      let agent: Agent | null = null;
      if (agents.kind === "agents") {
        agent = agents.agents.find((candidate) => candidate.id === task.data.agent_id) ?? null;
      }
      // `null` keeps "the rounds could not be read" apart from "there are none":
      // an empty list would claim the task recorded nothing, which is a different
      // fact from a request that failed.
      setState({ kind: "task", task: task.data, steps: steps.ok ? steps.data : null, agent });
      return;
    }

    // 400 (`invalid_input`, an id that is not a uuid) and 404 are the same screen:
    // the id is a schema detail, not a state the person can act on.
    if (task.code === "task_not_found" || task.status === 404 || task.status === 400) {
      setState({ kind: "missing" });
      return;
    }
    setState({ kind: "error", message: task.message });
  }, [taskID]);

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
        <Text style={styles.empty}>Tugas tidak ditemukan.</Text>
        <Text style={styles.emptyHint}>
          Mungkin sudah dihapus, atau tugas ini milik ruang kerja lain.
        </Text>
        <Pressable
          onPress={() => router.replace("/(tabs)/tasks")}
          accessibilityRole="button"
          style={({ pressed }) => [styles.primary, pressed && styles.pressed]}
        >
          <Text style={styles.primaryText}>Kembali ke daftar tugas</Text>
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
          onPress={() => router.replace("/(tabs)/tasks")}
          accessibilityRole="button"
          style={({ pressed }) => [styles.secondary, pressed && styles.pressed]}
        >
          <Text style={styles.secondaryText}>Kembali ke daftar tugas</Text>
        </Pressable>
      </View>
    );
  }

  const { task, steps, agent } = state;
  const view = taskState(task);
  // A stopped task keeps its waiting reason as history, so the "menunggu" line is
  // gated on the task still being live: a cancelled task that still announced what
  // it waited for would say two contradicting things.
  const references = contentRefs(...(steps ?? []).flatMap((step) => [step.input, step.output]));

  return (
    <ScrollView style={styles.page} contentContainerStyle={styles.content}>
      <Text style={styles.title}>{task.title !== "" ? task.title : "Tugas"}</Text>
      <Text style={[styles.state, TASK_TONE[view.tone]]}>{view.label}</Text>
      <Text style={styles.meta}>
        {agent !== null ? `${agent.name} · ` : ""}dibuka {momentText(task.created_at)}
      </Text>

      {task.stopped_reason ? (
        <View style={styles.notice}>
          <Text style={styles.noticeText}>Tugas berhenti: {task.stopped_reason}</Text>
        </View>
      ) : null}

      {view.live && task.waiting_reason ? (
        <View style={styles.notice}>
          <Text style={styles.noticeText}>Menunggu: {task.waiting_reason}</Text>
        </View>
      ) : null}

      {task.summary ? <Text style={styles.summary}>{task.summary}</Text> : null}

      <View style={styles.card}>
        <Text style={styles.cardHeading}>Ringkasan</Text>
        <View style={styles.field}>
          <Text style={styles.fieldLabel}>Langkah tercatat</Text>
          <Text style={styles.fieldValue}>{task.step_count}</Text>
        </View>
        <View style={styles.field}>
          <Text style={styles.fieldLabel}>Selesai</Text>
          <Text style={styles.fieldValue}>
            {task.finished_at ? momentText(task.finished_at) : "belum selesai"}
          </Text>
        </View>
      </View>

      {references.length > 0 ? (
        <View style={styles.card}>
          <Text style={styles.cardHeading}>Hasil</Text>
          {references.map((reference) => (
            <Pressable
              key={reference}
              accessibilityRole="link"
              onPress={() => {
                // The content origin serves the document without a session, so
                // the browser can show it even though the API is not open there.
                void Linking.openURL(`${API_URL}/content/${reference}`).catch(() => {
                  router.push("/(tabs)/tasks");
                });
              }}
              style={({ pressed }) => [styles.secondary, pressed && styles.pressed]}
            >
              <Text style={styles.secondaryText}>Buka hasil</Text>
            </Pressable>
          ))}
        </View>
      ) : null}

      <View style={styles.card}>
        <Text style={styles.cardHeading}>Langkah</Text>

        {steps === null ? (
          <>
            <Text style={styles.error}>Langkahnya tidak bisa dimuat.</Text>
            <Pressable
              onPress={() => void load()}
              accessibilityRole="button"
              style={({ pressed }) => [styles.secondary, pressed && styles.pressed]}
            >
              <Text style={styles.secondaryText}>Muat ulang langkah</Text>
            </Pressable>
          </>
        ) : steps.length === 0 ? (
          <Text style={styles.emptyHint}>
            {view.live ? "Belum ada langkah yang tercatat." : "Tidak ada langkah yang tercatat untuk tugas ini."}
          </Text>
        ) : (
          steps.map((step) => {
            // `output` is what a round produced; a tool call produced nothing yet,
            // so its arguments (`input`) are what there is to read.
            const payload = step.pruned ? "" : stepPayloadText(step.output ?? step.input);
            return (
              <View key={step.step_id} style={styles.step}>
                <View style={styles.stepHead}>
                  <Text style={styles.seq}>{step.seq}</Text>
                  <Text style={styles.kind}>{stepLabel(step.kind)}</Text>
                  {step.tool_name ? <Text style={styles.tool}>{step.tool_name}</Text> : null}
                </View>
                {step.tool_label ? <Text style={styles.stepNote}>{step.tool_label}</Text> : null}
                {/* A pruned round is stated rather than drawn empty: "the retention job
                    removed this" and "nothing happened here" are different facts. */}
                {step.pruned ? (
                  <Text style={styles.stepNote}>
                    Isinya sudah dipangkas oleh kebijakan retensi; bentuk langkahnya tetap disimpan.
                  </Text>
                ) : payload !== "" ? (
                  <Text style={styles.payload} numberOfLines={8}>
                    {payload}
                  </Text>
                ) : null}
                <Text style={styles.stepTime}>{momentText(step.created_at)}</Text>
              </View>
            );
          })
        )}
      </View>
    </ScrollView>
  );
}

const styles = StyleSheet.create({
  page: { flex: 1, backgroundColor: "#FBF7F2" },
  content: { padding: 16, gap: 10 },
  centered: { flex: 1, alignItems: "center", justifyContent: "center", paddingHorizontal: 32, gap: 10 },
  title: { fontSize: 20, fontWeight: "700", color: "#1E1B2E" },
  state: { fontSize: 14, fontWeight: "700" },
  meta: { fontSize: 13, color: "#5A5670" },
  summary: { fontSize: 15, lineHeight: 21, color: "#1E1B2E" },
  notice: { backgroundColor: "#FFF1E4", borderRadius: 12, padding: 12 },
  noticeText: { fontSize: 13, color: "#6B3A16" },
  card: { backgroundColor: "#FFFFFF", borderRadius: 14, padding: 14, gap: 10 },
  cardHeading: { fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  field: { gap: 2 },
  fieldLabel: { fontSize: 12, fontWeight: "600", color: "#5A5670" },
  fieldValue: { fontSize: 15, color: "#1E1B2E" },
  step: { borderTopWidth: 1, borderTopColor: "#EFE9E1", paddingTop: 10, gap: 3 },
  stepHead: { flexDirection: "row", alignItems: "center", gap: 8 },
  seq: { minWidth: 22, textAlign: "right", fontSize: 12, color: "#5A5670" },
  kind: { fontSize: 12, fontWeight: "700", color: "#B4531E" },
  tool: { flex: 1, fontSize: 12, color: "#1E1B2E" },
  stepNote: { fontSize: 12, color: "#5A5670" },
  // Android has no "Courier" family; its own monospace is the equivalent.
  payload: {
    fontSize: 12,
    lineHeight: 17,
    color: "#1E1B2E",
    fontFamily: Platform.OS === "ios" ? "Courier" : "monospace",
  },
  stepTime: { fontSize: 12, color: "#B7B0A6" },
  primary: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 12, alignItems: "center" },
  primaryText: { color: "#FFFFFF", fontWeight: "700" },
  secondary: { borderRadius: 10, borderWidth: 1, borderColor: "#F28C4E", paddingVertical: 12, alignItems: "center" },
  secondaryText: { color: "#F28C4E", fontWeight: "700" },
  pressed: { opacity: 0.6 },
  error: { color: "#B3261E", fontSize: 14, textAlign: "center" },
  empty: { fontSize: 16, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  emptyHint: { fontSize: 13, color: "#5A5670", textAlign: "center" },
});

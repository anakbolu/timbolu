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

import { listAgents, listTasks } from "@/lib/api";
import type { Agent, Task } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { relativeAge, TASK_TONE, taskState } from "@/lib/task-view";

/**
 * What the tab is showing. The list is loaded together with the Bolu it names,
 * so a row can say who ran the task; the two reads are separate endpoints and
 * either can fail on its own.
 */
type ListState =
  | { kind: "loading" }
  | { kind: "tasks"; tasks: Task[]; agents: Map<string, Agent> }
  | { kind: "error"; message: string };

/**
 * The workspace's task history, newest first.
 *
 * Tasks are read, never opened from here: a task is started by chat, a routine, a
 * webhook, or another task, and the API has no route that starts one
 * (`docs/api.md` § Task runtime). So this tab answers "what did the Bolu do" and
 * hands every row to the detail screen.
 *
 * The list is reloaded on focus rather than on mount: a task that was running when
 * the person left this tab has usually moved on by the time they return, and the
 * status the list keeps is the whole reason to look at it.
 */
export default function TasksScreen() {
  const { status } = useAuth();
  const [list, setList] = useState<ListState>({ kind: "loading" });
  const [refreshing, setRefreshing] = useState(false);
  const [retrying, setRetrying] = useState(false);

  const load = useCallback(async (mode: "focus" | "pull" | "retry") => {
    if (mode === "pull") setRefreshing(true);
    if (mode === "retry") setRetrying(true);

    // The Bolu list is only the labels for rows, so its failure does not hide
    // the tasks: a task whose name is unknown reads by its id rather than not at
    // all. Only the task read decides whether this screen has content.
    const [tasks, agents] = await Promise.all([listTasks({ limit: 50 }), listAgents()]);

    if (tasks.ok) {
      const byId = new Map<string, Agent>();
      if (agents.kind === "agents") {
        for (const agent of agents.agents) byId.set(agent.id, agent);
      }
      setList({ kind: "tasks", tasks: tasks.data, agents: byId });
    } else {
      setList({ kind: "error", message: tasks.message });
    }

    setRefreshing(false);
    setRetrying(false);
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

      {list.kind === "tasks" && list.tasks.length === 0 ? (
        <View style={styles.centered}>
          <Text style={styles.empty}>Belum ada tugas.</Text>
          <Text style={styles.emptyHint}>
            Tugas dibuat dari obrolan dengan Bolu. Buka daftar Bolu, tanya sesuatu, dan tugasnya
            muncul di sini.
          </Text>
          <Pressable
            onPress={() => router.replace("/(tabs)/bolu")}
            accessibilityRole="button"
            style={({ pressed }) => [styles.primary, pressed && styles.pressed]}
          >
            <Text style={styles.primaryText}>Kembali ke daftar Bolu</Text>
          </Pressable>
        </View>
      ) : null}

      {list.kind === "tasks" && list.tasks.length > 0 ? (
        <FlatList
          data={list.tasks}
          keyExtractor={(task) => task.task_id}
          contentContainerStyle={styles.list}
          refreshControl={<RefreshControl refreshing={refreshing} onRefresh={() => void load("pull")} />}
          renderItem={({ item }) => {
            const state = taskState(item);
            const agent = list.agents.get(item.agent_id);
            return (
              <Pressable
                accessibilityRole="button"
                onPress={() => router.push(`/task/${item.task_id}`)}
                style={({ pressed }) => [styles.row, pressed && styles.pressed]}
              >
                <View style={styles.rowHead}>
                  <Text style={styles.title} numberOfLines={2}>
                    {item.title !== "" ? item.title : "Tugas"}
                  </Text>
                  <Text style={styles.age}>{relativeAge(item.created_at)}</Text>
                </View>
                <View style={styles.rowFoot}>
                  <Text style={[styles.state, TASK_TONE[state.tone]]}>{state.label}</Text>
                  {agent ? <Text style={styles.agent}>{agent.name}</Text> : null}
                </View>
              </Pressable>
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
  row: { backgroundColor: "#FFFFFF", borderRadius: 14, paddingHorizontal: 14, paddingVertical: 12, gap: 6 },
  rowHead: { flexDirection: "row", alignItems: "flex-start", justifyContent: "space-between", gap: 8 },
  rowFoot: { flexDirection: "row", alignItems: "center", gap: 8 },
  title: { flex: 1, fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  age: { fontSize: 12, color: "#5A5670" },
  state: { fontSize: 12, fontWeight: "700" },
  agent: { fontSize: 12, color: "#5A5670" },
  pressed: { opacity: 0.7 },
  error: { color: "#B3261E", fontSize: 14, textAlign: "center" },
  empty: { fontSize: 16, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  emptyHint: { fontSize: 13, color: "#5A5670", textAlign: "center" },
  primary: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 10 },
  primaryText: { color: "#FFFFFF", fontWeight: "700" },
  secondary: { borderRadius: 10, borderWidth: 1, borderColor: "#F28C4E", paddingHorizontal: 18, paddingVertical: 10 },
  secondaryText: { color: "#F28C4E", fontWeight: "700" },
});

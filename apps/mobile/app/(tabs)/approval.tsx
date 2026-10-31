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
import { Redirect, router, useFocusEffect, useNavigation } from "expo-router";

import { listDrafts } from "@/lib/api";
import type { ApprovalDraft } from "@/lib/api";
import { useAuth } from "@/lib/auth";

/**
 * Names the kinds a person actually meets; a kind that is not here reads as its
 * own words (`kirim faktur`) rather than as an id. Same labels as the web inbox,
 * so a draft is one name in both places.
 */
const ACTION_LABEL: Record<string, string> = {
  "gmail.send": "Kirim email",
  "whatsapp.send_message": "Kirim WhatsApp",
};

/**
 * One draft's age in the words a person would use. Coarse on purpose: the list
 * answers "how long has this been waiting", and minutes are enough for that.
 */
function relativeAge(iso: string, now = Date.now()): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const minutes = Math.floor((now - then) / 60_000);
  if (minutes < 1) return "baru saja";
  if (minutes < 60) return `${minutes} menit lalu`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} jam lalu`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "kemarin";
  if (days < 30) return `${days} hari lalu`;
  return `${Math.floor(days / 30)} bulan lalu`;
}

/** The display kind: a named kind, else its underscored id read as words. */
function kindLabel(kind: string): string {
  const named = ACTION_LABEL[kind];
  if (named) return named;
  const words = kind.replace(/[._]/g, " ").trim();
  return words === "" ? "Draf" : words;
}

/**
 * What the tab is showing. There is no "no workspace" case here: the tab is only
 * reachable behind a signed-in session, and a workspace that is missing answers
 * as an error whose retry re-resolves it.
 */
type ListState =
  | { kind: "loading" }
  | { kind: "drafts"; drafts: ApprovalDraft[]; count: number }
  | { kind: "error"; message: string };

/** Only `setOptions` is used, and only for the tab badge. */
type TabBarBadge = { setOptions(options: { tabBarBadge?: number | string }): void };

/**
 * The drafts waiting for a decision, newest first.
 *
 * The list is the API's own filtered read rather than every draft the workspace
 * has: the screen's whole promise is "nothing else is waiting", which is only
 * true of a page the API filtered to `pending`.
 *
 * The badge in the tab bar comes from `meta.count` of this same response. A
 * second endpoint would be a second answer that could disagree with the list,
 * and a timer would be a poll; refreshing on focus is enough, because a decision
 * made on the detail screen returns here.
 */
export default function ApprovalScreen() {
  const { status } = useAuth();
  const navigation = useNavigation<TabBarBadge>();
  const [list, setList] = useState<ListState>({ kind: "loading" });
  const [refreshing, setRefreshing] = useState(false);
  const [retrying, setRetrying] = useState(false);

  const load = useCallback(
    async (mode: "focus" | "pull" | "retry") => {
      if (mode === "pull") setRefreshing(true);
      if (mode === "retry") setRetrying(true);

      const result = await listDrafts({ status: "pending", limit: 50 });

      if (result.ok) {
        const count =
          typeof result.meta?.count === "number" ? result.meta.count : result.data.length;
        setList({ kind: "drafts", drafts: result.data, count });
        navigation.setOptions({ tabBarBadge: count > 0 ? count : undefined });
      } else {
        setList({ kind: "error", message: result.message });
      }

      setRefreshing(false);
      setRetrying(false);
    },
    [navigation],
  );

  // On focus rather than on mount: a decision taken on the detail screen, or a
  // draft another member decided, changes the list and the badge while this tab
  // is behind that screen. Leaving and coming back is what re-asks.
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
        </View>
      ) : null}

      {list.kind === "drafts" && list.drafts.length === 0 ? (
        <View style={styles.centered}>
          <Text style={styles.empty}>Tidak ada yang menunggu keputusan.</Text>
          <Text style={styles.emptyHint}>
            Draf muncul di sini begitu ada Bolu yang menyiapkan aksi untuk kamu setujui.
          </Text>
        </View>
      ) : null}

      {list.kind === "drafts" && list.drafts.length > 0 ? (
        <FlatList
          data={list.drafts}
          keyExtractor={(draft) => draft.draft_id}
          contentContainerStyle={styles.list}
          refreshControl={<RefreshControl refreshing={refreshing} onRefresh={() => void load("pull")} />}
          ListHeaderComponent={
            <Text style={styles.count}>
              {list.count === 1 ? "1 draf menunggu" : `${list.count} draf menunggu`}
            </Text>
          }
          renderItem={({ item }) => (
            <Pressable
              accessibilityRole="button"
              onPress={() => router.push(`/draft/${item.draft_id}`)}
              style={({ pressed }) => [styles.row, pressed && styles.pressed]}
            >
              <View style={styles.rowHead}>
                <Text style={styles.kind}>{kindLabel(item.action_kind)}</Text>
                <Text style={styles.age}>{relativeAge(item.created_at)}</Text>
              </View>
              <Text style={styles.title} numberOfLines={2}>
                {item.title !== "" ? item.title : "Draf menunggu persetujuan"}
              </Text>
              {item.summary ? (
                <Text style={styles.summary} numberOfLines={2}>
                  {item.summary}
                </Text>
              ) : null}
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
  count: { fontSize: 13, color: "#5A5670", marginBottom: 2 },
  row: { backgroundColor: "#FFFFFF", borderRadius: 14, paddingHorizontal: 14, paddingVertical: 12, gap: 4 },
  rowHead: { flexDirection: "row", alignItems: "center", justifyContent: "space-between", gap: 8 },
  kind: { flex: 1, fontSize: 12, fontWeight: "700", color: "#B4531E" },
  age: { fontSize: 12, color: "#5A5670" },
  title: { fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  summary: { fontSize: 13, color: "#5A5670" },
  pressed: { opacity: 0.7 },
  error: { color: "#B3261E", fontSize: 14, textAlign: "center" },
  empty: { fontSize: 16, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  emptyHint: { fontSize: 13, color: "#5A5670", textAlign: "center" },
  primary: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 10 },
  primaryText: { color: "#FFFFFF", fontWeight: "700" },
});

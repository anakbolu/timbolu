import { useCallback, useEffect, useState } from "react";
import {
  ActivityIndicator,
  Alert,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  View,
} from "react-native";
import { Redirect, router, useLocalSearchParams } from "expo-router";

import { approveDraft, cancelDraft, getDraft, rejectDraft } from "@/lib/api";
import type { ApprovalDraft } from "@/lib/api";
import { useAuth } from "@/lib/auth";

/** The decision in flight, so one tap cannot send two answers to one draft. */
type Decision = "approve" | "reject" | "cancel";

const STATUS_LABEL: Record<ApprovalDraft["status"], string> = {
  pending: "Menunggu keputusanmu",
  approved: "Disetujui",
  revise: "Dikembalikan untuk revisi",
  sent: "Terkirim",
  canceled: "Dibatalkan",
};

/**
 * The one sentence for a payload value. Amounts are the only values whose
 * meaning is in the key: everywhere else a number is a number.
 */
function payloadText(key: string, value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "string") return value === "" ? "—" : value;
  if (typeof value === "boolean") return value ? "ya" : "tidak";
  if (typeof value === "number") {
    const formatted = new Intl.NumberFormat("id-ID").format(value);
    return key === "amount" || key.endsWith("_amount") ? `Rp ${formatted}` : formatted;
  }
  return JSON.stringify(value);
}

/**
 * The payload as readable lines. An answer that is not an object names no
 * fields, so it renders nothing rather than "[object Object]" or a bare array.
 */
function payloadLines(payload: unknown): Array<{ label: string; value: string }> {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) return [];
  return Object.entries(payload as Record<string, unknown>)
    .filter(([, value]) => value !== undefined)
    .map(([key, value]) => ({
      // Underscores are the API's spelling, not a person's; the label reads as
      // words and the value keeps whatever the Bolu wrote.
      label: key.replace(/[._]/g, " ").trim(),
      value: payloadText(key, value),
    }));
}

/** One moment, in the app's language: a date and a time, not an ISO string. */
function momentText(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  const day = date.toLocaleDateString("id-ID", { day: "numeric", month: "long", year: "numeric" });
  const time = `${String(date.getHours()).padStart(2, "0")}.${String(date.getMinutes()).padStart(2, "0")}`;
  return `${day}, ${time}`;
}

type LoadState =
  | { kind: "loading" }
  | { kind: "draft"; draft: ApprovalDraft }
  | { kind: "missing" }
  | { kind: "error"; message: string };

/**
 * One draft: what will leave, and the one decision that allows or stops it.
 *
 * A decided draft stays readable and its controls are gone rather than disabled:
 * the read is a record, and buttons that cannot act invite a second tap that the
 * API would refuse.
 */
export default function DraftScreen() {
  const { status } = useAuth();
  const { draftID } = useLocalSearchParams<{ draftID: string }>();
  const [state, setState] = useState<LoadState>({ kind: "loading" });
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState<Decision | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (!draftID) {
      setState({ kind: "missing" });
      return;
    }

    setState({ kind: "loading" });
    const result = await getDraft(draftID);

    if (result.ok) {
      setState({ kind: "draft", draft: result.data });
      return;
    }
    // A 404 is the API's answer for "no such draft" and for another workspace's
    // draft alike, so it reads as missing rather than as a retryable failure.
    if (result.code === "draft_not_found" || result.status === 404) {
      setState({ kind: "missing" });
      return;
    }
    setState({ kind: "error", message: result.message });
  }, [draftID]);

  useEffect(() => {
    if (status !== "signed-in") return;
    void load();
  }, [status, load]);

  if (status === "signed-out") {
    return <Redirect href="/login" />;
  }

  async function decide(decision: Decision, draft: ApprovalDraft) {
    const note = reason.trim();
    if (decision === "reject" && note === "") return;

    setBusy(decision);
    setFailure(null);

    // The note belongs to "Tolak" in this screen: it is the reason the Bolu
    // reads before writing the next draft. Approving and cancelling carry the
    // decision alone, so a rejection written and then reconsidered cannot
    // travel as an approval note.
    const result =
      decision === "approve"
        ? await approveDraft(draft.draft_id)
        : decision === "reject"
          ? await rejectDraft(draft.draft_id, note)
          : await cancelDraft(draft.draft_id);

    setBusy(null);

    if (result.ok) {
      router.back();
      return;
    }
    // The first decision stands, which is a fact about the draft rather than an
    // error to retry: the person is told and taken back to the list, where the
    // draft is no longer waiting.
    if (result.code === "draft_already_decided") {
      Alert.alert("Draf ini sudah diputuskan orang lain.");
      router.back();
      return;
    }
    setFailure(result.message);
  }

  function confirmCancel(draft: ApprovalDraft) {
    Alert.alert("Batalkan draf?", "Aksinya tidak akan dikirim dan tidak bisa dihidupkan lagi.", [
      { text: "Tidak", style: "cancel" },
      { text: "Batalkan draf", style: "destructive", onPress: () => void decide("cancel", draft) },
    ]);
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
        <Text style={styles.empty}>Draf tidak ditemukan.</Text>
        <Text style={styles.emptyHint}>
          Mungkin sudah dihapus, atau draf ini milik ruang kerja lain.
        </Text>
        <Pressable onPress={() => router.back()} accessibilityRole="button" style={styles.primary}>
          <Text style={styles.primaryText}>Kembali</Text>
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
      </View>
    );
  }

  const draft = state.draft;
  const open = draft.status === "pending";
  const lines = payloadLines(draft.payload);
  const rejectReady = reason.trim() !== "";

  return (
    <KeyboardAvoidingView
      style={styles.page}
      behavior={Platform.OS === "ios" ? "padding" : undefined}
      keyboardVerticalOffset={Platform.OS === "ios" ? 88 : 0}
    >
      <ScrollView contentContainerStyle={styles.content}>
        <Text style={styles.kind}>{draft.action_kind.replace(/[._]/g, " ").trim() || "Draf"}</Text>
        <Text style={styles.title}>{draft.title !== "" ? draft.title : "Draf menunggu persetujuan"}</Text>
        <Text style={styles.meta}>
          {STATUS_LABEL[draft.status] ?? draft.status} · dibuat {momentText(draft.created_at)}
        </Text>

        {draft.summary ? <Text style={styles.summary}>{draft.summary}</Text> : null}

        {draft.blocked_reason ? (
          <View style={styles.notice}>
            <Text style={styles.noticeText}>Ditahan: {draft.blocked_reason}</Text>
            {draft.not_before ? (
              <Text style={styles.noticeText}>Dikirim setelah {momentText(draft.not_before)}.</Text>
            ) : null}
          </View>
        ) : null}

        {lines.length > 0 ? (
          <View style={styles.card}>
            <Text style={styles.cardHeading}>Isi aksi</Text>
            {lines.map((line) => (
              <View key={line.label} style={styles.field}>
                <Text style={styles.fieldLabel}>{line.label}</Text>
                <Text style={styles.fieldValue}>{line.value}</Text>
              </View>
            ))}
          </View>
        ) : null}

        {open ? (
          <View style={styles.card}>
            <Text style={styles.cardHeading}>Keputusan</Text>

            {failure !== null ? <Text style={styles.error}>{failure}</Text> : null}

            <Text style={styles.fieldLabel}>Alasan (wajib untuk menolak)</Text>
            <TextInput
              value={reason}
              onChangeText={setReason}
              placeholder="Apa yang perlu diubah?"
              style={styles.input}
              multiline
              editable={busy === null}
            />

            <Pressable
              onPress={() => void decide("approve", draft)}
              accessibilityRole="button"
              accessibilityState={{ disabled: busy !== null }}
              disabled={busy !== null}
              style={({ pressed }) => [styles.primary, (pressed || busy !== null) && styles.pressed]}
            >
              <Text style={styles.primaryText}>{busy === "approve" ? "Menyetujui…" : "Setujui"}</Text>
            </Pressable>

            <Pressable
              onPress={() => void decide("reject", draft)}
              accessibilityRole="button"
              accessibilityState={{ disabled: busy !== null || !rejectReady }}
              disabled={busy !== null || !rejectReady}
              style={({ pressed }) => [
                styles.secondary,
                (pressed || busy !== null || !rejectReady) && styles.pressed,
              ]}
            >
              <Text style={styles.secondaryText}>{busy === "reject" ? "Mengirim…" : "Tolak"}</Text>
            </Pressable>

            <Pressable
              onPress={() => confirmCancel(draft)}
              accessibilityRole="button"
              accessibilityState={{ disabled: busy !== null }}
              disabled={busy !== null}
              style={({ pressed }) => [styles.ghost, (pressed || busy !== null) && styles.pressed]}
            >
              <Text style={styles.ghostText}>{busy === "cancel" ? "Membatalkan…" : "Batalkan"}</Text>
            </Pressable>
          </View>
        ) : (
          <View style={styles.card}>
            <Text style={styles.cardHeading}>Keputusan</Text>
            <Text style={styles.fieldValue}>{STATUS_LABEL[draft.status] ?? draft.status}.</Text>
            {draft.decided_at ? (
              <Text style={styles.fieldLabel}>Diputuskan {momentText(draft.decided_at)}.</Text>
            ) : null}
            {draft.decision_note ? <Text style={styles.fieldValue}>{draft.decision_note}</Text> : null}
          </View>
        )}
      </ScrollView>
    </KeyboardAvoidingView>
  );
}

const styles = StyleSheet.create({
  page: { flex: 1, backgroundColor: "#FBF7F2" },
  content: { padding: 16, gap: 10 },
  centered: { flex: 1, alignItems: "center", justifyContent: "center", paddingHorizontal: 32, gap: 10 },
  kind: { fontSize: 12, fontWeight: "700", color: "#B4531E" },
  title: { fontSize: 20, fontWeight: "700", color: "#1E1B2E" },
  meta: { fontSize: 13, color: "#5A5670" },
  summary: { fontSize: 15, lineHeight: 21, color: "#1E1B2E" },
  notice: { backgroundColor: "#FFF1E4", borderRadius: 12, padding: 12, gap: 4 },
  noticeText: { fontSize: 13, color: "#6B3A16" },
  card: { backgroundColor: "#FFFFFF", borderRadius: 14, padding: 14, gap: 10 },
  cardHeading: { fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  field: { gap: 2 },
  fieldLabel: { fontSize: 12, fontWeight: "600", color: "#5A5670" },
  fieldValue: { fontSize: 15, color: "#1E1B2E" },
  input: {
    minHeight: 80,
    backgroundColor: "#FBF7F2",
    borderWidth: 1,
    borderColor: "#E2DCD4",
    borderRadius: 12,
    paddingHorizontal: 12,
    paddingVertical: 10,
    fontSize: 15,
    color: "#1E1B2E",
    textAlignVertical: "top",
  },
  primary: { backgroundColor: "#F28C4E", borderRadius: 10, paddingHorizontal: 18, paddingVertical: 12, alignItems: "center" },
  primaryText: { color: "#FFFFFF", fontWeight: "700" },
  secondary: { borderRadius: 10, borderWidth: 1, borderColor: "#F28C4E", paddingVertical: 12, alignItems: "center" },
  secondaryText: { color: "#F28C4E", fontWeight: "700" },
  ghost: { paddingVertical: 10, alignItems: "center" },
  ghostText: { color: "#B3261E", fontWeight: "700" },
  pressed: { opacity: 0.6 },
  error: { color: "#B3261E", fontSize: 14, textAlign: "center" },
  empty: { fontSize: 16, fontWeight: "600", color: "#1E1B2E", textAlign: "center" },
  emptyHint: { fontSize: 13, color: "#5A5670", textAlign: "center" },
});

import { useCallback, useEffect, useRef, useState } from "react";
import {
  ActivityIndicator,
  FlatList,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from "react-native";
import { Redirect, useFocusEffect, useLocalSearchParams } from "expo-router";

import { BoluFace } from "@/components/bolu-face";
import { listMessages, messageText, openDirectConversation, sendMessage } from "@/lib/api";
import type { ChatConversation, ChatMessage } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { applyPage, connectChatStream, insertMessage } from "@/lib/chat-stream";

/**
 * How long after a send the reply is also read once, whatever the stream says.
 *
 * The live feed is the fast path, and it is not the only one. A one-shot read
 * covers a stream that is down, a frame that was dropped while the phone slept,
 * and the window between tapping Kirim and the stream's next event; the
 * pull-to-refresh and the "Muat ulang" button cover the rest. It is one read per
 * send, so it cannot become a polling loop on the battery.
 */
const REPLY_READ_DELAY_MS = 5_000;

/** The 1:1 thread with one Bolu: its history, and one box to add to it. */
export default function ChatScreen() {
  const { status } = useAuth();
  const { agentID } = useLocalSearchParams<{ agentID: string }>();
  const [conversation, setConversation] = useState<ChatConversation | null>(null);
  const [messages, setMessages] = useState<ChatMessage[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  // A screen that unmounted must not push the reply's read into a list nobody
  // is looking at. A flag rather than a cleared timer: the wait holds no handle
  // worth typing, and a pending one costs five seconds of nothing.
  const gone = useRef(false);
  /**
   * The last event id this screen saw.
   *
   * In memory only: it exists so a connection opened again after a blur resumes
   * from here instead of asking the server to replay the whole workspace. The
   * workspace id is what the server keys it by, and that never changes under an
   * open screen.
   */
  const lastEventId = useRef(0);

  /**
   * Puts one streamed message into the open thread.
   *
   * `insertMessage` owns the ordering and the de-duplication; this callback only
   * says which list it applies to. A frame for another thread never reaches
   * here, because the stream is opened for one conversation.
   */
  const applyStreamedMessage = useCallback((incoming: ChatMessage) => {
    // The first page has not landed yet; `reload` is about to read a page that
    // already includes this message, so there is nothing to insert into.
    setMessages((current) => (current === null ? current : insertMessage(current, incoming)));
  }, []);

  const conversationId = conversation?.id ?? null;

  /**
   * The live feed, for as long as this screen is the one on top.
   *
   * `useFocusEffect` rather than `useEffect` is the whole of "never two
   * connections": the cleanup runs on blur as well as on unmount, so a screen
   * pushed behind another one stops listening and starts again when it returns.
   * The cursor travels through the ref, so the return resumes instead of
   * replaying.
   */
  useFocusEffect(
    useCallback(() => {
      if (status !== "signed-in" || conversationId === null) return;
      const stream = connectChatStream(conversationId, applyStreamedMessage, lastEventId.current);
      return () => {
        lastEventId.current = stream.lastEventId();
        stream.close();
      };
    }, [status, conversationId, applyStreamedMessage]),
  );

  const reload = useCallback(async () => {
    if (!agentID) return;
    const opened = await openDirectConversation(agentID);
    if (!opened.ok) {
      setError(opened.message);
      return;
    }
    setConversation(opened.data);

    const page = await listMessages(opened.data.id);
    if (!page.ok) {
      setError(page.message);
      return;
    }
    // Kept in the API's order — newest first — because the list is `inverted`,
    // which puts index zero at the bottom where a chat's newest message belongs.
    // The merge keeps a frame that arrived while this read was in flight.
    setMessages((current) => applyPage(current, page.data.messages));
    setError(null);
  }, [agentID]);

  useEffect(() => {
    if (status !== "signed-in") return;
    gone.current = false;
    void reload();
    return () => {
      gone.current = true;
    };
  }, [status, reload]);

  if (status === "signed-out") {
    return <Redirect href="/login" />;
  }

  async function submit() {
    const text = draft.trim();
    if (text === "" || sending || conversation === null) return;

    setSending(true);
    setError(null);
    const result = await sendMessage(conversation.id, text);
    setSending(false);

    if (!result.ok) {
      // The draft is kept: a failed send is the one case where the words are
      // still only on screen.
      setError(result.message);
      return;
    }

    setDraft("");
    void reload();

    // The answer is a durable task, so it lands a moment after the send returns.
    // One delayed read is enough for the scaffold; the pull-to-refresh covers
    // answers that take longer.
    await new Promise((resolve) => setTimeout(resolve, REPLY_READ_DELAY_MS));
    if (!gone.current) void reload();
  }

  async function refresh() {
    setRefreshing(true);
    await reload();
    setRefreshing(false);
  }

  const bolu = conversation?.participants.find((participant) => participant.is_agent);
  const header = (
    <View style={styles.header}>
      <BoluFace shape="bulat" size={36} label={bolu?.name.slice(0, 1)} />
      <View style={{ flex: 1 }}>
        <Text style={styles.headerName}>{bolu?.name ?? "Bolu"}</Text>
        <Text style={styles.headerRole}>{bolu?.role ?? ""}</Text>
      </View>
      <Pressable onPress={() => void refresh()} accessibilityRole="button" hitSlop={10}>
        <Text style={styles.headerAction}>{refreshing ? "Memuat…" : "Muat ulang"}</Text>
      </Pressable>
    </View>
  );

  return (
    <KeyboardAvoidingView
      style={styles.page}
      behavior={Platform.OS === "ios" ? "padding" : undefined}
      keyboardVerticalOffset={Platform.OS === "ios" ? 88 : 0}
    >
      {header}

      {messages === null && error === null ? (
        <View style={styles.centered}>
          <ActivityIndicator />
        </View>
      ) : null}

      {error !== null ? (
        <View style={styles.errorBar}>
          <Text style={styles.errorText}>{error}</Text>
        </View>
      ) : null}

      {messages !== null && messages.length > 0 ? (
        <FlatList
          data={messages}
          keyExtractor={(message) => message.message_id}
          contentContainerStyle={styles.list}
          // A chat sits at its newest end, which is the bottom.
          inverted
          renderItem={({ item }) => <MessageBubble message={item} />}
        />
      ) : null}

      {messages !== null && messages.length === 0 ? (
        <View style={styles.centered}>
          <Text style={styles.emptyHint}>Belum ada pesan. Tulis sesuatu untuk mulai.</Text>
        </View>
      ) : null}

      <View style={styles.composer}>
        <TextInput
          value={draft}
          onChangeText={setDraft}
          placeholder="Tulis pesan…"
          style={styles.input}
          multiline
          editable={!sending && conversation !== null}
        />
        <Pressable
          onPress={() => void submit()}
          disabled={sending || draft.trim() === "" || conversation === null}
          accessibilityRole="button"
          style={({ pressed }) => [
            styles.send,
            (pressed || sending || draft.trim() === "") && styles.sendDisabled,
          ]}
        >
          <Text style={styles.sendText}>{sending ? "…" : "Kirim"}</Text>
        </Pressable>
      </View>
    </KeyboardAvoidingView>
  );
}

/**
 * One message: the user's own on the right, the Bolu's on the left. The list is
 * `inverted`, so this bubble is drawn in the orientation it is read in and the
 * flip is the list's business.
 */
function MessageBubble({ message }: { message: ChatMessage }) {
  const mine = message.user_id !== undefined && message.agent_id === undefined;
  const text = messageText(message.blocks);
  const pending = message.status === "streaming";

  return (
    <View style={[styles.bubbleRow, mine ? styles.bubbleRowMine : styles.bubbleRowTheirs]}>
      <View style={[styles.bubble, mine ? styles.bubbleMine : styles.bubbleTheirs]}>
        <Text style={[styles.bubbleText, mine && styles.bubbleTextMine]}>
          {text !== "" ? text : pending ? "Bolu sedang menyusun jawaban…" : "(pesan tanpa teks)"}
        </Text>
        {message.status === "failed" ? <Text style={styles.failed}>Jawaban gagal.</Text> : null}
        {message.status === "partial" ? <Text style={styles.failed}>Jawaban terpotong.</Text> : null}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  page: { flex: 1, backgroundColor: "#FBF7F2" },
  centered: { flex: 1, alignItems: "center", justifyContent: "center" },
  header: {
    flexDirection: "row",
    alignItems: "center",
    gap: 10,
    paddingHorizontal: 16,
    paddingVertical: 10,
    backgroundColor: "#FFFFFF",
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: "#E2DCD4",
  },
  headerName: { fontSize: 16, fontWeight: "700", color: "#1E1B2E" },
  headerRole: { fontSize: 12, color: "#5A5670" },
  headerAction: { fontSize: 13, fontWeight: "600", color: "#F28C4E" },
  errorBar: { backgroundColor: "#FDECEA", paddingHorizontal: 16, paddingVertical: 10 },
  errorText: { color: "#B3261E", fontSize: 13 },
  list: { padding: 16, gap: 8, flexGrow: 1 },
  bubbleRow: { flexDirection: "row" },
  bubbleRowMine: { justifyContent: "flex-end" },
  bubbleRowTheirs: { justifyContent: "flex-start" },
  bubble: { maxWidth: "86%", borderRadius: 14, paddingHorizontal: 12, paddingVertical: 9 },
  bubbleMine: { backgroundColor: "#F28C4E" },
  bubbleTheirs: { backgroundColor: "#FFFFFF" },
  bubbleText: { fontSize: 15, lineHeight: 21, color: "#1E1B2E" },
  bubbleTextMine: { color: "#FFFFFF" },
  failed: { marginTop: 4, fontSize: 12, color: "#B3261E" },
  emptyHint: { textAlign: "center", color: "#5A5670", fontSize: 13, paddingHorizontal: 32 },
  composer: {
    flexDirection: "row",
    alignItems: "flex-end",
    gap: 8,
    padding: 12,
    backgroundColor: "#FFFFFF",
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: "#E2DCD4",
  },
  input: {
    flex: 1,
    maxHeight: 120,
    backgroundColor: "#FBF7F2",
    borderWidth: 1,
    borderColor: "#E2DCD4",
    borderRadius: 12,
    paddingHorizontal: 12,
    paddingVertical: 10,
    fontSize: 15,
    color: "#1E1B2E",
  },
  send: { backgroundColor: "#F28C4E", borderRadius: 12, paddingHorizontal: 16, paddingVertical: 12 },
  sendDisabled: { opacity: 0.5 },
  sendText: { color: "#FFFFFF", fontWeight: "700" },
});

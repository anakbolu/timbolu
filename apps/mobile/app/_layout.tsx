import { useEffect, useSyncExternalStore } from "react";
import { Stack, router } from "expo-router";
import { StatusBar } from "expo-status-bar";
import * as Linking from "expo-linking";

import { AuthProvider, useAuth } from "@/lib/auth";
import { isAppLink, parseIncomingLink } from "@/lib/deep-link";
import { destinationFor } from "@/lib/link-destination";
import {
  getPendingLink,
  setLinkNotice,
  setPendingLink,
  subscribePendingLink,
  takePendingLink,
} from "@/lib/pending-link";

/**
 * The root: one stack, holding the gate (`index`), the sign-in form, the tabbed
 * workspace, and the chat, draft, task and routine screens opened from it, plus
 * the reader that turns an incoming link into a destination.
 */
export default function RootLayout() {
  return (
    <AuthProvider>
      <StatusBar style="dark" />
      <Stack screenOptions={{ headerShown: false }}>
        <Stack.Screen name="(tabs)" />
        <Stack.Screen name="login" />
        <Stack.Screen
          name="chat/[agentID]"
          options={{ headerShown: true, headerTitle: "Obrolan", headerBackTitle: "Bolu" }}
        />
        <Stack.Screen
          name="draft/[draftID]"
          options={{ headerShown: true, headerTitle: "Draf", headerBackTitle: "Persetujuan" }}
        />
        <Stack.Screen
          name="task/[taskID]"
          options={{ headerShown: true, headerTitle: "Tugas", headerBackTitle: "Tugas" }}
        />
        <Stack.Screen
          name="routine/[routineID]"
          options={{ headerShown: true, headerTitle: "Rutinitas", headerBackTitle: "Rutinitas" }}
        />
      </Stack>
      <IncomingLinks />
    </AuthProvider>
  );
}

/**
 * Reads links and spends them once the session allows it.
 *
 * Sits after the stack on purpose: a link is the last word about which screen
 * opens, so its navigation must commit after the gate's or the sign-in form's
 * own redirect, not race them.
 */
function IncomingLinks() {
  const { status } = useAuth();
  const pending = useSyncExternalStore(subscribePendingLink, getPendingLink);

  // The launch URL is read before the session is answered, because a link that
  // arrives while the app is dead is the common case for notifications
  // (`docs/deep-links.md` § Auth saat cold start, rule 1).
  useEffect(() => {
    const capture = (url: string | null) => {
      // Only this app's own URLs are translated here: `bolu://`, and the
      // `https://timbolu.com/...` paths the universal-link table claims. A web
      // page from another host, or one of the browser-owned timbolu.com paths,
      // belongs to the router.
      if (!url || !isAppLink(url)) return;
      setPendingLink(parseIncomingLink(url));
    };
    const subscription = Linking.addEventListener("url", ({ url }) => capture(url));
    void Linking.getInitialURL().then(capture, () => {
      // A launch URL the platform refuses to answer is no link at all; the app
      // starts at the gate as usual.
    });
    return () => subscription.remove();
  }, []);

  // A signed-out link stays pending: `signIn` flips the status, and this effect
  // runs again then, which is what lands the person on the link's destination
  // instead of the Bolu list.
  useEffect(() => {
    if (status !== "signed-in" || pending === null) return;
    const target = takePendingLink();
    if (target === null) return;
    const destination = destinationFor(target);
    setLinkNotice(destination.notice);
    // The id travels as a route param rather than through the path string, so
    // the screen reads it from `useLocalSearchParams` like every other screen
    // opened by id in this app.
    switch (destination.kind) {
      case "chat":
        router.replace({
          pathname: "/chat/thread/[conversationID]",
          params: { conversationID: destination.conversationID },
        });
        return;
      case "task":
        router.replace({ pathname: "/task/[taskID]", params: { taskID: destination.taskID } });
        return;
      case "draft":
        router.replace({ pathname: "/draft/[draftID]", params: { draftID: destination.draftID } });
        return;
      case "routine":
        router.replace({
          pathname: "/routine/[routineID]",
          params: { routineID: destination.routineID },
        });
        return;
      case "list":
        router.replace("/(tabs)/bolu");
        return;
    }
  }, [status, pending]);

  return null;
}

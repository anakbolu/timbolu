import { Stack } from "expo-router";
import { StatusBar } from "expo-status-bar";

import { AuthProvider } from "@/lib/auth";

/**
 * The root: one stack, holding the gate (`index`), the sign-in form, the tabbed
 * workspace, and the chat screen opened from it.
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
      </Stack>
    </AuthProvider>
  );
}

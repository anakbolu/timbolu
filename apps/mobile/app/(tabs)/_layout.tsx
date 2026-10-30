import { Pressable, Text } from "react-native";
import { Tabs } from "expo-router";

import { useAuth } from "@/lib/auth";

/**
 * The workspace's tabs. One tab today — the list of Bolu — because the rest of
 * the screens are the next issues; the group exists now so a second tab is a
 * file, not a restructure.
 *
 * There are no icons: the only icon source this scaffold could use is a package
 * the issue deliberately leaves out.
 */
export default function TabsLayout() {
  const { signOut } = useAuth();

  return (
    <Tabs screenOptions={{ tabBarActiveTintColor: "#F28C4E", headerTitleAlign: "center" }}>
      <Tabs.Screen
        name="bolu"
        options={{
          title: "Bolu",
          tabBarLabel: "Bolu",
          headerRight: () => (
            <Pressable onPress={signOut} accessibilityRole="button" hitSlop={12} style={{ paddingHorizontal: 16 }}>
              <Text style={{ color: "#B3261E", fontWeight: "600" }}>Keluar</Text>
            </Pressable>
          ),
        }}
      />
    </Tabs>
  );
}

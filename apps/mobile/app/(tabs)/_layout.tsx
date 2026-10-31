import { Pressable, Text } from "react-native";
import { Tabs } from "expo-router";

import { useAuth } from "@/lib/auth";

/**
 * The workspace's tabs: the list of Bolu, the drafts waiting for a decision, and
 * the tasks the Bolu have run. There are no icons: the only icon source this
 * scaffold could use is a package the issue deliberately leaves out. The approval
 * tab's badge is set by that screen from the count its own list answered with.
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
      <Tabs.Screen
        name="approval"
        options={{ title: "Persetujuan", tabBarLabel: "Persetujuan" }}
      />
      <Tabs.Screen
        name="tasks"
        options={{ title: "Tugas", tabBarLabel: "Tugas" }}
      />
    </Tabs>
  );
}

import { ActivityIndicator, StyleSheet, View } from "react-native";
import { Redirect } from "expo-router";

import { useAuth } from "@/lib/auth";

/**
 * The gate. It waits for the stored session to be answered instead of showing
 * the sign-in form first: a flash of "masuk" at somebody who is already signed
 * in reads as a bug, and the restore takes one round trip.
 */
export default function Index() {
  const { status } = useAuth();

  if (status === "restoring") {
    return (
      <View style={styles.centered}>
        <ActivityIndicator />
      </View>
    );
  }

  return <Redirect href={status === "signed-in" ? "/(tabs)/bolu" : "/login"} />;
}

const styles = StyleSheet.create({
  centered: { flex: 1, alignItems: "center", justifyContent: "center" },
});

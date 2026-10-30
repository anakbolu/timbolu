import { useState } from "react";
import {
  ActivityIndicator,
  KeyboardAvoidingView,
  Platform,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from "react-native";
import { Redirect } from "expo-router";
// React Native's own `SafeAreaView` is deprecated in 0.86; this one is what
// expo-router already pulls in, and it reads the real insets on both platforms.
import { SafeAreaView } from "react-native-safe-area-context";

import { useAuth } from "@/lib/auth";

/**
 * Sign-in. The form is deliberately the only way in: a native client has no
 * cookie channel for the browser flows, and the password path is the one the API
 * answers with a session a phone can hold.
 */
export default function LoginScreen() {
  const { status, signIn } = useAuth();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (status === "signed-in") {
    return <Redirect href="/(tabs)/bolu" />;
  }

  async function submit() {
    if (busy) return;

    const address = email.trim();
    // Checked here rather than left to the API: a form that answers "email atau
    // kata sandi salah" for an empty field sends the person looking at the wrong
    // thing.
    if (address === "" || password === "") {
      setError("Isi email dan kata sandi dulu.");
      return;
    }

    setBusy(true);
    setError(null);
    const result = await signIn(address, password);
    setBusy(false);
    // A success is not followed by a redirect here: the provider flips the
    // status, and the guard above takes over on the next render.
    if (!result.ok) setError(result.message);
  }

  return (
    <SafeAreaView style={styles.page}>
      <KeyboardAvoidingView
        style={styles.page}
        behavior={Platform.OS === "ios" ? "padding" : undefined}
      >
        <View style={styles.card}>
          <Text style={styles.brand}>Timbolu</Text>
          <Text style={styles.lead}>Masuk untuk bekerja bersama Bolu-mu.</Text>

          <Text style={styles.label}>Email</Text>
          <TextInput
            value={email}
            onChangeText={setEmail}
            autoCapitalize="none"
            autoComplete="email"
            keyboardType="email-address"
            inputMode="email"
            placeholder="nama@perusahaan.com"
            style={styles.input}
            editable={!busy}
            returnKeyType="next"
          />

          <Text style={styles.label}>Kata sandi</Text>
          <TextInput
            value={password}
            onChangeText={setPassword}
            secureTextEntry
            autoComplete="password"
            placeholder="Kata sandi"
            style={styles.input}
            editable={!busy}
            returnKeyType="go"
            onSubmitEditing={submit}
          />

          {error !== null ? <Text style={styles.error}>{error}</Text> : null}

          <Pressable
            onPress={submit}
            disabled={busy}
            accessibilityRole="button"
            style={({ pressed }) => [styles.button, (pressed || busy) && styles.buttonPressed]}
          >
            {busy ? <ActivityIndicator color="#FFFFFF" /> : <Text style={styles.buttonText}>Masuk</Text>}
          </Pressable>
        </View>
      </KeyboardAvoidingView>
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  page: { flex: 1, backgroundColor: "#FBF7F2" },
  card: { flex: 1, justifyContent: "center", paddingHorizontal: 24, gap: 8 },
  brand: { fontSize: 32, fontWeight: "700", color: "#1E1B2E" },
  lead: { fontSize: 15, color: "#5A5670", marginBottom: 20 },
  label: { fontSize: 13, fontWeight: "600", color: "#3A3550", marginTop: 12 },
  input: {
    backgroundColor: "#FFFFFF",
    borderWidth: 1,
    borderColor: "#E2DCD4",
    borderRadius: 12,
    paddingHorizontal: 14,
    paddingVertical: 12,
    fontSize: 16,
    color: "#1E1B2E",
  },
  error: { color: "#B3261E", fontSize: 14, marginTop: 12 },
  button: {
    marginTop: 24,
    backgroundColor: "#F28C4E",
    borderRadius: 12,
    paddingVertical: 14,
    alignItems: "center",
  },
  buttonPressed: { opacity: 0.7 },
  buttonText: { color: "#FFFFFF", fontSize: 16, fontWeight: "700" },
});

/**
 * The signed-in session every screen reads.
 *
 * Restoring happens once, at mount, and the whole tree waits for its answer:
 * a screen that guessed "signed out" first would flash the login form at
 * somebody whose refresh token is still perfectly good.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";

import { login, logout, restoreSession } from "@/lib/api";
import type { ApiResult, SignedIn, User } from "@/lib/api";

type AuthStatus = "restoring" | "signed-out" | "signed-in";

type AuthContextValue = {
  status: AuthStatus;
  user: User | null;
  signIn: (email: string, password: string) => Promise<ApiResult<SignedIn>>;
  signOut: () => Promise<void>;
};

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("restoring");
  const [user, setUser] = useState<User | null>(null);

  useEffect(() => {
    let cancelled = false;
    restoreSession().then((result) => {
      // A screen that unmounted mid-restore must not set state; React logs the
      // warning and the value is dropped anyway.
      if (cancelled) return;
      if (result.ok) {
        setUser(result.data.session.user);
        setStatus("signed-in");
      } else {
        setStatus("signed-out");
      }
    });
    return () => {
      cancelled = true;
    };
  }, []);

  const signIn = useCallback(async (email: string, password: string) => {
    const result = await login(email, password);
    if (result.ok) {
      setUser(result.data.session.user);
      setStatus("signed-in");
    }
    return result;
  }, []);

  const signOut = useCallback(async () => {
    await logout();
    setUser(null);
    setStatus("signed-out");
  }, []);

  // The value is memoised because every screen reads it: a new object on each
  // render would re-run each screen's effects and re-fetch on every keystroke.
  const value = useMemo<AuthContextValue>(
    () => ({ status, user, signIn, signOut }),
    [status, user, signIn, signOut],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth dipakai di luar AuthProvider");
  return value;
}

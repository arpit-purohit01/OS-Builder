import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { api, setCSRF, type Membership, type User } from "./api";

interface Session {
  user: User | null;
  memberships: Membership[];
  sso: boolean;
  loading: boolean;
  refresh: () => Promise<void>;
  logout: () => Promise<void>;
}

const Ctx = createContext<Session>(null as unknown as Session);
export const useSession = () => useContext(Ctx);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [state, set] = useState<Omit<Session, "refresh" | "logout">>({ user: null, memberships: [], sso: false, loading: true });
  const refresh = useCallback(async () => {
    try {
      const me = await api<{ user: User; memberships: Membership[]; csrf: string; sso: boolean }>("GET", "/v1/me");
      setCSRF(me.csrf);
      set({ user: me.user, memberships: me.memberships ?? [], sso: me.sso, loading: false });
    } catch {
      set({ user: null, memberships: [], sso: false, loading: false });
    }
  }, []);
  const logout = useCallback(async () => {
    await api("POST", "/v1/auth/logout").catch(() => undefined);
    set({ user: null, memberships: [], sso: false, loading: false });
  }, []);
  useEffect(() => void refresh(), [refresh]);
  return <Ctx.Provider value={{ ...state, refresh, logout }}>{children}</Ctx.Provider>;
}

// useAsync runs a loader and re-runs it when deps change.
export function useAsync<T>(fn: () => Promise<T>, deps: unknown[]): { data: T | undefined; error: Error | undefined; loading: boolean; reload: () => void } {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<Error>();
  const [loading, setLoading] = useState(true);
  const [n, setN] = useState(0);
  useEffect(() => {
    let live = true;
    setLoading(true);
    fn().then(
      (d) => live && (setData(d), setError(undefined), setLoading(false)),
      (e) => live && (setError(e), setLoading(false)),
    );
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, n]);
  return { data, error, loading, reload: () => setN((x) => x + 1) };
}

export function fmtBytes(n: number): string {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) (n /= 1024), i++;
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

export function ago(iso?: string): string {
  if (!iso) return "";
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return new Date(iso).toLocaleDateString();
}

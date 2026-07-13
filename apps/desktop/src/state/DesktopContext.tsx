import {
  createContext,
  type PropsWithChildren,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { CoreGatewayError, type CoreGateway } from "../api/gateway";
import { createRuntimeGateway } from "../api/runtime";
import type {
  DesktopAction,
  DesktopActionResponse,
  DesktopSnapshot,
  LoadState,
  StreamState,
} from "../api/types";

interface Notice {
  id: number;
  tone: "success" | "danger" | "info";
  message: string;
  artifactPath: string | null;
}

interface DesktopContextValue {
  gatewayMode: "mock" | "live" | "resolving";
  endpoint: string;
  snapshot: DesktopSnapshot | null;
  loadState: LoadState;
  streamState: StreamState;
  retryAttempt: number;
  error: string | null;
  lastEventAt: string | null;
  busyAction: string | null;
  notice: Notice | null;
  refresh(): Promise<void>;
  runAction(action: DesktopAction, actionKey?: string): Promise<DesktopActionResponse | null>;
  dismissNotice(): void;
}

const DesktopContext = createContext<DesktopContextValue | null>(null);

interface DesktopProviderProps extends PropsWithChildren {
  gateway?: CoreGateway;
}

export function DesktopProvider({ gateway: injectedGateway, children }: DesktopProviderProps) {
  const [gateway, setGateway] = useState<CoreGateway | null>(injectedGateway ?? null);
  const [snapshot, setSnapshot] = useState<DesktopSnapshot | null>(null);
  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [streamState, setStreamState] = useState<StreamState>("connecting");
  const [retryAttempt, setRetryAttempt] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [lastEventAt, setLastEventAt] = useState<string | null>(null);
  const [busyAction, setBusyAction] = useState<string | null>(null);
  const [notice, setNotice] = useState<Notice | null>(null);
  const snapshotRef = useRef<DesktopSnapshot | null>(null);
  const refreshSequence = useRef(0);
  const noticeSequence = useRef(0);
  const refreshTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (injectedGateway) {
      setGateway(injectedGateway);
      return;
    }
    let cancelled = false;
    void createRuntimeGateway()
      .then((resolved) => {
        if (!cancelled) setGateway(resolved);
      })
      .catch((runtimeError: unknown) => {
        if (cancelled) return;
        setLoadState("failed");
        setError(errorMessage(runtimeError));
      });
    return () => {
      cancelled = true;
    };
  }, [injectedGateway]);

  const refresh = useCallback(async () => {
    if (!gateway) return;
    const sequence = ++refreshSequence.current;
    if (!snapshotRef.current) setLoadState("loading");
    setError(null);
    try {
      const next = await gateway.loadSnapshot();
      if (sequence !== refreshSequence.current) return;
      snapshotRef.current = next;
      setSnapshot(next);
      setLoadState("ready");
    } catch (refreshError) {
      if (sequence !== refreshSequence.current) return;
      if (refreshError instanceof CoreGatewayError && refreshError.kind === "cancelled") return;
      setError(errorMessage(refreshError));
      setLoadState(refreshError instanceof CoreGatewayError && refreshError.kind === "disconnected" ? "disconnected" : "failed");
    }
  }, [gateway]);

  useEffect(() => {
    if (!gateway) return;
    let active = true;
    void refresh();
    const disconnect = gateway.connectEvents({
      onEvent: (event) => {
        if (!active) return;
        setLastEventAt(event.occurred_at);
        if (event.type === "heartbeat") return;
        if (snapshotRef.current && event.data.revision < snapshotRef.current.revision) return;
        if (refreshTimer.current) clearTimeout(refreshTimer.current);
        refreshTimer.current = setTimeout(() => void refresh(), 60);
      },
      onState: (state, attempt) => {
        if (!active) return;
        setStreamState(state);
        setRetryAttempt(attempt);
        if (state === "connected") void refresh();
        if (!snapshotRef.current) {
          if (state === "retrying") setLoadState("retrying");
          if (state === "disconnected") setLoadState("disconnected");
          if (state === "failed") setLoadState("failed");
        }
      },
      onError: (streamError) => {
        if (active && !snapshotRef.current) setError(streamError.message);
      },
    });
    return () => {
      active = false;
      disconnect();
      if (refreshTimer.current) clearTimeout(refreshTimer.current);
      refreshTimer.current = null;
    };
  }, [gateway, refresh]);

  useEffect(() => {
    if (!snapshot) return;
    const theme = snapshot.settings.theme;
    const colorScheme = matchMedia("(prefers-color-scheme: dark)");
    const applyTheme = () => {
      const dark = theme === "dark" || (theme === "system" && colorScheme.matches);
      document.documentElement.dataset.theme = dark ? "dark" : "light";
    };
    applyTheme();
    const pendingCount = snapshot.approvals.filter((approval) => approval.status === "pending").length;
    void window.veqriShell?.setTrayBadge?.(pendingCount);
    if (theme !== "system") return;
    colorScheme.addEventListener("change", applyTheme);
    return () => colorScheme.removeEventListener("change", applyTheme);
  }, [snapshot]);

  const runAction = useCallback(
    async (action: DesktopAction, actionKey = action.type): Promise<DesktopActionResponse | null> => {
      if (!gateway || busyAction) return null;
      setBusyAction(actionKey);
      try {
        const response = await gateway.performAction(action);
        noticeSequence.current += 1;
        setNotice({ id: noticeSequence.current, tone: response.accepted ? "success" : "danger", message: response.message, artifactPath: response.artifact_path });
        await refresh();
        return response;
      } catch (actionError) {
        noticeSequence.current += 1;
        setNotice({ id: noticeSequence.current, tone: "danger", message: errorMessage(actionError), artifactPath: null });
        return null;
      } finally {
        setBusyAction(null);
      }
    },
    [busyAction, gateway, refresh],
  );

  const value = useMemo<DesktopContextValue>(
    () => ({
      gatewayMode: gateway?.mode ?? "resolving",
      endpoint: gateway?.endpoint ?? "Resolving native shell…",
      snapshot,
      loadState,
      streamState,
      retryAttempt,
      error,
      lastEventAt,
      busyAction,
      notice,
      refresh,
      runAction,
      dismissNotice: () => setNotice(null),
    }),
    [busyAction, error, gateway, lastEventAt, loadState, notice, refresh, retryAttempt, runAction, snapshot, streamState],
  );

  return <DesktopContext.Provider value={value}>{children}</DesktopContext.Provider>;
}

export function useDesktop(): DesktopContextValue {
  const value = useContext(DesktopContext);
  if (!value) throw new Error("useDesktop must be used inside DesktopProvider.");
  return value;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "An unexpected desktop client error occurred.";
}

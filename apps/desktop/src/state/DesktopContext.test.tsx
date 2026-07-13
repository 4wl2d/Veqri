import userEvent from "@testing-library/user-event";
import { act, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { CoreGatewayError, type CoreGateway, type StreamListeners } from "../api/gateway";
import type { DesktopEvent, DesktopSnapshot } from "../api/types";
import { createMockSnapshot } from "../data/mockSnapshot";
import { DesktopProvider, useDesktop } from "./DesktopContext";

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((next) => { resolve = next; });
  return { promise, resolve };
}

function SnapshotProbe() {
  const { snapshot, refresh } = useDesktop();
  return (
    <div>
      <span>revision {snapshot?.revision ?? "none"}</span>
      <button onClick={() => void refresh()}>Refresh</button>
    </div>
  );
}

function StateProbe() {
  const { loadState, error } = useDesktop();
  return <div><span>state {loadState}</span><span>{error}</span></div>;
}

function snapshotEvent(revision: number): DesktopEvent {
  return {
    id: `event-${revision}`,
    type: "snapshot.changed",
    occurred_at: "2026-07-12T09:42:18.000Z",
    correlation_id: null,
    sequence: revision,
    data: { revision },
  };
}

describe("DesktopProvider snapshot ordering", () => {
  it("does not let an older concurrent response replace newer state", async () => {
    const loads: Array<Deferred<DesktopSnapshot>> = [];
    const gateway: CoreGateway = {
      mode: "mock",
      endpoint: "controlled test gateway",
      loadSnapshot: () => {
        const load = deferred<DesktopSnapshot>();
        loads.push(load);
        return load.promise;
      },
      performAction: async () => { throw new Error("not used"); },
      connectEvents: () => () => undefined,
    };
    render(<DesktopProvider gateway={gateway}><SnapshotProbe /></DesktopProvider>);
    await waitFor(() => expect(loads).toHaveLength(1));
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(loads).toHaveLength(2));

    const newer = createMockSnapshot();
    newer.revision = 2;
    loads[1]!.resolve(newer);
    expect(await screen.findByText("revision 2")).toBeInTheDocument();

    const older = createMockSnapshot();
    older.revision = 1;
    loads[0]!.resolve(older);
    await waitFor(() => expect(screen.getByText("revision 2")).toBeInTheDocument());
  });

  it("ignores older events but reloads equal revisions because Core uses millisecond timestamps", async () => {
    const snapshot = createMockSnapshot();
    snapshot.revision = 10;
    const loadSnapshot = vi.fn(async () => structuredClone(snapshot));
    let listeners: StreamListeners | null = null;
    const gateway: CoreGateway = {
      mode: "mock",
      endpoint: "controlled test gateway",
      loadSnapshot,
      performAction: async () => { throw new Error("not used"); },
      connectEvents: (next) => {
        listeners = next;
        return () => undefined;
      },
    };
    render(<DesktopProvider gateway={gateway}><SnapshotProbe /></DesktopProvider>);
    expect(await screen.findByText("revision 10")).toBeInTheDocument();
    expect(loadSnapshot).toHaveBeenCalledTimes(1);

    vi.useFakeTimers();
    try {
      act(() => listeners?.onEvent(snapshotEvent(9)));
      await act(async () => vi.advanceTimersByTimeAsync(61));
      expect(loadSnapshot).toHaveBeenCalledTimes(1);

      act(() => listeners?.onEvent(snapshotEvent(10)));
      await act(async () => vi.advanceTimersByTimeAsync(61));
      expect(loadSnapshot).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("DesktopProvider connection state", () => {
  it("leaves retrying state when the stream exhausts its retries", async () => {
    let listeners: StreamListeners | null = null;
    const gateway: CoreGateway = {
      mode: "live",
      endpoint: "http://127.0.0.1:7342",
      loadSnapshot: async () => { throw new CoreGatewayError("disconnected", "Core is offline."); },
      performAction: async () => { throw new Error("not used"); },
      connectEvents: (next) => {
        listeners = next;
        return () => undefined;
      },
    };
    render(<DesktopProvider gateway={gateway}><StateProbe /></DesktopProvider>);
    expect(await screen.findByText("state disconnected")).toBeInTheDocument();

    act(() => listeners?.onState("retrying", 8));
    expect(screen.getByText("state retrying")).toBeInTheDocument();
    act(() => listeners?.onState("failed", 8));
    expect(screen.getByText("state failed")).toBeInTheDocument();
  });
});

describe("DesktopProvider theme", () => {
  it("tracks OS color-scheme changes while system theme is selected", async () => {
    let dark = false;
    let onChange: (() => void) | null = null;
    const removeEventListener = vi.fn();
    const mediaQuery = {
      get matches() { return dark; },
      media: "(prefers-color-scheme: dark)",
      onchange: null,
      addEventListener: vi.fn((_type: string, listener: () => void) => { onChange = listener; }),
      removeEventListener,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    } as unknown as MediaQueryList;
    const matchMedia = vi.spyOn(window, "matchMedia").mockReturnValue(mediaQuery);
    const snapshot = createMockSnapshot();
    snapshot.settings.theme = "system";
    const gateway: CoreGateway = {
      mode: "mock",
      endpoint: "controlled test gateway",
      loadSnapshot: async () => structuredClone(snapshot),
      performAction: async () => { throw new Error("not used"); },
      connectEvents: () => () => undefined,
    };
    const view = render(<DesktopProvider gateway={gateway}><SnapshotProbe /></DesktopProvider>);

    try {
      await waitFor(() => expect(document.documentElement.dataset.theme).toBe("light"));
      act(() => {
        dark = true;
        onChange?.();
      });
      expect(document.documentElement.dataset.theme).toBe("dark");
    } finally {
      view.unmount();
      matchMedia.mockRestore();
    }
    expect(removeEventListener).toHaveBeenCalledWith("change", expect.any(Function));
  });
});

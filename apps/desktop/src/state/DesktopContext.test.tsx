import userEvent from "@testing-library/user-event";
import { render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { CoreGateway } from "../api/gateway";
import type { DesktopSnapshot } from "../api/types";
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
});

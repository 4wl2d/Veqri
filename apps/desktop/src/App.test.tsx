import userEvent from "@testing-library/user-event";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import { CoreGatewayError } from "./api/gateway";
import { MockCoreGateway } from "./api/mockGateway";
import { AppRoutes } from "./App";
import { createMockSnapshot } from "./data/mockSnapshot";
import { DesktopProvider } from "./state/DesktopContext";

function renderApp(route: string, gateway = new MockCoreGateway({ latencyMs: 0 })) {
  return render(
    <MemoryRouter initialEntries={[route]}>
      <DesktopProvider gateway={gateway}>
        <AppRoutes />
      </DesktopProvider>
    </MemoryRouter>,
  );
}

describe("desktop routes", () => {
  it.each([
    ["/", "Good morning. Veqri is ready."],
    ["/core", "Core health"],
    ["/devices", "Android devices"],
    ["/voice", "Active voice sessions"],
    ["/conversations", "Conversations"],
    ["/tasks", "Tasks"],
    ["/tasks/task-release", "Assess release readiness and summarize blockers"],
    ["/approvals", "Pending approvals"],
    ["/agents", "Agents"],
    ["/tools", "Tools & permission policies"],
    ["/connectors", "Messaging connectors"],
    ["/providers", "Voice & AI providers"],
    ["/audit", "Audit log"],
    ["/settings", "Settings"],
    ["/diagnostics", "Diagnostics & backup"],
  ])("renders %s with its primary heading", async (route, heading) => {
    renderApp(route);
    expect(await screen.findByRole("heading", { level: 1, name: heading })).toBeInTheDocument();
  });

  it("shows an explicit empty state", async () => {
    const snapshot = createMockSnapshot();
    snapshot.devices = [];
    renderApp("/devices", new MockCoreGateway({ latencyMs: 0, snapshot }));
    expect(await screen.findByRole("heading", { name: "No paired devices" })).toBeInTheDocument();
  });
});

describe("desktop safety interactions", () => {
  it("shows exact approval capability and arguments, then applies a single-use decision", async () => {
    const user = userEvent.setup();
    const gateway = new MockCoreGateway({ latencyMs: 0 });
    renderApp("/approvals", gateway);
    expect(await screen.findByText("shell.state_changing")).toBeInTheDocument();
    expect(screen.getByText("./bin/integration-tests --local")).toBeInTheDocument();
    await user.click(screen.getAllByRole("button", { name: "Approve once" })[0]!);
    expect(await screen.findByText("Single-use approval granted.")).toBeInTheDocument();
    await waitFor(() => expect(gateway.actionHistory).toContainEqual({ type: "approval.resolve", approval_id: "approval-tests", decision: "approved" }));
  });

  it("requires an explicit keyboard-dismissible confirmation before device revocation", async () => {
    const user = userEvent.setup();
    const gateway = new MockCoreGateway({ latencyMs: 0 });
    renderApp("/devices", gateway);
    await screen.findByRole("heading", { name: "Android devices" });
    await user.click(screen.getAllByRole("button", { name: "Revoke device" })[0]!);
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toHaveAccessibleName("Revoke Pixel 9 Pro?");
    expect(within(dialog).getByRole("button", { name: "Revoke device" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    expect(gateway.actionHistory).toHaveLength(0);
  });

  it("keeps network exposure read-only and saves only a working theme change", async () => {
    const user = userEvent.setup();
    const gateway = new MockCoreGateway({ latencyMs: 0 });
    renderApp("/settings", gateway);
    await screen.findByRole("heading", { name: "Settings" });
    expect(screen.getByRole("switch", { name: /Non-loopback listener active/i })).toBeDisabled();
    expect(screen.getByRole("switch", { name: /Quiet hours/i })).toBeDisabled();
    await user.selectOptions(screen.getByRole("combobox", { name: /Color theme/i }), "light");
    await user.click(screen.getByRole("button", { name: "Save theme" }));
    await waitFor(() => expect(gateway.actionHistory).toContainEqual({ type: "settings.update", patch: { theme: "light" } }));
  });
});

describe("connection state", () => {
  it("renders a disconnected state with an explicit retry action", async () => {
    const gateway = new MockCoreGateway({
      latencyMs: 0,
      initiallyConnected: false,
      loadError: new CoreGatewayError("disconnected", "Core process is not listening."),
    });
    renderApp("/", gateway);
    expect(await screen.findByRole("heading", { name: "Core is disconnected" })).toBeInTheDocument();
    expect(screen.getByText("Core process is not listening.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry now" })).toBeInTheDocument();
  });
});

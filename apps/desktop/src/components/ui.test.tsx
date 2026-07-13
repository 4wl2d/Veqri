import userEvent from "@testing-library/user-event";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { LoadState } from "../api/types";
import { Button, ConfirmButton, IconButton, LoadStatePanel } from "./ui";

describe("explicit resource states", () => {
  it.each([
    ["loading", "Loading local state"],
    ["retrying", "Reconnecting to Core"],
    ["disconnected", "Core is disconnected"],
    ["failed", "Desktop state failed to load"],
  ] satisfies Array<[LoadState, string]>)("renders %s without hiding it behind a spinner", (state, heading) => {
    render(<LoadStatePanel state={state} error={state === "failed" ? "Protocol mismatch." : null} onRetry={vi.fn()} />);
    expect(screen.getByRole("heading", { name: heading })).toBeInTheDocument();
    if (state === "failed") expect(screen.getByText("Protocol mismatch.")).toBeInTheDocument();
  });
});

describe("shared button behavior", () => {
  it("merges caller classes without dropping required component styles", () => {
    render(<>
      <Button className="menu-button">Refresh</Button>
      <IconButton label="Open navigation" className="sidebar-toggle">menu</IconButton>
    </>);

    expect(screen.getByRole("button", { name: "Refresh" })).toHaveClass("button", "button--secondary", "menu-button");
    expect(screen.getByRole("button", { name: "Open navigation" })).toHaveClass("icon-button", "sidebar-toggle");
  });

  it("restores focus to the trigger when confirmation is dismissed", async () => {
    const user = userEvent.setup();
    render(<ConfirmButton
      label="Revoke device"
      title="Revoke this device?"
      description="The credential will stop working."
      confirmLabel="Revoke"
      onConfirm={vi.fn()}
    />);

    const trigger = screen.getByRole("button", { name: "Revoke device" });
    await user.click(trigger);
    expect(screen.getByRole("button", { name: "Revoke" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();
  });
});

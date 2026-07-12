import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { LoadState } from "../api/types";
import { LoadStatePanel } from "./ui";

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

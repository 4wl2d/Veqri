import { describe, expect, it } from "vitest";
import { MockCoreGateway } from "./mockGateway";

describe("deterministic mock gateway", () => {
  it("applies an approval exactly once and resumes the task", async () => {
    const gateway = new MockCoreGateway({ latencyMs: 0 });
    await expect(gateway.performAction({ type: "approval.resolve", approval_id: "approval-tests", decision: "approved" })).resolves.toMatchObject({
      accepted: true,
      request_id: "mock-request-1",
      revision: 43,
    });
    const snapshot = await gateway.loadSnapshot();
    expect(snapshot.approvals.find((item) => item.id === "approval-tests")?.status).toBe("approved");
    expect(snapshot.tasks.find((item) => item.id === "task-tests")?.status).toBe("RUNNING");
    await expect(gateway.performAction({ type: "approval.resolve", approval_id: "approval-tests", decision: "approved" })).rejects.toMatchObject({ status: 409 });
  });

  it("creates repeatable backup metadata and records action history", async () => {
    const gateway = new MockCoreGateway({ latencyMs: 0 });
    const response = await gateway.performAction({ type: "backup.create" });
    const snapshot = await gateway.loadSnapshot();
    expect(response.artifact_path).toBe("~/Backups/Veqri/veqri-manual-2026-07-12.db");
    expect(snapshot.diagnostics.storage.backup_count).toBe(5);
    expect(gateway.actionHistory).toEqual([{ type: "backup.create" }]);
  });

  it("reprioritizes active tasks and dismisses only terminal tasks", async () => {
    const gateway = new MockCoreGateway({ latencyMs: 0 });
    await gateway.performAction({ type: "task.reprioritize", task_id: "task-release", priority: 70 });
    await expect(gateway.performAction({ type: "task.dismiss", task_id: "task-release" })).rejects.toMatchObject({ status: 409 });
    await gateway.performAction({ type: "task.dismiss", task_id: "task-index" });
    const snapshot = await gateway.loadSnapshot();
    expect(snapshot.tasks.find((task) => task.id === "task-release")?.priority).toBe(70);
    expect(snapshot.tasks.some((task) => task.id === "task-index")).toBe(false);
  });
});

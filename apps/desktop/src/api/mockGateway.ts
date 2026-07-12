import { createMockSnapshot, MOCK_NOW } from "../data/mockSnapshot";
import { CoreGatewayError, type CoreGateway, type StreamListeners } from "./gateway";
import type { DesktopAction, DesktopActionResponse, DesktopEvent, DesktopSnapshot, StreamState } from "./types";

export interface MockGatewayOptions {
  snapshot?: DesktopSnapshot;
  latencyMs?: number;
  loadError?: Error;
  initiallyConnected?: boolean;
}

export class MockCoreGateway implements CoreGateway {
  readonly mode = "mock" as const;
  readonly endpoint = "deterministic mock";
  readonly actionHistory: DesktopAction[] = [];
  private snapshot: DesktopSnapshot;
  private readonly latencyMs: number;
  private loadError: Error | null;
  private listeners = new Set<StreamListeners>();
  private streamState: StreamState;
  private sequence = 1042;
  private requestCounter = 0;

  constructor(options: MockGatewayOptions = {}) {
    this.snapshot = structuredClone(options.snapshot ?? createMockSnapshot());
    this.latencyMs = options.latencyMs ?? 35;
    this.loadError = options.loadError ?? null;
    this.streamState = options.initiallyConnected === false ? "disconnected" : "connected";
  }

  async loadSnapshot(signal?: AbortSignal): Promise<DesktopSnapshot> {
    await delay(this.latencyMs, signal);
    if (this.loadError) throw this.loadError;
    return structuredClone(this.snapshot);
  }

  async performAction(action: DesktopAction, signal?: AbortSignal): Promise<DesktopActionResponse> {
    await delay(this.latencyMs, signal);
    this.actionHistory.push(structuredClone(action));
    let message = "Action applied in deterministic mock mode.";
    let artifactPath: string | null = null;

    switch (action.type) {
      case "approval.resolve": {
        const approval = this.snapshot.approvals.find((item) => item.id === action.approval_id);
        if (!approval || approval.status !== "pending") throw new CoreGatewayError("server", "Approval is no longer pending.", 409);
        approval.status = action.decision;
        const task = this.snapshot.tasks.find((item) => item.id === approval.task_id);
        if (task) {
          task.status = action.decision === "approved" ? "RUNNING" : "BLOCKED";
          task.summary = action.decision === "approved" ? "Approval granted; execution resumed." : "Execution blocked after approval was denied.";
        }
        message = action.decision === "approved" ? "Single-use approval granted." : "Request denied; the tool was not executed.";
        break;
      }
      case "task.cancel": {
        const task = requireEntity(this.snapshot.tasks, action.task_id, "Task");
        task.status = "CANCEL_REQUESTED";
        task.summary = "Cancellation requested; waiting for the assigned agent to acknowledge it.";
        message = "Cancellation requested.";
        break;
      }
      case "task.retry": {
        const task = requireEntity(this.snapshot.tasks, action.task_id, "Task");
        task.status = "QUEUED";
        task.progress_percent = 0;
        task.retry_count += 1;
        task.error = null;
        task.finished_at = null;
        task.summary = "Retry queued with the original policy and tool boundaries.";
        message = "Task queued for retry.";
        break;
      }
      case "task.reprioritize": {
        const task = requireEntity(this.snapshot.tasks, action.task_id, "Task");
        if (action.priority < -100 || action.priority > 100) {
          throw new CoreGatewayError("server", "Task priority must be between -100 and 100.", 400);
        }
        task.priority = action.priority;
        message = `Task priority set to ${action.priority}.`;
        break;
      }
      case "task.dismiss": {
        const task = requireEntity(this.snapshot.tasks, action.task_id, "Task");
        if (!["COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "CANCELLED", "TIMED_OUT"].includes(task.status)) {
          throw new CoreGatewayError("server", "Only terminal tasks can be dismissed.", 409);
        }
        task.dismissed = true;
        this.snapshot.tasks = this.snapshot.tasks.filter((item) => item.id !== task.id);
        message = "Task dismissed from default lists.";
        break;
      }
      case "device.revoke": {
        const device = requireEntity(this.snapshot.devices, action.device_id, "Device");
        device.status = "revoked";
        message = `${device.name} was revoked.`;
        break;
      }
      case "voice.end": {
        const session = requireEntity(this.snapshot.voice_sessions, action.session_id, "Voice session");
        session.state = "ENDED";
        session.partial_transcript = "";
        message = "Voice session ended.";
        break;
      }
      case "connector.retry": {
        const connector = requireEntity(this.snapshot.connectors, action.connector_id, "Connector");
        connector.health = "healthy";
        connector.error = null;
        message = `${connector.name} reconnected in simulator mode.`;
        break;
      }
      case "connector.kill_switch.set": {
        const connector = requireEntity(this.snapshot.connectors, action.connector_id, "Connector");
        connector.kill_switch = action.enabled;
        message = action.enabled ? `${connector.name} was stopped.` : `${connector.name} was enabled.`;
        break;
      }
      case "agent.kill_switch.set": {
        const agent = requireEntity(this.snapshot.agents, action.agent_id, "Agent");
        agent.kill_switch = action.enabled;
        message = action.enabled ? `${agent.name} will receive no new work.` : `${agent.name} can receive work again.`;
        break;
      }
      case "core.emergency_stop.set": {
        this.snapshot.core.emergency_stop = action.enabled;
        message = action.enabled ? "Emergency stop enabled; no new tool execution may start." : "Emergency stop cleared.";
        break;
      }
      case "settings.update": {
        this.snapshot.settings = { ...this.snapshot.settings, ...action.patch };
        message = "Settings saved.";
        break;
      }
      case "backup.create": {
        this.snapshot.diagnostics.storage.backup_count += 1;
        this.snapshot.diagnostics.storage.last_backup_at = MOCK_NOW;
        this.snapshot.diagnostics.storage.last_backup_path = "~/Backups/Veqri/veqri-manual-2026-07-12.db";
        artifactPath = this.snapshot.diagnostics.storage.last_backup_path;
        message = "Local SQLite backup created; storage encryption is operator-managed.";
        break;
      }
      case "diagnostics.export": {
        artifactPath = `~/Desktop/veqri-diagnostics-${action.redact ? "redacted" : "full"}.zip`;
        message = action.redact ? "Redacted diagnostic bundle created." : "Full diagnostic bundle created.";
        break;
      }
    }

    this.snapshot.revision += 1;
    this.snapshot.generated_at = MOCK_NOW;
    this.emit("snapshot.changed", null);
    this.requestCounter += 1;
    return {
      request_id: `mock-request-${this.requestCounter}`,
      accepted: true,
      occurred_at: MOCK_NOW,
      revision: this.snapshot.revision,
      message,
      artifact_path: artifactPath,
    };
  }

  connectEvents(listeners: StreamListeners): () => void {
    this.listeners.add(listeners);
    listeners.onState("connecting", 0);
    queueMicrotask(() => {
      if (this.listeners.has(listeners)) listeners.onState(this.streamState, this.streamState === "retrying" ? 1 : 0);
    });
    return () => {
      this.listeners.delete(listeners);
      listeners.onState("disconnected", 0);
    };
  }

  setStreamState(state: StreamState, retryAttempt = 0): void {
    this.streamState = state;
    for (const listener of this.listeners) listener.onState(state, retryAttempt);
  }

  setLoadError(error: Error | null): void {
    this.loadError = error;
  }

  replaceSnapshot(snapshot: DesktopSnapshot): void {
    this.snapshot = structuredClone(snapshot);
    this.emit("snapshot.changed", null);
  }

  private emit(type: DesktopEvent["type"], entityId: string | null): void {
    this.sequence += 1;
    const data: DesktopEvent["data"] = { revision: this.snapshot.revision };
    if (entityId !== null) data.entity_id = entityId;
    const event: DesktopEvent = {
      id: `mock-event-${this.sequence}`,
      type,
      occurred_at: MOCK_NOW,
      correlation_id: null,
      sequence: this.sequence,
      data,
    };
    for (const listener of this.listeners) listener.onEvent(event);
  }
}

function requireEntity<T extends { id: string }>(collection: T[], id: string, label: string): T {
  const entity = collection.find((item) => item.id === id);
  if (!entity) throw new CoreGatewayError("server", `${label} was not found.`, 404);
  return entity;
}

function delay(milliseconds: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.reject(new CoreGatewayError("cancelled", "Request cancelled."));
  if (milliseconds <= 0) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const timeout = setTimeout(resolve, milliseconds);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timeout);
        reject(new CoreGatewayError("cancelled", "Request cancelled."));
      },
      { once: true },
    );
  });
}

import { describe, expect, it, vi } from "vitest";
import { createMockSnapshot } from "../data/mockSnapshot";
import {
  AuthenticatedHttpGateway,
  CoreGatewayError,
  ReconnectingEventStream,
  validateLocalCoreUrl,
  websocketProtocols,
  type WebSocketLike,
} from "./gateway";

describe("local Core URL validation", () => {
  it.each(["http://127.0.0.1:8420", "http://localhost:8420", "https://[::1]:8420"])("accepts a local origin: %s", (value) => {
    expect(validateLocalCoreUrl(value).origin).toContain(":8420");
  });

  it.each([
    "https://core.example.com",
    "http://127.0.0.1:8420/admin",
    "http://token@127.0.0.1:8420",
    "file:///tmp/core.sock",
  ])("rejects non-local or credential-bearing input: %s", (value) => {
    expect(() => validateLocalCoreUrl(value)).toThrow(CoreGatewayError);
  });
});

describe("authenticated HTTP gateway", () => {
  it("uses Bearer auth and protocol headers without putting the token in the URL", async () => {
    const snapshot = createMockSnapshot();
    const fetchImplementation = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify(snapshot), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    }));
    const gateway = new AuthenticatedHttpGateway({
      config: { mode: "live", api_base_url: "http://127.0.0.1:8420", auth_token: "local token/with+symbols" },
      fetchImplementation,
    });

    await expect(gateway.loadSnapshot()).resolves.toEqual(snapshot);
    const [url, request] = fetchImplementation.mock.calls[0] ?? [];
    expect(String(url)).toBe("http://127.0.0.1:8420/api/v1/desktop/snapshot");
    expect(String(url)).not.toContain("local token");
    expect(new Headers(request?.headers).get("Authorization")).toBe("Bearer local token/with+symbols");
    expect(new Headers(request?.headers).get("X-Veqri-Protocol-Version")).toBe("1");
  });

  it("serializes typed action envelopes", async () => {
    const fetchImplementation = vi.fn<typeof fetch>().mockImplementation(async (_url, request) => {
      const body = JSON.parse(String(request?.body)) as { request_id: string };
      return new Response(JSON.stringify({
        request_id: body.request_id,
        accepted: true,
        occurred_at: "2026-07-12T09:42:18.000Z",
        revision: 43,
        message: "accepted",
        artifact_path: null,
      }), { status: 200, headers: { "Content-Type": "application/json" } });
    });
    const gateway = new AuthenticatedHttpGateway({
      config: { mode: "live", api_base_url: "http://localhost:8420", auth_token: "secret" },
      fetchImplementation,
    });

    await gateway.performAction({ type: "task.cancel", task_id: "task-release" });
    const [url, request] = fetchImplementation.mock.calls[0] ?? [];
    expect(String(url)).toBe("http://localhost:8420/api/v1/desktop/actions");
    const body = JSON.parse(String(request?.body)) as { request_id: string; action: unknown };
    expect(body.request_id).toEqual(expect.any(String));
    expect(body.action).toEqual({ type: "task.cancel", task_id: "task-release" });
  });

  it("rejects incomplete snapshots before unsafe fields reach React", async () => {
    const malformed: Record<string, unknown> = { ...createMockSnapshot() };
    delete malformed.settings;
    const gateway = new AuthenticatedHttpGateway({
      config: { mode: "live", api_base_url: "http://127.0.0.1:7342", auth_token: "secret" },
      fetchImplementation: vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify(malformed), { status: 200 })),
    });

    await expect(gateway.loadSnapshot()).rejects.toMatchObject({ kind: "protocol" });
  });

  it("rejects snapshots without canonical Core build metadata", async () => {
    const malformed = createMockSnapshot() as unknown as Record<string, unknown>;
    const core = malformed.core as Record<string, unknown>;
    delete core.commit;
    const gateway = new AuthenticatedHttpGateway({
      config: { mode: "live", api_base_url: "http://127.0.0.1:7342", auth_token: "secret" },
      fetchImplementation: vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify(malformed), { status: 200 })),
    });

    await expect(gateway.loadSnapshot()).rejects.toMatchObject({ kind: "protocol" });
  });

  it("classifies a successful non-JSON response as a protocol error", async () => {
    const gateway = new AuthenticatedHttpGateway({
      config: { mode: "live", api_base_url: "http://127.0.0.1:7342", auth_token: "secret" },
      fetchImplementation: vi.fn<typeof fetch>().mockResolvedValue(new Response("not-json", { status: 200 })),
    });

    await expect(gateway.loadSnapshot()).rejects.toMatchObject({ kind: "protocol" });
  });

  it("rejects an action response for a different request id", async () => {
    const gateway = new AuthenticatedHttpGateway({
      config: { mode: "live", api_base_url: "http://127.0.0.1:7342", auth_token: "secret" },
      fetchImplementation: vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({
        request_id: "another-request",
        accepted: true,
        occurred_at: "2026-07-12T09:42:18.000Z",
        revision: 43,
        message: "accepted",
        artifact_path: null,
      }), { status: 200 })),
    });

    await expect(gateway.performAction({ type: "task.cancel", task_id: "task-release" })).rejects.toMatchObject({ kind: "protocol" });
  });

  it("encodes websocket auth as subprotocols rather than a query parameter", () => {
    expect(websocketProtocols("local token/with+symbols")).toEqual([
      "veqri.v1",
      "veqri.auth.bG9jYWwgdG9rZW4vd2l0aCtzeW1ib2xz",
    ]);
  });
});

class TestSocket implements WebSocketLike {
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  close = vi.fn();
}

describe("reconnecting event stream", () => {
  it("reports bounded retry state and reconnects deterministically", () => {
    vi.useFakeTimers();
    const sockets: TestSocket[] = [];
    const states: Array<[string, number]> = [];
    const stream = new ReconnectingEventStream({
      createSocket: () => {
        const socket = new TestSocket();
        sockets.push(socket);
        return socket;
      },
      url: "ws://127.0.0.1:8420/api/v1/events",
      protocols: ["veqri.v1", "veqri.auth.dG9rZW4"],
      listeners: {
        onEvent: vi.fn(),
        onState: (state, retry) => states.push([state, retry]),
        onError: vi.fn(),
      },
      initialDelayMs: 500,
      maximumDelayMs: 1_000,
      maximumRetries: 2,
    });

    stream.start();
    expect(states).toEqual([["connecting", 0]]);
    sockets[0]?.onclose?.({} as CloseEvent);
    expect(states.at(-1)).toEqual(["retrying", 1]);
    vi.advanceTimersByTime(500);
    expect(states.at(-1)).toEqual(["connecting", 1]);
    expect(sockets).toHaveLength(2);
    sockets[1]?.onopen?.({} as Event);
    expect(states.at(-1)).toEqual(["connected", 0]);
    stream.stop();
    expect(states.at(-1)).toEqual(["disconnected", 0]);
    vi.useRealTimers();
  });

  it("rejects unknown event types instead of treating them as snapshot changes", () => {
    const socket = new TestSocket();
    const onEvent = vi.fn();
    const onError = vi.fn();
    const stream = new ReconnectingEventStream({
      createSocket: () => socket,
      url: "ws://127.0.0.1:7342/api/v1/events",
      protocols: ["veqri.v1", "veqri.auth.dG9rZW4"],
      listeners: { onEvent, onError, onState: vi.fn() },
    });

    stream.start();
    socket.onmessage?.({ data: JSON.stringify({
      id: "event-1",
      type: "tool.credentials.changed",
      occurred_at: "2026-07-12T09:42:18.000Z",
      correlation_id: null,
      sequence: 1,
      data: { revision: 43 },
    }) } as MessageEvent<string>);

    expect(onEvent).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledWith(expect.objectContaining({ kind: "protocol" }));
    stream.stop();
  });
});

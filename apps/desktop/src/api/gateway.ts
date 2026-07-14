import {
  DESKTOP_PROTOCOL_VERSION,
  type DesktopAction,
  type DesktopActionRequest,
  type DesktopActionResponse,
  type DesktopEvent,
  type DesktopSnapshot,
  type RuntimeConfig,
  type StreamState,
} from "./types";

export interface StreamListeners {
  onEvent(event: DesktopEvent): void;
  onState(state: StreamState, retryAttempt: number): void;
  onError(error: Error): void;
}

export interface CoreGateway {
  readonly mode: "mock" | "live";
  readonly endpoint: string;
  loadSnapshot(signal?: AbortSignal): Promise<DesktopSnapshot>;
  performAction(action: DesktopAction, signal?: AbortSignal): Promise<DesktopActionResponse>;
  connectEvents(listeners: StreamListeners): () => void;
}

export type CoreErrorKind = "configuration" | "authentication" | "disconnected" | "protocol" | "server" | "cancelled";

export class CoreGatewayError extends Error {
  readonly kind: CoreErrorKind;
  readonly status: number | null;

  constructor(kind: CoreErrorKind, message: string, status: number | null = null) {
    super(message);
    this.name = "CoreGatewayError";
    this.kind = kind;
    this.status = status;
  }
}

export function validateLocalCoreUrl(rawUrl: string): URL {
  let url: URL;
  try {
    url = new URL(rawUrl);
  } catch {
    throw new CoreGatewayError("configuration", "The Veqri Core URL is invalid.");
  }

  const localHosts = new Set(["localhost", "127.0.0.1", "[::1]", "::1"]);
  if (!localHosts.has(url.hostname.toLowerCase())) {
    throw new CoreGatewayError("configuration", "Desktop access is restricted to a localhost Veqri Core endpoint.");
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new CoreGatewayError("configuration", "The Veqri Core URL must use HTTP or HTTPS.");
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new CoreGatewayError("configuration", "The Veqri Core URL cannot contain credentials, query parameters, or fragments.");
  }
  if (url.pathname !== "/") {
    throw new CoreGatewayError("configuration", "The Veqri Core URL must be an origin without a path.");
  }
  return url;
}

function encodeUtf8Base64Url(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/u, "");
}

export function websocketProtocols(authToken: string): ["veqri.v1", string] {
  if (!authToken.trim()) throw new CoreGatewayError("configuration", "A local Core authentication token is required.");
  return ["veqri.v1", `veqri.auth.${encodeUtf8Base64Url(authToken)}`];
}

export interface WebSocketLike {
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent<string>) => void) | null;
  onerror: ((event: Event) => void) | null;
  onclose: ((event: CloseEvent) => void) | null;
  close(code?: number, reason?: string): void;
}

interface ReconnectingEventStreamOptions {
  createSocket: (url: string, protocols: string[]) => WebSocketLike;
  url: string;
  protocols: string[];
  listeners: StreamListeners;
  initialDelayMs?: number;
  maximumDelayMs?: number;
  maximumRetries?: number;
}

export class ReconnectingEventStream {
  private readonly createSocket: ReconnectingEventStreamOptions["createSocket"];
  private readonly url: string;
  private readonly protocols: string[];
  private readonly listeners: StreamListeners;
  private readonly initialDelayMs: number;
  private readonly maximumDelayMs: number;
  private readonly maximumRetries: number;
  private socket: WebSocketLike | null = null;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private retryAttempt = 0;
  private stopped = true;
  private generation = 0;

  constructor(options: ReconnectingEventStreamOptions) {
    this.createSocket = options.createSocket;
    this.url = options.url;
    this.protocols = options.protocols;
    this.listeners = options.listeners;
    this.initialDelayMs = options.initialDelayMs ?? 500;
    this.maximumDelayMs = options.maximumDelayMs ?? 10_000;
    this.maximumRetries = options.maximumRetries ?? 8;
  }

  start(): void {
    if (!this.stopped) return;
    this.stopped = false;
    this.retryAttempt = 0;
    this.listeners.onState("connecting", 0);
    this.open();
  }

  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    this.generation += 1;
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = null;
    const socket = this.socket;
    this.socket = null;
    socket?.close(1000, "desktop client closed");
    this.listeners.onState("disconnected", this.retryAttempt);
  }

  private open(): void {
    if (this.stopped) return;
    const generation = ++this.generation;
    let socket: WebSocketLike;
    try {
      socket = this.createSocket(this.url, this.protocols);
      this.socket = socket;
    } catch (error) {
      this.listeners.onError(toError(error, "Unable to create the Veqri event stream."));
      this.scheduleRetry(generation);
      return;
    }

    socket.onopen = () => {
      if (this.stopped || generation !== this.generation) return;
      this.retryAttempt = 0;
      this.listeners.onState("connected", 0);
    };
    socket.onmessage = (message) => {
      if (this.stopped || generation !== this.generation) return;
      try {
        this.listeners.onEvent(parseDesktopEvent(message.data));
      } catch (error) {
        this.listeners.onError(toError(error, "Veqri Core sent an invalid event."));
      }
    };
    socket.onerror = () => {
      if (this.stopped || generation !== this.generation) return;
      this.listeners.onError(new CoreGatewayError("disconnected", "The Veqri event stream encountered a network error."));
    };
    socket.onclose = () => {
      if (this.stopped || generation !== this.generation) return;
      this.socket = null;
      this.scheduleRetry(generation);
    };
  }

  private scheduleRetry(generation: number): void {
    if (this.stopped || generation !== this.generation || this.retryTimer) return;
    this.retryAttempt += 1;
    if (this.retryAttempt > this.maximumRetries) {
      this.listeners.onState("failed", this.retryAttempt - 1);
      this.listeners.onError(new CoreGatewayError("disconnected", "The event stream could not reconnect."));
      return;
    }
    this.listeners.onState("retrying", this.retryAttempt);
    const delay = Math.min(this.initialDelayMs * 2 ** (this.retryAttempt - 1), this.maximumDelayMs);
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      if (this.stopped || generation !== this.generation) return;
      this.listeners.onState("connecting", this.retryAttempt);
      this.open();
    }, delay);
  }
}

const desktopEventTypes = new Set<DesktopEvent["type"]>([
  "snapshot.changed",
  "task.changed",
  "approval.changed",
  "core.changed",
  "heartbeat",
]);

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function isProtocolNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function isUtcTimestamp(value: unknown): value is string {
  return typeof value === "string"
    && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/u.test(value)
    && !Number.isNaN(Date.parse(value));
}

function parseDesktopEvent(raw: string): DesktopEvent {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new CoreGatewayError("protocol", "Event payload is not valid JSON.");
  }
  if (!isRecord(value) || !isRecord(value.data)) {
    throw new CoreGatewayError("protocol", "Event payload does not match protocol v1.");
  }
  const entityId = value.data.entity_id;
  if (
    typeof value.id !== "string" ||
    typeof value.type !== "string" ||
    !desktopEventTypes.has(value.type as DesktopEvent["type"]) ||
    !isUtcTimestamp(value.occurred_at) ||
    (value.correlation_id !== null && typeof value.correlation_id !== "string") ||
    !isProtocolNumber(value.sequence) ||
    !isProtocolNumber(value.data.revision) ||
    (entityId !== undefined && typeof entityId !== "string")
  ) {
    throw new CoreGatewayError("protocol", "Event payload does not match protocol v1.");
  }
  return value as unknown as DesktopEvent;
}

function validateSnapshot(value: unknown): DesktopSnapshot {
  if (!isRecord(value)) throw new CoreGatewayError("protocol", "Core returned an invalid desktop snapshot.");
  if (value.protocol_version !== DESKTOP_PROTOCOL_VERSION) {
    throw new CoreGatewayError(
      "protocol",
      `Unsupported desktop protocol version ${String(value.protocol_version)}; expected ${DESKTOP_PROTOCOL_VERSION}.`,
    );
  }
  const requiredCollections: Array<keyof DesktopSnapshot> = [
    "devices",
    "voice_sessions",
    "conversations",
    "tasks",
    "agents",
    "tools",
    "policies",
    "approvals",
    "connectors",
    "providers",
    "audit_entries",
  ];
  if (
    !isProtocolNumber(value.revision)
    || !isUtcTimestamp(value.generated_at)
    || !isRecord(value.core)
    || typeof value.core.version !== "string"
    || typeof value.core.commit !== "string"
    || (value.core.build_time !== "unknown" && !isUtcTimestamp(value.core.build_time))
    || value.core.protocol_version !== DESKTOP_PROTOCOL_VERSION
    || !isRecord(value.core.database)
    || !isRecord(value.core.queue)
    || !isRecord(value.diagnostics)
    || !Array.isArray(value.diagnostics.checks)
    || !Array.isArray(value.diagnostics.recent_logs)
    || !isRecord(value.diagnostics.event_stream)
    || !isRecord(value.diagnostics.webrtc)
    || !isRecord(value.diagnostics.storage)
    || !isRecord(value.settings)
    || !new Set(["dark", "light", "system"]).has(String(value.settings.theme))
    || requiredCollections.some((key) => !Array.isArray(value[key]))
  ) {
    throw new CoreGatewayError("protocol", "Desktop snapshot is missing required protocol v1 fields.");
  }

  const snapshot = value as unknown as DesktopSnapshot;
  const collectionsAreSafe =
    snapshot.devices.every((item) => isRecord(item) && Array.isArray(item.capabilities))
    && snapshot.tasks.every((item) => isRecord(item) && Array.isArray(item.allowed_tools) && Array.isArray(item.dependencies) && Array.isArray(item.artifacts))
    && snapshot.agents.every((item) => isRecord(item) && Array.isArray(item.capabilities) && Array.isArray(item.tool_scopes))
    && snapshot.tools.every((item) => isRecord(item) && Array.isArray(item.scopes) && Array.isArray(item.supported_os))
    && snapshot.approvals.every((item) => isRecord(item) && isRecord(item.arguments));
  if (!collectionsAreSafe) {
    throw new CoreGatewayError("protocol", "Desktop snapshot contains malformed protocol v1 collections.");
  }
  return snapshot;
}

function validateActionResponse(value: unknown, expectedRequestId: string): DesktopActionResponse {
  if (
    !isRecord(value)
    || value.request_id !== expectedRequestId
    || typeof value.accepted !== "boolean"
    || !isUtcTimestamp(value.occurred_at)
    || !isProtocolNumber(value.revision)
    || typeof value.message !== "string"
    || (value.artifact_path !== null && typeof value.artifact_path !== "string")
  ) {
    throw new CoreGatewayError("protocol", "Core returned an invalid desktop action response.");
  }
  return value as unknown as DesktopActionResponse;
}

function toError(value: unknown, fallback: string): Error {
  return value instanceof Error ? value : new Error(fallback);
}

function requestId(): string {
  return typeof crypto.randomUUID === "function" ? crypto.randomUUID() : `desktop-${Date.now().toString(36)}`;
}

export interface HttpGatewayOptions {
  config: RuntimeConfig;
  fetchImplementation?: typeof fetch;
  socketFactory?: (url: string, protocols: string[]) => WebSocketLike;
  requestTimeoutMs?: number;
}

export class AuthenticatedHttpGateway implements CoreGateway {
  readonly mode = "live" as const;
  readonly endpoint: string;
  private readonly origin: URL;
  private readonly authToken: string;
  private readonly fetchImplementation: typeof fetch;
  private readonly socketFactory: (url: string, protocols: string[]) => WebSocketLike;
  private readonly requestTimeoutMs: number;

  constructor(options: HttpGatewayOptions) {
    this.origin = validateLocalCoreUrl(options.config.api_base_url);
    if (!options.config.auth_token.trim()) {
      throw new CoreGatewayError("configuration", "The native shell did not provide a Veqri Core authentication token.");
    }
    this.endpoint = this.origin.origin;
    this.authToken = options.config.auth_token;
    this.fetchImplementation = options.fetchImplementation ?? fetch.bind(globalThis);
    this.socketFactory = options.socketFactory ?? ((url, protocols) => new WebSocket(url, protocols));
    this.requestTimeoutMs = options.requestTimeoutMs ?? 10_000;
  }

  async loadSnapshot(signal?: AbortSignal): Promise<DesktopSnapshot> {
    return validateSnapshot(await this.requestJson("/api/v1/desktop/snapshot", { method: "GET" }, signal));
  }

  async performAction(action: DesktopAction, signal?: AbortSignal): Promise<DesktopActionResponse> {
    const request: DesktopActionRequest = { request_id: requestId(), action };
    return validateActionResponse(await this.requestJson(
      "/api/v1/desktop/actions",
      { method: "POST", body: JSON.stringify(request) },
      signal,
    ), request.request_id);
  }

  connectEvents(listeners: StreamListeners): () => void {
    const streamUrl = new URL("/api/v1/events", this.origin);
    streamUrl.protocol = this.origin.protocol === "https:" ? "wss:" : "ws:";
    const stream = new ReconnectingEventStream({
      createSocket: this.socketFactory,
      url: streamUrl.toString(),
      protocols: websocketProtocols(this.authToken),
      listeners,
    });
    stream.start();
    return () => stream.stop();
  }

  private async requestJson(path: string, init: RequestInit, upstreamSignal?: AbortSignal): Promise<unknown> {
    const timeoutController = new AbortController();
    const timeout = setTimeout(() => timeoutController.abort("timeout"), this.requestTimeoutMs);
    const signal = upstreamSignal ? AbortSignal.any([upstreamSignal, timeoutController.signal]) : timeoutController.signal;
    try {
      const response = await this.fetchImplementation(new URL(path, this.origin), {
        ...init,
        credentials: "omit",
        cache: "no-store",
        redirect: "error",
        headers: {
          Accept: "application/json",
          "Content-Type": "application/json",
          Authorization: `Bearer ${this.authToken}`,
          "X-Veqri-Client": "desktop",
          "X-Veqri-Protocol-Version": String(DESKTOP_PROTOCOL_VERSION),
          ...init.headers,
        },
        signal,
      });
      if (response.status === 401 || response.status === 403) {
        throw new CoreGatewayError("authentication", "Veqri Core rejected the desktop authentication token.", response.status);
      }
      if (!response.ok) {
        let message = `Veqri Core returned HTTP ${response.status}.`;
        try {
          const problem = (await response.json()) as { message?: unknown };
          if (typeof problem.message === "string" && problem.message.trim()) message = problem.message;
        } catch {
          // Keep the sanitized status message. Never surface raw response bodies.
        }
        throw new CoreGatewayError("server", message, response.status);
      }
      try {
        return await response.json();
      } catch {
        throw new CoreGatewayError("protocol", "Veqri Core returned invalid JSON.", response.status);
      }
    } catch (error) {
      if (error instanceof CoreGatewayError) throw error;
      if (signal.aborted) {
        const cancelledByCaller = upstreamSignal?.aborted === true;
        throw new CoreGatewayError(cancelledByCaller ? "cancelled" : "disconnected", cancelledByCaller ? "Request cancelled." : "Veqri Core did not respond in time.");
      }
      throw new CoreGatewayError("disconnected", "Cannot reach Veqri Core on localhost.");
    } finally {
      clearTimeout(timeout);
    }
  }
}

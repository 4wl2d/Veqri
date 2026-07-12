const absoluteDateFormatter = new Intl.DateTimeFormat(undefined, {
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
});

const timeFormatter = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });

export function formatDate(value: string | null): string {
  if (!value) return "Never";
  return absoluteDateFormatter.format(new Date(value));
}

export function formatTime(value: string | null): string {
  if (!value) return "—";
  return timeFormatter.format(new Date(value));
}

export function formatDuration(totalSeconds: number): string {
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = Math.floor(totalSeconds % 60);
  return hours > 0
    ? `${hours}:${minutes.toString().padStart(2, "0")}:${seconds.toString().padStart(2, "0")}`
    : `${minutes}:${seconds.toString().padStart(2, "0")}`;
}

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`;
}

export function sentenceCase(value: string): string {
  const normalized = value.replaceAll("_", " ").toLowerCase();
  return normalized.charAt(0).toUpperCase() + normalized.slice(1);
}

export function statusTone(value: string): "positive" | "warning" | "danger" | "neutral" | "accent" {
  const positive = new Set(["healthy", "online", "connected", "COMPLETED", "approved", "allowed", "ALLOW"]);
  const warning = new Set(["degraded", "retrying", "WAITING_FOR_APPROVAL", "WAITING_FOR_CHILDREN", "PARTIALLY_COMPLETED", "approval_required", "REQUIRE_APPROVAL"]);
  const danger = new Set(["offline", "failed", "FAILED", "BLOCKED", "TIMED_OUT", "denied", "revoked", "DENY"]);
  const accent = new Set(["RUNNING", "LISTENING", "SPEAKING", "THINKING", "DELEGATING", "WAITING_FOR_RESULT", "connecting"]);
  if (positive.has(value)) return "positive";
  if (warning.has(value)) return "warning";
  if (danger.has(value)) return "danger";
  if (accent.has(value)) return "accent";
  return "neutral";
}

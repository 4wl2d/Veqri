import { AuthenticatedHttpGateway, CoreGatewayError, type CoreGateway } from "./gateway";
import { MockCoreGateway } from "./mockGateway";
import type { RuntimeConfig } from "./types";

export async function resolveRuntimeConfig(): Promise<RuntimeConfig> {
  const wailsBridge = window.go?.main?.Bridge;
  if (!window.veqriShell && wailsBridge) {
    window.veqriShell = {
      getRuntimeConfig: () => wailsBridge.GetRuntimeConfig(),
    };
  }
  if (window.veqriShell) return await window.veqriShell.getRuntimeConfig();

  const mode = import.meta.env.VITE_VEQRI_MODE ?? (import.meta.env.DEV ? "mock" : "live");
  return {
    mode,
    api_base_url: import.meta.env.VITE_VEQRI_CORE_URL ?? "http://127.0.0.1:7342",
    auth_token: mode === "mock" ? "" : (import.meta.env.VITE_VEQRI_DEV_TOKEN ?? ""),
  };
}

export async function createRuntimeGateway(): Promise<CoreGateway> {
  const config = await resolveRuntimeConfig();
  if (config.mode === "mock") return new MockCoreGateway();
  if (config.mode !== "live") throw new CoreGatewayError("configuration", `Unsupported desktop runtime mode: ${String(config.mode)}.`);
  return new AuthenticatedHttpGateway({ config });
}

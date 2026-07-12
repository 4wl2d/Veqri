/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_VEQRI_MODE?: "mock" | "live";
  readonly VITE_VEQRI_CORE_URL?: string;
  readonly VITE_VEQRI_DEV_TOKEN?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}

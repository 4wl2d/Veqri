import { readFile } from "node:fs/promises";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { DEFAULT_CORE_URL } from "../src/api/runtime.ts";

describe("desktop runtime defaults", () => {
  it("keeps the copyable environment example aligned with the live Core default", async () => {
    const environmentExample = await readFile(
      path.join(process.cwd(), ".env.example"),
      "utf8",
    );

    expect(DEFAULT_CORE_URL).toBe("http://127.0.0.1:7342");
    expect(environmentExample).toContain(
      `VITE_VEQRI_CORE_URL=${DEFAULT_CORE_URL}`,
    );
  });
});

import path from "node:path";
import { describe, expect, it } from "vitest";
import {
  LINUX_WEBKIT_ENV,
  WAILS_CLI_PACKAGE,
  createNativeBuildPlan,
  parseLinuxWebKitOverride,
} from "./build-native.mjs";

const desktopDirectory = path.resolve("/workspace/apps/desktop");

function planFor(platform, overrides = {}) {
  return createNativeBuildPlan({
    platform,
    desktopDirectory,
    nodeExecutable: "/tools/node",
    npmCliPath: "/tools/npm-cli.js",
    ...overrides,
  });
}

function argumentAfter(args, flag) {
  const index = args.indexOf(flag);
  expect(index).toBeGreaterThanOrEqual(0);
  return args[index + 1];
}

describe("createNativeBuildPlan", () => {
  it("creates a macOS production build with fixed generated paths", () => {
    const plan = planFor("darwin");

    expect(plan.frontendBuild).toEqual({
      command: "/tools/node",
      args: ["/tools/npm-cli.js", "run", "build"],
      cwd: desktopDirectory,
    });
    expect(plan.generatedAssets).toEqual({
      source: path.join(desktopDirectory, "dist"),
      destination: path.join(desktopDirectory, "native", "dist"),
    });
    expect(plan.outputDirectory).toBe(
      path.resolve(desktopDirectory, "..", "..", "build", "bin"),
    );
    expect(plan.outputFile).toBe(
      path.join(plan.outputDirectory, "Veqri.app"),
    );
    expect(plan.nativeArtifact).toEqual({
      source: path.join(
        desktopDirectory,
        "native",
        "build",
        "bin",
        "Veqri.app",
      ),
      destination: path.join(plan.outputDirectory, "Veqri.app"),
      recursive: true,
    });
    expect(plan.nativeBuild.args.slice(0, 3)).toEqual([
      "run",
      WAILS_CLI_PACKAGE,
      "build",
    ]);
    expect(argumentAfter(plan.nativeBuild.args, "-platform")).toBe("darwin");
    expect(argumentAfter(plan.nativeBuild.args, "-tags")).toBe(
      "production,desktop",
    );
    expect(plan.nativeBuild.args).not.toContain("-ldflags");
    expect(plan.nativeBuild.args).not.toContain("windowsgui");
  });

  it("adds the Windows executable suffix and GUI linker flag", () => {
    const plan = planFor("win32");

    expect(plan.outputFile).toBe(
      path.join(plan.outputDirectory, "veqri-desktop.exe"),
    );
    expect(plan.nativeArtifact.recursive).toBe(false);
    expect(argumentAfter(plan.nativeBuild.args, "-platform")).toBe("windows");
    expect(argumentAfter(plan.nativeBuild.args, "-o")).toBe(
      "veqri-desktop.exe",
    );
    expect(argumentAfter(plan.nativeBuild.args, "-tags")).toBe(
      "production,desktop",
    );
    expect(argumentAfter(plan.nativeBuild.args, "-ldflags")).toBe(
      "-H windowsgui",
    );
  });

  it("selects WebKitGTK 4.1 from pkg-config availability on Linux", () => {
    const plan = planFor("linux", { webKit2Gtk41Available: true });

    expect(plan.linuxWebKitVersion).toBe("4.1");
    expect(argumentAfter(plan.nativeBuild.args, "-tags")).toBe(
      "production,desktop,webkit2_41",
    );
    expect(plan.nativeBuild.args).not.toContain("-ldflags");
    expect(plan.nativeBuild.args).not.toContain("windowsgui");
  });

  it("uses the legacy Linux tags when WebKitGTK 4.1 is unavailable", () => {
    const plan = planFor("linux");

    expect(plan.linuxWebKitVersion).toBe("4.0");
    expect(argumentAfter(plan.nativeBuild.args, "-tags")).toBe(
      "production,desktop",
    );
  });

  it("honors allowlisted Linux overrides without accepting arbitrary tags", () => {
    const forceLegacy = planFor("linux", {
      linuxWebKitOverride: "4.0",
      webKit2Gtk41Available: true,
    });
    const forceCurrent = planFor("linux", {
      linuxWebKitOverride: "4.1",
      webKit2Gtk41Available: false,
    });

    expect(argumentAfter(forceLegacy.nativeBuild.args, "-tags")).toBe(
      "production,desktop",
    );
    expect(argumentAfter(forceCurrent.nativeBuild.args, "-tags")).toBe(
      "production,desktop,webkit2_41",
    );
    expect(() =>
      planFor("linux", { linuxWebKitOverride: "4.1,malicious" }),
    ).toThrow(`${LINUX_WEBKIT_ENV} must be one of: auto, 4.0, 4.1`);
  });

  it("rejects unsupported platforms and invalid inputs", () => {
    expect(() => planFor("freebsd")).toThrow(
      "Unsupported desktop platform: freebsd",
    );
    expect(() =>
      createNativeBuildPlan({
        platform: "linux",
        desktopDirectory,
        nodeExecutable: "/tools/node",
        npmCliPath: "/tools/npm-cli.js",
        webKit2Gtk41Available: "yes",
      }),
    ).toThrow("webKit2Gtk41Available must be a boolean");
    expect(() =>
      createNativeBuildPlan({
        platform: "linux",
        desktopDirectory: "apps/desktop",
        nodeExecutable: "/tools/node",
        npmCliPath: "/tools/npm-cli.js",
      }),
    ).toThrow("desktopDirectory must be an absolute path");
  });
});

describe("parseLinuxWebKitOverride", () => {
  it("defaults missing and empty values to auto", () => {
    expect(parseLinuxWebKitOverride(undefined)).toBe("auto");
    expect(parseLinuxWebKitOverride("")).toBe("auto");
  });

  it.each(["auto", "4.0", "4.1"])("accepts %s", (value) => {
    expect(parseLinuxWebKitOverride(value)).toBe(value);
  });
});

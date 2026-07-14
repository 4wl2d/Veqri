import path from "node:path";
import { describe, expect, it } from "vitest";
import {
  BUILDINFO_LDFLAGS_ENV,
  DESKTOP_BUILD_COMMIT_ENV,
  DESKTOP_BUILD_VERSION_ENV,
  DESKTOP_PRODUCT_VERSION_ENV,
  LINUX_WEBKIT_ENV,
  TEMPORARY_NATIVE_DIRECTORY_PREFIX,
  WAILS_CLI_PACKAGE,
  createNativeFrontendEnvironment,
  createNativeBuildPlan,
  createWailsBuildConfig,
  parseDesktopProductVersion,
  parseLinuxWebKitOverride,
  shouldCopyNativeSource,
} from "./build-native.mjs";

const desktopDirectory = path.resolve("/workspace/apps/desktop");
const temporaryNativeDirectory = path.join(
  desktopDirectory,
  `${TEMPORARY_NATIVE_DIRECTORY_PREFIX}test`,
);

function planFor(platform, overrides = {}) {
  return createNativeBuildPlan({
    platform,
    desktopDirectory,
    temporaryNativeDirectory,
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
  it("creates a macOS production build in an isolated native workspace", () => {
    const plan = planFor("darwin");

    expect(plan.frontendBuild).toEqual({
      command: "/tools/node",
      args: ["/tools/npm-cli.js", "run", "build"],
      cwd: desktopDirectory,
    });
    expect(plan.temporaryDirectory).toBe(temporaryNativeDirectory);
    expect(plan.nativeWorkspace).toEqual({
      source: path.join(desktopDirectory, "native"),
      destination: temporaryNativeDirectory,
    });
    expect(plan.generatedAssets).toEqual({
      source: path.join(desktopDirectory, "dist"),
      destination: path.join(temporaryNativeDirectory, "dist"),
    });
    expect(plan.wailsConfig).toEqual({
      source: path.join(desktopDirectory, "native", "wails.json"),
      destination: path.join(temporaryNativeDirectory, "wails.json"),
      metadata: {
        productVersion: undefined,
        buildVersion: undefined,
        buildCommit: undefined,
      },
    });
    expect(plan.outputDirectory).toBe(
      path.resolve(desktopDirectory, "..", "..", "build", "bin"),
    );
    expect(plan.outputFile).toBe(
      path.join(plan.outputDirectory, "Veqri.app"),
    );
    expect(plan.nativeArtifact).toEqual({
      source: path.join(
        temporaryNativeDirectory,
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
    expect(plan.nativeBuild.cwd).toBe(temporaryNativeDirectory);
    expect(argumentAfter(plan.nativeBuild.args, "-platform")).toBe("darwin");
    expect(argumentAfter(plan.nativeBuild.args, "-tags")).toBe(
      "production,desktop",
    );
    expect(plan.nativeBuild.args).not.toContain("-ldflags");
    expect(plan.nativeBuild.args).not.toContain("windowsgui");
  });

  it("propagates build metadata without shell parsing", () => {
    const ldflags =
      "-X=github.com/veqri/veqri/internal/buildinfo.version=1.2.3";
    const commit = "a".repeat(40);
    const plan = planFor("darwin", {
      buildInfoLdflags: ldflags,
      productVersion: "1.2.3",
      buildVersion: "1.2.3-rc.1",
      buildCommit: commit,
    });

    expect(argumentAfter(plan.nativeBuild.args, "-ldflags")).toBe(ldflags);
    expect(plan.wailsConfig.metadata).toEqual({
      productVersion: "1.2.3",
      buildVersion: "1.2.3-rc.1",
      buildCommit: commit,
    });
  });

  it("adds the Windows executable suffix and preserves both linker inputs", () => {
    const plan = planFor("win32", {
      buildInfoLdflags: "-X=example.invalid/buildinfo.version=1.2.3",
    });

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
      "-X=example.invalid/buildinfo.version=1.2.3 -H windowsgui",
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

  it("requires the throwaway workspace to be a generated direct child", () => {
    for (const invalid of [
      path.join(desktopDirectory, "native"),
      path.join(desktopDirectory, TEMPORARY_NATIVE_DIRECTORY_PREFIX),
      path.join(desktopDirectory, "nested", `${TEMPORARY_NATIVE_DIRECTORY_PREFIX}x`),
      `${TEMPORARY_NATIVE_DIRECTORY_PREFIX}relative`,
    ]) {
      expect(() =>
        planFor("darwin", { temporaryNativeDirectory: invalid }),
      ).toThrow(/temporaryNativeDirectory/u);
    }
  });

  it("rejects unsupported platforms and invalid inputs", () => {
    expect(() => planFor("freebsd")).toThrow(
      "Unsupported desktop platform: freebsd",
    );
    expect(() =>
      createNativeBuildPlan({
        platform: "linux",
        desktopDirectory,
        temporaryNativeDirectory,
        nodeExecutable: "/tools/node",
        npmCliPath: "/tools/npm-cli.js",
        webKit2Gtk41Available: "yes",
      }),
    ).toThrow("webKit2Gtk41Available must be a boolean");
    expect(() =>
      createNativeBuildPlan({
        platform: "linux",
        desktopDirectory: "apps/desktop",
        temporaryNativeDirectory,
        nodeExecutable: "/tools/node",
        npmCliPath: "/tools/npm-cli.js",
      }),
    ).toThrow("desktopDirectory must be an absolute path");
    expect(() => planFor("darwin", { buildInfoLdflags: "bad\nflag" })).toThrow(
      BUILDINFO_LDFLAGS_ENV,
    );
  });
});

describe("createWailsBuildConfig", () => {
  const source = {
    name: "Veqri",
    outputfilename: "veqri-desktop",
    info: {
      productName: "Veqri",
      productVersion: "0.1.0",
      comments: "Local-first personal AI orchestrator",
    },
  };

  it("uses the source platform version without mutating the source config", () => {
    const generated = createWailsBuildConfig(source);

    expect(generated).toEqual(source);
    expect(generated).not.toBe(source);
    expect(generated.info).not.toBe(source.info);
  });

  it("overrides numeric platform version and appends safe build identity", () => {
    const commit = "b".repeat(64);
    const generated = createWailsBuildConfig(source, {
      productVersion: "2.3.4",
      buildVersion: "2.3.4+native.7",
      buildCommit: commit,
    });

    expect(generated.info.productVersion).toBe("2.3.4");
    expect(generated.info.comments).toBe(
      `Local-first personal AI orchestrator; build version 2.3.4+native.7, commit ${commit}`,
    );
    expect(source.info.productVersion).toBe("0.1.0");
    expect(source.info.comments).toBe("Local-first personal AI orchestrator");
  });

  it.each(["1.2", "01.2.3", "1.2.3-dev", "65536.0.0"])(
    "rejects non-platform version %s",
    (productVersion) => {
      expect(() =>
        createWailsBuildConfig(source, { productVersion }),
      ).toThrow(DESKTOP_PRODUCT_VERSION_ENV);
    },
  );

  it("rejects unsafe comment metadata", () => {
    expect(() =>
      createWailsBuildConfig(source, { buildVersion: "1.2.3<script>" }),
    ).toThrow(DESKTOP_BUILD_VERSION_ENV);
    expect(() =>
      createWailsBuildConfig(source, { buildCommit: "bad commit" }),
    ).toThrow(DESKTOP_BUILD_COMMIT_ENV);
  });
});

describe("shouldCopyNativeSource", () => {
  const nativeDirectory = path.join(desktopDirectory, "native");

  it.each([
    ["main.go", true],
    ["go.mod", true],
    ["build/appicon.png", true],
    ["build/darwin/Info.plist", true],
    ["build/windows/wails.exe.manifest", true],
    ["build/windows/icon.ico", false],
    ["build/windows/info.json", false],
    ["build/bin/veqri-desktop", false],
    ["dist/index.html", false],
    ["native", false],
    ["native.exe", false],
  ])("filters %s", (relative, expected) => {
    expect(
      shouldCopyNativeSource(path.join(nativeDirectory, relative), nativeDirectory),
    ).toBe(expected);
  });

  it("rejects paths outside the native source", () => {
    expect(() =>
      shouldCopyNativeSource(path.join(desktopDirectory, "outside"), nativeDirectory),
    ).toThrow(/outside/u);
  });
});

describe("parseDesktopProductVersion", () => {
  it.each(["0.1.0", "1.2.3", "65535.65535.65535"])(
    "accepts %s",
    (value) => {
      expect(parseDesktopProductVersion(value)).toBe(value);
    },
  );

  it("treats missing and empty values as an omitted override", () => {
    expect(parseDesktopProductVersion(undefined)).toBeUndefined();
    expect(parseDesktopProductVersion("")).toBeUndefined();
  });
});

describe("createNativeFrontendEnvironment", () => {
  it("forces live native configuration and removes case-variant dev tokens", () => {
    expect(
      createNativeFrontendEnvironment({
        PATH: "/tools",
        VITE_VEQRI_MODE: "mock",
        vite_veqri_dev_token: "must-not-ship",
        VITE_VEQRI_CORE_URL: "https://wrong.invalid",
      }),
    ).toEqual({
      PATH: "/tools",
      VITE_VEQRI_MODE: "live",
      VITE_VEQRI_CORE_URL: "http://127.0.0.1:7342",
      VITE_VEQRI_DEV_TOKEN: "",
    });
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

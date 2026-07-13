import { spawn } from "node:child_process";
import { cp, mkdir, rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const LINUX_WEBKIT_ENV = "VEQRI_WEBKIT2GTK_VERSION";
export const WAILS_CLI_PACKAGE =
  "github.com/wailsapp/wails/v2/cmd/wails@v2.12.0";

const NATIVE_FRONTEND_ENVIRONMENT = {
  VITE_VEQRI_MODE: "live",
  VITE_VEQRI_CORE_URL: "http://127.0.0.1:7342",
  VITE_VEQRI_DEV_TOKEN: "",
};

const SUPPORTED_PLATFORMS = new Set(["darwin", "linux", "win32"]);
const SUPPORTED_WEBKIT_VERSIONS = new Set(["auto", "4.0", "4.1"]);
const SCRIPT_DIRECTORY = path.dirname(fileURLToPath(import.meta.url));
const DESKTOP_DIRECTORY = path.resolve(SCRIPT_DIRECTORY, "..");

function requireNonEmptyString(value, label) {
  if (typeof value !== "string" || value.length === 0) {
    throw new TypeError(`${label} must be a non-empty string`);
  }

  return value;
}

export function parseLinuxWebKitOverride(value) {
  const version = value === undefined || value === "" ? "auto" : value;

  if (!SUPPORTED_WEBKIT_VERSIONS.has(version)) {
    throw new Error(
      `${LINUX_WEBKIT_ENV} must be one of: auto, 4.0, 4.1`,
    );
  }

  return version;
}

// Vite gives existing process variables precedence over .env files. Pinning
// these values prevents a developer's .env.local token or mock mode from being
// compiled into a native application artifact.
export function createNativeFrontendEnvironment(environment) {
  const protectedNames = new Set(
    Object.keys(NATIVE_FRONTEND_ENVIRONMENT).map((name) => name.toUpperCase()),
  );
  const result = Object.fromEntries(
    Object.entries(environment).filter(
      ([name]) => !protectedNames.has(name.toUpperCase()),
    ),
  );
  return { ...result, ...NATIVE_FRONTEND_ENVIRONMENT };
}

/**
 * Creates a deterministic native-build plan without reading the filesystem,
 * environment variables, or process state.
 */
export function createNativeBuildPlan({
  platform,
  desktopDirectory,
  nodeExecutable,
  npmCliPath,
  linuxWebKitOverride,
  webKit2Gtk41Available = false,
}) {
  if (!SUPPORTED_PLATFORMS.has(platform)) {
    throw new Error(`Unsupported desktop platform: ${String(platform)}`);
  }

  requireNonEmptyString(desktopDirectory, "desktopDirectory");
  requireNonEmptyString(nodeExecutable, "nodeExecutable");
  requireNonEmptyString(npmCliPath, "npmCliPath");

  if (!path.isAbsolute(desktopDirectory)) {
    throw new Error("desktopDirectory must be an absolute path");
  }

  if (typeof webKit2Gtk41Available !== "boolean") {
    throw new TypeError("webKit2Gtk41Available must be a boolean");
  }

  const resolvedDesktopDirectory = path.resolve(desktopDirectory);
  const nativeDirectory = path.join(resolvedDesktopDirectory, "native");
  const frontendDistDirectory = path.join(resolvedDesktopDirectory, "dist");
  const generatedNativeDistDirectory = path.join(nativeDirectory, "dist");
  const outputDirectory = path.resolve(
    resolvedDesktopDirectory,
    "..",
    "..",
    "build",
    "bin",
  );
  const binaryFilename =
    platform === "win32" ? "veqri-desktop.exe" : "veqri-desktop";
  const artifactFilename =
    platform === "darwin" ? "Veqri.app" : binaryFilename;
  const wailsBuildDirectory = path.join(nativeDirectory, "build", "bin");
  const wailsArtifact = path.join(wailsBuildDirectory, artifactFilename);
  const outputFile = path.join(outputDirectory, artifactFilename);

  const tags = ["production", "desktop"];
  let linuxWebKitVersion = null;

  if (platform === "linux") {
    const override = parseLinuxWebKitOverride(linuxWebKitOverride);
    linuxWebKitVersion =
      override === "auto"
        ? webKit2Gtk41Available
          ? "4.1"
          : "4.0"
        : override;

    if (linuxWebKitVersion === "4.1") {
      tags.push("webkit2_41");
    }
  }

  const wailsPlatform = platform === "win32" ? "windows" : platform;
  const nativeBuildArgs = [
    "run",
    WAILS_CLI_PACKAGE,
    "build",
    "-s",
    "-skipbindings",
    "-skipembedcreate",
    "-nosyncgomod",
    "-m",
    "-trimpath",
    "-platform",
    wailsPlatform,
    "-tags",
    tags.join(","),
    "-o",
    binaryFilename,
  ];

  if (platform === "win32") {
    nativeBuildArgs.push("-ldflags", "-H windowsgui");
  }

  return {
    platform,
    linuxWebKitVersion,
    frontendBuild: {
      command: nodeExecutable,
      args: [npmCliPath, "run", "build"],
      cwd: resolvedDesktopDirectory,
    },
    generatedAssets: {
      source: frontendDistDirectory,
      destination: generatedNativeDistDirectory,
    },
    outputDirectory,
    outputFile,
    nativeArtifact: {
      source: wailsArtifact,
      destination: outputFile,
      recursive: platform === "darwin",
    },
    nativeBuild: {
      command: "go",
      args: nativeBuildArgs,
      cwd: nativeDirectory,
    },
  };
}

function waitForCommand(command, args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      ...options,
      shell: false,
    });

    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (code === 0) {
        resolve();
        return;
      }

      const result = signal === null ? `exit code ${code}` : `signal ${signal}`;
      reject(new Error(`${command} failed with ${result}`));
    });
  });
}

async function packageIsAvailable(packageName) {
  try {
    await waitForCommand("pkg-config", ["--exists", packageName], {
      stdio: "ignore",
    });
    return true;
  } catch (error) {
    if (
      error instanceof Error &&
      ("code" in error ? error.code === "ENOENT" : false)
    ) {
      return false;
    }

    if (error instanceof Error && error.message.startsWith("pkg-config failed")) {
      return false;
    }

    throw error;
  }
}

async function runCommand(step, environment = process.env) {
  await waitForCommand(step.command, step.args, {
    cwd: step.cwd,
    env: environment,
    stdio: "inherit",
  });
}

export async function runNativeBuild() {
  const npmCliPath = process.env.npm_execpath;
  if (!npmCliPath) {
    throw new Error(
      "npm_execpath is unavailable; run this build with `npm run native:build`",
    );
  }

  const linuxWebKitOverride =
    process.platform === "linux"
      ? parseLinuxWebKitOverride(process.env[LINUX_WEBKIT_ENV])
      : undefined;
  const webKit2Gtk41Available =
    process.platform === "linux" && linuxWebKitOverride === "auto"
      ? await packageIsAvailable("webkit2gtk-4.1")
      : false;

  const plan = createNativeBuildPlan({
    platform: process.platform,
    desktopDirectory: DESKTOP_DIRECTORY,
    nodeExecutable: process.env.npm_node_execpath ?? process.execPath,
    npmCliPath,
    linuxWebKitOverride,
    webKit2Gtk41Available,
  });

  await runCommand(
    plan.frontendBuild,
    createNativeFrontendEnvironment(process.env),
  );

  await rm(plan.generatedAssets.destination, { recursive: true, force: true });
  await cp(plan.generatedAssets.source, plan.generatedAssets.destination, {
    recursive: true,
  });

  await mkdir(plan.outputDirectory, { recursive: true });
  await runCommand(plan.nativeBuild);

  await rm(plan.nativeArtifact.destination, { recursive: true, force: true });
  await cp(plan.nativeArtifact.source, plan.nativeArtifact.destination, {
    recursive: plan.nativeArtifact.recursive,
  });

  return plan.outputFile;
}

const isMainModule =
  process.argv[1] !== undefined &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);

if (isMainModule) {
  runNativeBuild()
    .then((outputFile) => {
      console.log(`Native desktop binary: ${outputFile}`);
    })
    .catch((error) => {
      console.error(error instanceof Error ? error.message : error);
      process.exitCode = 1;
    });
}

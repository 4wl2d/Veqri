import { spawn } from "node:child_process";
import {
  cp,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const LINUX_WEBKIT_ENV = "VEQRI_WEBKIT2GTK_VERSION";
export const BUILDINFO_LDFLAGS_ENV = "VEQRI_BUILDINFO_LDFLAGS";
export const DESKTOP_PRODUCT_VERSION_ENV = "VEQRI_DESKTOP_PRODUCT_VERSION";
export const DESKTOP_BUILD_VERSION_ENV = "VEQRI_DESKTOP_BUILD_VERSION";
export const DESKTOP_BUILD_COMMIT_ENV = "VEQRI_DESKTOP_BUILD_COMMIT";
export const WAILS_CLI_PACKAGE =
  "github.com/wailsapp/wails/v2/cmd/wails@v2.12.0";
export const TEMPORARY_NATIVE_DIRECTORY_PREFIX = ".veqri-native-build-";

const NATIVE_FRONTEND_ENVIRONMENT = {
  VITE_VEQRI_MODE: "live",
  VITE_VEQRI_CORE_URL: "http://127.0.0.1:7342",
  VITE_VEQRI_DEV_TOKEN: "",
};

const SUPPORTED_PLATFORMS = new Set(["darwin", "linux", "win32"]);
const SUPPORTED_WEBKIT_VERSIONS = new Set(["auto", "4.0", "4.1"]);
const SAFE_BUILD_COMMENT_VALUE = /^[0-9A-Za-z.+_-]+$/;
const SCRIPT_DIRECTORY = path.dirname(fileURLToPath(import.meta.url));
const DESKTOP_DIRECTORY = path.resolve(SCRIPT_DIRECTORY, "..");

function requireNonEmptyString(value, label) {
  if (typeof value !== "string" || value.length === 0) {
    throw new TypeError(`${label} must be a non-empty string`);
  }

  return value;
}

function optionalSingleLineString(value, label) {
  if (value === undefined || value === "") return undefined;
  requireNonEmptyString(value, label);
  if (value.trim() !== value || /[\0\r\n]/u.test(value)) {
    throw new Error(`${label} must be one non-empty line without surrounding whitespace`);
  }
  return value;
}

function optionalBuildCommentValue(value, label) {
  const parsed = optionalSingleLineString(value, label);
  if (parsed !== undefined && !SAFE_BUILD_COMMENT_VALUE.test(parsed)) {
    throw new Error(`${label} contains characters that are unsafe in platform metadata`);
  }
  return parsed;
}

export function parseDesktopProductVersion(value, label = "productVersion") {
  if (value === undefined || value === "") return undefined;
  requireNonEmptyString(value, label);
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/u.test(value)) {
    throw new Error(`${label} must be a canonical numeric MAJOR.MINOR.PATCH`);
  }
  if (value.split(".").some((component) => Number(component) > 65535)) {
    throw new Error(`${label} components must fit unsigned 16-bit platform fields`);
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

export function createWailsBuildConfig(
  sourceConfig,
  { productVersion, buildVersion, buildCommit } = {},
) {
  if (
    sourceConfig === null ||
    typeof sourceConfig !== "object" ||
    Array.isArray(sourceConfig)
  ) {
    throw new TypeError("sourceConfig must be a Wails configuration object");
  }
  if (
    sourceConfig.info === null ||
    typeof sourceConfig.info !== "object" ||
    Array.isArray(sourceConfig.info)
  ) {
    throw new TypeError("sourceConfig.info must be a Wails information object");
  }

  const sourceProductVersion = parseDesktopProductVersion(
    sourceConfig.info.productVersion,
    "sourceConfig.info.productVersion",
  );
  if (sourceProductVersion === undefined) {
    throw new Error("sourceConfig.info.productVersion is required");
  }
  const resolvedProductVersion =
    parseDesktopProductVersion(productVersion, DESKTOP_PRODUCT_VERSION_ENV) ??
    sourceProductVersion;
  const resolvedBuildVersion = optionalBuildCommentValue(
    buildVersion,
    DESKTOP_BUILD_VERSION_ENV,
  );
  const resolvedBuildCommit = optionalBuildCommentValue(
    buildCommit,
    DESKTOP_BUILD_COMMIT_ENV,
  );

  const buildDetails = [];
  if (resolvedBuildVersion !== undefined) {
    buildDetails.push(`build version ${resolvedBuildVersion}`);
  }
  if (resolvedBuildCommit !== undefined) {
    buildDetails.push(`commit ${resolvedBuildCommit}`);
  }

  let comments = sourceConfig.info.comments;
  if (buildDetails.length > 0) {
    const sourceComments =
      typeof comments === "string" && comments.length > 0 ? [comments] : [];
    comments = [...sourceComments, buildDetails.join(", ")].join("; ");
  }

  return {
    ...sourceConfig,
    info: {
      ...sourceConfig.info,
      productVersion: resolvedProductVersion,
      comments,
    },
  };
}

function resolveTemporaryNativeDirectory(value, desktopDirectory) {
  requireNonEmptyString(value, "temporaryNativeDirectory");
  if (!path.isAbsolute(value)) {
    throw new Error("temporaryNativeDirectory must be an absolute path");
  }

  const resolved = path.resolve(value);
  if (
    path.dirname(resolved) !== desktopDirectory ||
    !path.basename(resolved).startsWith(TEMPORARY_NATIVE_DIRECTORY_PREFIX) ||
    path.basename(resolved) === TEMPORARY_NATIVE_DIRECTORY_PREFIX
  ) {
    throw new Error(
      `temporaryNativeDirectory must be a generated direct child of ${desktopDirectory}`,
    );
  }
  return resolved;
}

/**
 * Creates a deterministic native-build plan without reading the filesystem,
 * environment variables, or process state.
 */
export function createNativeBuildPlan({
  platform,
  desktopDirectory,
  temporaryNativeDirectory,
  nodeExecutable,
  npmCliPath,
  linuxWebKitOverride,
  webKit2Gtk41Available = false,
  buildInfoLdflags,
  productVersion,
  buildVersion,
  buildCommit,
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
  const nativeSourceDirectory = path.join(resolvedDesktopDirectory, "native");
  const nativeDirectory = resolveTemporaryNativeDirectory(
    temporaryNativeDirectory,
    resolvedDesktopDirectory,
  );
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

  const resolvedLdflags = optionalSingleLineString(
    buildInfoLdflags,
    BUILDINFO_LDFLAGS_ENV,
  );
  const resolvedProductVersion = parseDesktopProductVersion(
    productVersion,
    DESKTOP_PRODUCT_VERSION_ENV,
  );
  const resolvedBuildVersion = optionalBuildCommentValue(
    buildVersion,
    DESKTOP_BUILD_VERSION_ENV,
  );
  const resolvedBuildCommit = optionalBuildCommentValue(
    buildCommit,
    DESKTOP_BUILD_COMMIT_ENV,
  );

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

  const linkerFlags = [];
  if (resolvedLdflags !== undefined) linkerFlags.push(resolvedLdflags);
  if (platform === "win32") linkerFlags.push("-H windowsgui");
  if (linkerFlags.length > 0) {
    nativeBuildArgs.push("-ldflags", linkerFlags.join(" "));
  }

  return {
    platform,
    linuxWebKitVersion,
    temporaryDirectory: nativeDirectory,
    frontendBuild: {
      command: nodeExecutable,
      args: [npmCliPath, "run", "build"],
      cwd: resolvedDesktopDirectory,
    },
    nativeWorkspace: {
      source: nativeSourceDirectory,
      destination: nativeDirectory,
    },
    wailsConfig: {
      source: path.join(nativeSourceDirectory, "wails.json"),
      destination: path.join(nativeDirectory, "wails.json"),
      metadata: {
        productVersion: resolvedProductVersion,
        buildVersion: resolvedBuildVersion,
        buildCommit: resolvedBuildCommit,
      },
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

export function shouldCopyNativeSource(sourcePath, nativeSourceDirectory) {
  const relative = path.relative(nativeSourceDirectory, sourcePath);
  if (relative === "") return true;
  if (relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) {
    throw new Error(`${sourcePath} is outside ${nativeSourceDirectory}`);
  }

  const normalized = relative.split(path.sep).join("/");
  return !(
    normalized === "dist" ||
    normalized.startsWith("dist/") ||
    normalized === "native" ||
    normalized === "native.exe" ||
    normalized === "build/bin" ||
    normalized.startsWith("build/bin/") ||
    normalized === "build/windows/icon.ico" ||
    normalized === "build/windows/info.json"
  );
}

async function copyNativeWorkspace(workspace) {
  const entries = await readdir(workspace.source, { withFileTypes: true });
  for (const entry of entries) {
    const source = path.join(workspace.source, entry.name);
    if (!shouldCopyNativeSource(source, workspace.source)) continue;
    await cp(source, path.join(workspace.destination, entry.name), {
      recursive: true,
      filter: (candidate) =>
        shouldCopyNativeSource(candidate, workspace.source),
    });
  }
}

async function materializeWailsConfig(configPlan) {
  let sourceConfig;
  try {
    sourceConfig = JSON.parse(await readFile(configPlan.source, "utf8"));
  } catch (error) {
    throw new Error(`read Wails configuration: ${error.message}`, {
      cause: error,
    });
  }
  const generatedConfig = createWailsBuildConfig(
    sourceConfig,
    configPlan.metadata,
  );
  await writeFile(
    configPlan.destination,
    `${JSON.stringify(generatedConfig, null, 2)}\n`,
    { encoding: "utf8", mode: 0o644 },
  );
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

  let temporaryNativeDirectory;
  try {
    temporaryNativeDirectory = await mkdtemp(
      path.join(DESKTOP_DIRECTORY, TEMPORARY_NATIVE_DIRECTORY_PREFIX),
    );
    const plan = createNativeBuildPlan({
      platform: process.platform,
      desktopDirectory: DESKTOP_DIRECTORY,
      temporaryNativeDirectory,
      nodeExecutable: process.env.npm_node_execpath ?? process.execPath,
      npmCliPath,
      linuxWebKitOverride,
      webKit2Gtk41Available,
      buildInfoLdflags: process.env[BUILDINFO_LDFLAGS_ENV],
      productVersion: process.env[DESKTOP_PRODUCT_VERSION_ENV],
      buildVersion: process.env[DESKTOP_BUILD_VERSION_ENV],
      buildCommit: process.env[DESKTOP_BUILD_COMMIT_ENV],
    });

    await runCommand(
      plan.frontendBuild,
      createNativeFrontendEnvironment(process.env),
    );

    await copyNativeWorkspace(plan.nativeWorkspace);
    await materializeWailsConfig(plan.wailsConfig);
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
  } finally {
    if (temporaryNativeDirectory !== undefined) {
      await rm(temporaryNativeDirectory, { recursive: true, force: true });
    }
  }
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

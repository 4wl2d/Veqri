package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/veqri/veqri/internal/buildinfo"
)

func TestParseOptionsDefaultsToDesktop(t *testing.T) {
	t.Parallel()

	opts, err := parseOptionsWithEnvironment(nil, nil)
	if err != nil {
		t.Fatalf("parseOptions() returned error: %v", err)
	}
	if opts.target != "desktop" || opts.root != "" || opts.skipNPMCI || opts.release || opts.strip || !opts.info.IsDevelopment() {
		t.Fatalf("parseOptions() = %+v", opts)
	}
}

func TestParseOptionsAcceptsAllTargetAndFlags(t *testing.T) {
	t.Parallel()

	opts, err := parseOptionsWithEnvironment([]string{"--root", "/repo", "--skip-npm-ci", "--strip", "all"}, nil)
	if err != nil {
		t.Fatalf("parseOptions() returned error: %v", err)
	}
	if opts.target != "all" || opts.root != "/repo" || !opts.skipNPMCI || !opts.strip {
		t.Fatalf("parseOptions() = %+v", opts)
	}
}

func TestAllTargetIncludesEveryBuildComponent(t *testing.T) {
	t.Parallel()

	for _, component := range []string{"binaries", "desktop", "android"} {
		if !targetIncludes("all", component) {
			t.Errorf("all target does not include %q", component)
		}
	}
	if targetIncludes("desktop", "binaries") {
		t.Fatal("desktop target unexpectedly includes standalone binaries")
	}
	if !targetHasCanonicalBuildIdentity("all") || targetHasCanonicalBuildIdentity("android") {
		t.Fatal("buildinfo manifest target selection does not match canonical P3 surfaces")
	}
}

func TestParseOptionsAcceptsValidatedReleaseMetadataFromEnvironment(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	opts, err := parseOptionsWithEnvironment([]string{"--release", "binaries"}, map[string]string{
		buildinfo.VersionEnvironment:   "1.2.3-rc.4",
		buildinfo.CommitEnvironment:    commit,
		buildinfo.BuildTimeEnvironment: "2026-07-14T12:30:45+02:00",
	})
	if err != nil {
		t.Fatalf("parseOptionsWithEnvironment() returned error: %v", err)
	}
	if !opts.release || opts.target != "binaries" || opts.info.Version != "1.2.3-rc.4" ||
		opts.info.Commit != commit || opts.info.BuildTime != "2026-07-14T10:30:45Z" {
		t.Fatalf("release options = %+v", opts)
	}
}

func TestParseOptionsMetadataFlagsOverrideEnvironment(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	opts, err := parseOptionsWithEnvironment([]string{
		"--release", "--version", "2.3.4", "--commit", commit,
		"--build-time", "2026-07-14T10:30:45Z", "binaries",
	}, map[string]string{
		buildinfo.VersionEnvironment:   "invalid",
		buildinfo.CommitEnvironment:    "invalid",
		buildinfo.BuildTimeEnvironment: "invalid",
	})
	if err != nil {
		t.Fatalf("metadata flags were not accepted: %v", err)
	}
	if opts.info != (buildinfo.Info{Version: "2.3.4", Commit: commit, BuildTime: "2026-07-14T10:30:45Z"}) {
		t.Fatalf("flag metadata = %+v", opts.info)
	}
}

func TestParseOptionsRejectsAndroidReleaseIdentityUntilP19(t *testing.T) {
	t.Parallel()

	environment := map[string]string{
		buildinfo.VersionEnvironment:   "1.2.3",
		buildinfo.CommitEnvironment:    "0123456789abcdef0123456789abcdef01234567",
		buildinfo.BuildTimeEnvironment: "2026-07-14T10:30:45Z",
	}
	for _, target := range []string{"android", "all"} {
		_, err := parseOptionsWithEnvironment([]string{"--release", target}, environment)
		if err == nil || !strings.Contains(err.Error(), "not supported for Android") {
			t.Errorf("release %s error = %v", target, err)
		}
	}
}

func TestParseOptionsRejectsImplicitOrIncompleteRelease(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	valid := map[string]string{
		buildinfo.VersionEnvironment:   "1.2.3",
		buildinfo.CommitEnvironment:    commit,
		buildinfo.BuildTimeEnvironment: "2026-07-14T10:30:45Z",
	}
	tests := []struct {
		name        string
		arguments   []string
		environment map[string]string
		want        string
	}{
		{name: "release requires switch", environment: valid, want: "explicit --release"},
		{name: "switch rejects development defaults", arguments: []string{"--release"}, want: "--release requires"},
		{name: "missing commit", arguments: []string{"--release"}, environment: map[string]string{
			buildinfo.VersionEnvironment: "1.2.3", buildinfo.BuildTimeEnvironment: "2026-07-14T10:30:45Z",
		}, want: "invalid build metadata"},
		{name: "invalid time", arguments: []string{"--release"}, environment: map[string]string{
			buildinfo.VersionEnvironment: "1.2.3", buildinfo.CommitEnvironment: commit,
			buildinfo.BuildTimeEnvironment: "14 July 2026",
		}, want: "invalid build metadata"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseOptionsWithEnvironment(test.arguments, test.environment)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parse error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestParseOptionsRejectsUnrepresentableDesktopProductVersion(t *testing.T) {
	t.Parallel()

	environment := map[string]string{
		buildinfo.VersionEnvironment:   "70000.1.2",
		buildinfo.CommitEnvironment:    "0123456789abcdef0123456789abcdef01234567",
		buildinfo.BuildTimeEnvironment: "2026-07-14T10:30:45Z",
	}
	if _, err := parseOptionsWithEnvironment([]string{"--release", "desktop"}, environment); err == nil ||
		!strings.Contains(err.Error(), "0 to 65535") {
		t.Fatalf("desktop product version error = %v", err)
	}
	if _, err := parseOptionsWithEnvironment([]string{"--release", "binaries"}, environment); err != nil {
		t.Fatalf("valid SemVer was unnecessarily rejected for standalone binaries: %v", err)
	}
}

func TestParseOptionsRejectsUnknownTarget(t *testing.T) {
	t.Parallel()

	if _, err := parseOptionsWithEnvironment([]string{"ios"}, nil); err == nil {
		t.Fatal("parseOptions() returned nil error for unknown target")
	}
}

func TestParseOptionsRejectsStripWithoutBinaries(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"desktop", "android"} {
		if _, err := parseOptionsWithEnvironment([]string{"--strip", target}, nil); err == nil || !strings.Contains(err.Error(), "applies only") {
			t.Errorf("--strip %s error = %v", target, err)
		}
	}
}

func TestBinaryBuildArgumentsCarryIdenticalValidatedMetadata(t *testing.T) {
	t.Parallel()

	info, err := buildinfo.Parse(
		"1.2.3", "0123456789abcdef0123456789abcdef01234567", "2026-07-14T10:30:45Z",
	)
	if err != nil {
		t.Fatal(err)
	}
	wantLDFlags, err := info.LDFlags()
	if err != nil {
		t.Fatal(err)
	}
	core, err := binaryBuildArguments("/out/veqri-core", "./cmd/veqri-core", info, false)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := binaryBuildArguments("/out/veqri", "./cmd/veqri-cli", info, true)
	if err != nil {
		t.Fatal(err)
	}
	if core[3] != wantLDFlags {
		t.Fatalf("Core ldflags = %q, want %q", core[3], wantLDFlags)
	}
	if cli[3] != wantLDFlags+" -s -w" {
		t.Fatalf("CLI stripped ldflags = %q", cli[3])
	}
}

func TestBuildEnvironmentFeedsDesktopFromOneInfo(t *testing.T) {
	t.Parallel()

	info, err := buildinfo.Parse(
		"1.2.3-rc.4", "0123456789abcdef0123456789abcdef01234567", "2026-07-14T10:30:45Z",
	)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := buildEnvironment(info)
	if err != nil {
		t.Fatal(err)
	}
	ldflags, _ := info.LDFlags()
	for name, want := range map[string]string{
		buildinfo.VersionEnvironment:    info.Version,
		buildinfo.CommitEnvironment:     info.Commit,
		buildinfo.BuildTimeEnvironment:  info.BuildTime,
		"VEQRI_BUILDINFO_LDFLAGS":       ldflags,
		"VEQRI_DESKTOP_PRODUCT_VERSION": "1.2.3",
		"VEQRI_DESKTOP_BUILD_VERSION":   info.Version,
		"VEQRI_DESKTOP_BUILD_COMMIT":    info.Commit,
	} {
		if got := environment[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestStageBuildInfoWritesPublicManifest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	info := buildinfo.Development()
	path, err := stageBuildInfo(root, info)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "build", "release", "buildinfo.json") {
		t.Fatalf("manifest path = %q", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded buildinfo.Info
	if err := json.Unmarshal(contents, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != info {
		t.Fatalf("manifest = %+v, want %+v", decoded, info)
	}
}

func TestDesktopArtifactPlans(t *testing.T) {
	t.Parallel()

	root := filepath.Clean("/repo")
	tests := []struct {
		goos        string
		source      string
		destination string
	}{
		{goos: "darwin", source: filepath.Join("build", "bin", "Veqri.app"), destination: filepath.Join("build", "release", "Veqri.app")},
		{goos: "windows", source: filepath.Join("build", "bin", "veqri-desktop.exe"), destination: filepath.Join("build", "release", "veqri-desktop.exe")},
		{goos: "linux", source: filepath.Join("build", "bin", "veqri-desktop"), destination: filepath.Join("build", "release", "veqri-desktop")},
	}
	for _, test := range tests {
		t.Run(test.goos, func(t *testing.T) {
			plan, err := desktopArtifactPlan(root, test.goos)
			if err != nil {
				t.Fatalf("desktopArtifactPlan() returned error: %v", err)
			}
			if plan.source != filepath.Join(root, test.source) || plan.destination != filepath.Join(root, test.destination) {
				t.Fatalf("desktopArtifactPlan() = %+v", plan)
			}
		})
	}
}

func TestFindAndroidSDKUsesEnvironmentBeforeStandardLocation(t *testing.T) {
	root := t.TempDir()
	sdk := filepath.Join(root, "custom-sdk")
	if err := os.MkdirAll(filepath.Join(sdk, "platforms", "android-37"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findAndroidSDK(runtime.GOOS, map[string]string{
		"ANDROID_HOME": sdk,
		"HOME":         filepath.Join(root, "home"),
		"LOCALAPPDATA": filepath.Join(root, "local"),
	})
	if err != nil {
		t.Fatalf("findAndroidSDK() returned error: %v", err)
	}
	want, _ := filepath.Abs(sdk)
	if got != want {
		t.Fatalf("findAndroidSDK() = %q, want %q", got, want)
	}
}

func TestVerifyLiveAndroidBuild(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "BuildConfig.java")
	contents := `public final class BuildConfig {
  public static final boolean USE_FAKE_TRANSPORT = false;
  public static final String DEFAULT_CORE_URL = "http://10.0.2.2:7342";
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyLiveAndroidBuild(path); err != nil {
		t.Fatalf("verifyLiveAndroidBuild() returned error: %v", err)
	}
}

func TestCopyPathPreservesExecutableFile(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	destination := filepath.Join(directory, "nested", "destination")
	if err := os.WriteFile(source, []byte("veqri"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyPath(source, destination); err != nil {
		t.Fatalf("copyPath() returned error: %v", err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "veqri" {
		t.Fatalf("copied contents = %q", contents)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("copied mode = %s, want executable", info.Mode())
		}
	}
}

func TestAndroidEnvironmentReplacesSDKAndPath(t *testing.T) {
	t.Parallel()

	values := androidEnvironment([]string{
		"PATH=/usr/bin",
		"ANDROID_HOME=/old",
		"OTHER=value",
	}, "/sdk")
	joined := strings.Join(values, "\n")
	if strings.Contains(joined, "/old") {
		t.Fatalf("androidEnvironment() retained old SDK: %q", joined)
	}
	for _, want := range []string{"ANDROID_HOME=/sdk", "ANDROID_SDK_ROOT=/sdk", "OTHER=value"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("androidEnvironment() = %q, missing %q", joined, want)
		}
	}
}

func TestWindowsNPMCLIFindsNodeSiblingInstallation(t *testing.T) {
	root := t.TempDir()
	nodePath := filepath.Join(root, "node.exe")
	npmPath := filepath.Join(root, "npm.cmd")
	npmCLI := filepath.Join(root, "node_modules", "npm", "bin", "npm-cli.js")
	if err := os.MkdirAll(filepath.Dir(npmCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{nodePath, npmPath, npmCLI} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := windowsNPMCLI(nodePath, npmPath)
	if err != nil {
		t.Fatalf("windowsNPMCLI() returned error: %v", err)
	}
	if got != npmCLI {
		t.Fatalf("windowsNPMCLI() = %q, want %q", got, npmCLI)
	}
}

func TestWindowsBatchCommandLineSupportsShimLayoutFallback(t *testing.T) {
	t.Parallel()

	got, err := windowsBatchCommandLine(`C:\Users\dev\App Data\fnm\npm.cmd`, "run", "native:build")
	if err != nil {
		t.Fatalf("windowsBatchCommandLine() returned error: %v", err)
	}
	want := `"C:\Users\dev\App Data\fnm\npm.cmd" run native:build`
	if got != want {
		t.Fatalf("windowsBatchCommandLine() = %q, want %q", got, want)
	}
	if _, err := windowsBatchCommandLine(`C:\Users\%USERNAME%\npm.cmd`, "ci"); err == nil {
		t.Fatal("windowsBatchCommandLine() accepted an expandable percent sequence")
	}
}

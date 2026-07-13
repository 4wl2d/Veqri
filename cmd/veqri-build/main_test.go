package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseOptionsDefaultsToDesktop(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions(nil)
	if err != nil {
		t.Fatalf("parseOptions() returned error: %v", err)
	}
	if opts.target != "desktop" || opts.root != "" || opts.skipNPMCI {
		t.Fatalf("parseOptions() = %+v", opts)
	}
}

func TestParseOptionsAcceptsAllTargetAndFlags(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions([]string{"--root", "/repo", "--skip-npm-ci", "all"})
	if err != nil {
		t.Fatalf("parseOptions() returned error: %v", err)
	}
	if opts.target != "all" || opts.root != "/repo" || !opts.skipNPMCI {
		t.Fatalf("parseOptions() = %+v", opts)
	}
}

func TestParseOptionsRejectsUnknownTarget(t *testing.T) {
	t.Parallel()

	if _, err := parseOptions([]string{"ios"}); err == nil {
		t.Fatal("parseOptions() returned nil error for unknown target")
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

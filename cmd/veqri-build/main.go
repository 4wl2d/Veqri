// Command veqri-build creates Veqri standalone binaries and application
// artifacts for the current host.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/veqri/veqri/internal/buildinfo"
)

const usageText = `Usage: go run ./cmd/veqri-build [flags] [binaries|desktop|android|all]

Targets:
  binaries Build standalone Core and CLI binaries for the current OS.
  desktop  Build one self-contained desktop app for the current OS (default).
  android  Build a real-Core debug APK with the emulator URL preconfigured.
  all      Build standalone binaries, desktop, and Android artifacts (development only).

Flags:
  --root PATH          Repository root (normally detected automatically).
  --skip-npm-ci        Reuse the existing desktop node_modules directory.
  --release            Require complete release metadata instead of development defaults.
  --version VERSION    Build version (or VEQRI_VERSION).
  --commit SHA         Full source commit (or VEQRI_COMMIT).
  --build-time TIME    RFC3339 build time (or VEQRI_BUILD_TIME).
  --strip              Strip Go symbol/debug tables from standalone binaries.
`

type options struct {
	root      string
	target    string
	skipNPMCI bool
	release   bool
	strip     bool
	info      buildinfo.Info
}

type artifactPlan struct {
	source      string
	destination string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "veqri-build:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	opts, err := parseOptions(arguments)
	if err != nil {
		return err
	}
	restoreEnvironment, err := applyBuildEnvironment(opts.info)
	if err != nil {
		return err
	}
	defer restoreEnvironment()
	if opts.root == "" {
		opts.root, err = findRepositoryRoot()
		if err != nil {
			return err
		}
	}
	opts.root, err = filepath.Abs(opts.root)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}
	if err := validateRepositoryRoot(opts.root); err != nil {
		return err
	}

	var artifacts []string
	if targetIncludes(opts.target, "binaries") {
		built, err := buildBinaries(opts)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, built...)
	}
	if targetIncludes(opts.target, "desktop") {
		artifact, err := buildDesktop(opts)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, artifact)
	}
	if targetIncludes(opts.target, "android") {
		artifact, err := buildAndroid(opts)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, artifact)
	}
	if targetHasCanonicalBuildIdentity(opts.target) {
		manifest, err := stageBuildInfo(opts.root, opts.info)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, manifest)
	}

	fmt.Println("\nVeqri artifacts:")
	for _, artifact := range artifacts {
		fmt.Println("  " + artifact)
	}
	return nil
}

func targetIncludes(target, component string) bool {
	return target == component || target == "all"
}

func targetHasCanonicalBuildIdentity(target string) bool {
	return targetIncludes(target, "binaries") || targetIncludes(target, "desktop")
}

func parseOptions(arguments []string) (options, error) {
	return parseOptionsWithEnvironment(arguments, environmentMap(os.Environ()))
}

func parseOptionsWithEnvironment(arguments []string, environment map[string]string) (options, error) {
	development := buildinfo.Development()
	flags := flag.NewFlagSet("veqri-build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "repository root")
	skipNPMCI := flags.Bool("skip-npm-ci", false, "reuse node_modules")
	release := flags.Bool("release", false, "require release metadata")
	strip := flags.Bool("strip", false, "strip Go symbol and debug tables")
	version := flags.String("version", environmentOrDefault(environment, buildinfo.VersionEnvironment, development.Version), "build version")
	commit := flags.String("commit", environmentOrDefault(environment, buildinfo.CommitEnvironment, development.Commit), "source commit")
	buildTime := flags.String("build-time", environmentOrDefault(environment, buildinfo.BuildTimeEnvironment, development.BuildTime), "RFC3339 build time")
	if err := flags.Parse(arguments); err != nil {
		return options{}, fmt.Errorf("%w\n\n%s", err, usageText)
	}
	remaining := flags.Args()
	if len(remaining) > 1 {
		return options{}, fmt.Errorf("expected one target\n\n%s", usageText)
	}
	target := "desktop"
	if len(remaining) == 1 {
		target = remaining[0]
	}
	switch target {
	case "binaries", "desktop", "android", "all":
	default:
		return options{}, fmt.Errorf("unknown target %q\n\n%s", target, usageText)
	}
	info, err := buildinfo.Parse(*version, *commit, *buildTime)
	if err != nil {
		return options{}, fmt.Errorf("invalid build metadata: %w", err)
	}
	if err := info.Validate(); err != nil {
		return options{}, fmt.Errorf("invalid build metadata: %w", err)
	}
	if *release && info.IsDevelopment() {
		return options{}, errors.New("--release requires a SemVer version, full commit, and RFC3339 build time")
	}
	if !*release && !info.IsDevelopment() {
		return options{}, errors.New("non-development build metadata requires explicit --release")
	}
	if *strip && !targetIncludes(target, "binaries") {
		return options{}, errors.New("--strip applies only to binaries or all")
	}
	if *release && targetIncludes(target, "android") {
		return options{}, errors.New("--release is not supported for Android until the Android release identity pipeline is implemented; build binaries or desktop explicitly")
	}
	if targetIncludes(target, "desktop") {
		if err := validateDesktopProductVersion(info.PlatformVersion()); err != nil {
			return options{}, err
		}
	}
	return options{
		root: *root, target: target, skipNPMCI: *skipNPMCI,
		release: *release, strip: *strip, info: info,
	}, nil
}

func validateDesktopProductVersion(value string) error {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return fmt.Errorf("desktop product version %q must contain three numeric components", value)
	}
	for _, part := range parts {
		component, err := strconv.ParseUint(part, 10, 16)
		if err != nil || strconv.FormatUint(component, 10) != part {
			return fmt.Errorf("desktop product version component %q must be a canonical integer from 0 to 65535", part)
		}
	}
	return nil
}

func environmentOrDefault(environment map[string]string, name, fallback string) string {
	if value, exists := environment[strings.ToUpper(name)]; exists {
		return value
	}
	return fallback
}

func findRepositoryRoot() (string, error) {
	start, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	if root, ok := walkToRepositoryRoot(start); ok {
		return root, nil
	}
	if executable, executableErr := os.Executable(); executableErr == nil {
		if root, ok := walkToRepositoryRoot(filepath.Dir(executable)); ok {
			return root, nil
		}
	}
	return "", errors.New("could not find the Veqri repository root; use --root")
}

func walkToRepositoryRoot(start string) (string, bool) {
	directory := filepath.Clean(start)
	for {
		if validateRepositoryRoot(directory) == nil {
			return directory, true
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", false
		}
		directory = parent
	}
}

func validateRepositoryRoot(root string) error {
	for _, path := range []string{
		filepath.Join(root, "go.mod"),
		filepath.Join(root, "apps", "desktop", "package.json"),
		filepath.Join(root, "apps", "android", "settings.gradle.kts"),
	} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return fmt.Errorf("%s is not a Veqri repository root", root)
		}
	}
	return nil
}

func buildBinaries(opts options) ([]string, error) {
	outputDirectory := filepath.Join(opts.root, "build", "bin")
	if err := os.MkdirAll(outputDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create binary output directory: %w", err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	targets := []struct {
		name        string
		packagePath string
	}{
		{name: "veqri-core" + suffix, packagePath: "./cmd/veqri-core"},
		{name: "veqri" + suffix, packagePath: "./cmd/veqri-cli"},
	}
	artifacts := make([]string, 0, len(targets))
	for _, target := range targets {
		output := filepath.Join(outputDirectory, target.name)
		arguments, err := binaryBuildArguments(output, target.packagePath, opts.info, opts.strip)
		if err != nil {
			return nil, err
		}
		if err := runCommand(opts.root, nil, "go", arguments...); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, output)
	}
	return artifacts, nil
}

func binaryBuildArguments(output, packagePath string, info buildinfo.Info, strip bool) ([]string, error) {
	ldflags, err := info.LDFlags()
	if err != nil {
		return nil, fmt.Errorf("prepare build metadata linker flags: %w", err)
	}
	if strip {
		ldflags += " -s -w"
	}
	return []string{"build", "-trimpath", "-ldflags", ldflags, "-o", output, packagePath}, nil
}

func stageBuildInfo(root string, info buildinfo.Info) (string, error) {
	directory := filepath.Join(root, "build", "release")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create release output directory: %w", err)
	}
	contents, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode build information: %w", err)
	}
	contents = append(contents, '\n')
	path := filepath.Join(directory, "buildinfo.json")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return "", fmt.Errorf("write build information: %w", err)
	}
	return path, nil
}

func buildEnvironment(info buildinfo.Info) (map[string]string, error) {
	ldflags, err := info.LDFlags()
	if err != nil {
		return nil, fmt.Errorf("prepare desktop build metadata: %w", err)
	}
	return map[string]string{
		buildinfo.VersionEnvironment:    info.Version,
		buildinfo.CommitEnvironment:     info.Commit,
		buildinfo.BuildTimeEnvironment:  info.BuildTime,
		"VEQRI_BUILDINFO_LDFLAGS":       ldflags,
		"VEQRI_DESKTOP_PRODUCT_VERSION": info.PlatformVersion(),
		"VEQRI_DESKTOP_BUILD_VERSION":   info.Version,
		"VEQRI_DESKTOP_BUILD_COMMIT":    info.Commit,
	}, nil
}

func applyBuildEnvironment(info buildinfo.Info) (func(), error) {
	values, err := buildEnvironment(info)
	if err != nil {
		return nil, err
	}
	type previousValue struct {
		value  string
		exists bool
	}
	previous := make(map[string]previousValue, len(values))
	for name, value := range values {
		old, exists := os.LookupEnv(name)
		previous[name] = previousValue{value: old, exists: exists}
		if err := os.Setenv(name, value); err != nil {
			for restoreName, restoreValue := range previous {
				if restoreValue.exists {
					_ = os.Setenv(restoreName, restoreValue.value)
				} else {
					_ = os.Unsetenv(restoreName)
				}
			}
			return nil, fmt.Errorf("set build environment %s: %w", name, err)
		}
	}
	return func() {
		for name, value := range previous {
			if value.exists {
				_ = os.Setenv(name, value.value)
			} else {
				_ = os.Unsetenv(name)
			}
		}
	}, nil
}

func buildDesktop(opts options) (string, error) {
	desktopDirectory := filepath.Join(opts.root, "apps", "desktop")
	if !opts.skipNPMCI {
		if err := runNPM(desktopDirectory, "ci"); err != nil {
			return "", err
		}
	}
	if err := runNPM(desktopDirectory, "run", "native:build"); err != nil {
		return "", err
	}
	plan, err := desktopArtifactPlan(opts.root, runtime.GOOS)
	if err != nil {
		return "", err
	}
	if err := replaceArtifact(plan.source, plan.destination); err != nil {
		return "", fmt.Errorf("stage desktop artifact: %w", err)
	}
	return plan.destination, nil
}

func runNPM(directory string, arguments ...string) error {
	if runtime.GOOS != "windows" {
		return runCommand(directory, nil, "npm", arguments...)
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return errors.New("required command \"node\" was not found in PATH")
	}
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		return errors.New("required command \"npm\" was not found in PATH")
	}
	extension := strings.ToLower(filepath.Ext(npmPath))
	if extension == ".exe" || extension == ".com" {
		return runCommand(directory, nil, npmPath, arguments...)
	}
	if npmCLI, cliErr := windowsNPMCLI(nodePath, npmPath); cliErr == nil {
		return runCommand(directory, nil, nodePath, append([]string{npmCLI}, arguments...)...)
	}
	return runWindowsNPMWrapper(directory, npmPath, arguments...)
}

func windowsNPMCLI(nodePath, npmPath string) (string, error) {
	candidates := []string{
		filepath.Join(filepath.Dir(nodePath), "node_modules", "npm", "bin", "npm-cli.js"),
		filepath.Join(filepath.Dir(npmPath), "node_modules", "npm", "bin", "npm-cli.js"),
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		canonical := strings.ToUpper(filepath.Clean(candidate))
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not locate npm-cli.js beside %s or %s", nodePath, npmPath)
}

func windowsBatchCommandLine(commandPath string, arguments ...string) (string, error) {
	if strings.ContainsAny(commandPath, "\"%\r\n") {
		return "", fmt.Errorf("Windows batch path contains unsupported command-shell characters: %q", commandPath)
	}
	for _, argument := range arguments {
		if argument == "" || strings.IndexFunc(argument, func(character rune) bool {
			return !(character >= 'a' && character <= 'z') &&
				!(character >= 'A' && character <= 'Z') &&
				!(character >= '0' && character <= '9') &&
				!strings.ContainsRune("-._:", character)
		}) >= 0 {
			return "", fmt.Errorf("unsupported Windows npm argument %q", argument)
		}
	}
	return `"` + commandPath + `" ` + strings.Join(arguments, " "), nil
}

func desktopArtifactPlan(root, goos string) (artifactPlan, error) {
	binDirectory := filepath.Join(root, "build", "bin")
	releaseDirectory := filepath.Join(root, "build", "release")
	switch goos {
	case "darwin":
		return artifactPlan{
			source:      filepath.Join(binDirectory, "Veqri.app"),
			destination: filepath.Join(releaseDirectory, "Veqri.app"),
		}, nil
	case "windows":
		return artifactPlan{
			source:      filepath.Join(binDirectory, "veqri-desktop.exe"),
			destination: filepath.Join(releaseDirectory, "veqri-desktop.exe"),
		}, nil
	case "linux":
		return artifactPlan{
			source:      filepath.Join(binDirectory, "veqri-desktop"),
			destination: filepath.Join(releaseDirectory, "veqri-desktop"),
		}, nil
	default:
		return artifactPlan{}, fmt.Errorf("desktop packaging is unsupported on %s", goos)
	}
}

func buildAndroid(opts options) (string, error) {
	sdk, err := findAndroidSDK(runtime.GOOS, environmentMap(os.Environ()))
	if err != nil {
		return "", err
	}
	androidDirectory := filepath.Join(opts.root, "apps", "android")
	wrapperJar := filepath.Join(androidDirectory, "gradle", "wrapper", "gradle-wrapper.jar")
	environment := androidEnvironment(os.Environ(), sdk)
	if err := runCommand(androidDirectory, environment, "java",
		"-classpath", wrapperJar, "org.gradle.wrapper.GradleWrapperMain",
		"--no-daemon", ":app:assembleDebug", "-PveqriFakeTransport=false"); err != nil {
		return "", err
	}
	buildConfig := filepath.Join(androidDirectory, "app", "build", "generated", "source", "buildConfig", "debug", "com", "veqri", "android", "BuildConfig.java")
	if err := verifyLiveAndroidBuild(buildConfig); err != nil {
		return "", err
	}
	plan := artifactPlan{
		source:      filepath.Join(androidDirectory, "app", "build", "outputs", "apk", "debug", "app-debug.apk"),
		destination: filepath.Join(opts.root, "build", "release", "Veqri-android-debug.apk"),
	}
	if err := replaceArtifact(plan.source, plan.destination); err != nil {
		return "", fmt.Errorf("stage Android artifact: %w", err)
	}
	return plan.destination, nil
}

func findAndroidSDK(goos string, environment map[string]string) (string, error) {
	var candidates []string
	for _, name := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if value := strings.TrimSpace(environment[name]); value != "" {
			candidates = append(candidates, value)
		}
	}
	home := environment["HOME"]
	switch goos {
	case "darwin":
		candidates = append(candidates, filepath.Join(home, "Library", "Android", "sdk"))
	case "windows":
		candidates = append(candidates, filepath.Join(environment["LOCALAPPDATA"], "Android", "Sdk"))
	case "linux":
		candidates = append(candidates, filepath.Join(home, "Android", "Sdk"))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		for _, platformName := range []string{"android-37", "android-37.0"} {
			platform := filepath.Join(candidate, "platforms", platformName)
			if info, err := os.Stat(platform); err == nil && info.IsDir() {
				absolute, absoluteErr := filepath.Abs(candidate)
				if absoluteErr != nil {
					return "", absoluteErr
				}
				return absolute, nil
			}
		}
	}
	return "", errors.New("Android SDK Platform 37/37.0 was not found; set ANDROID_HOME or install it in the standard SDK location")
}

func androidEnvironment(current []string, sdk string) []string {
	values := withoutEnvironment(current, "ANDROID_HOME", "ANDROID_SDK_ROOT", "PATH")
	pathValue := environmentMap(current)["PATH"]
	pathValue = filepath.Join(sdk, "platform-tools") + string(os.PathListSeparator) + pathValue
	return append(values,
		"ANDROID_HOME="+sdk,
		"ANDROID_SDK_ROOT="+sdk,
		"PATH="+pathValue,
	)
}

func verifyLiveAndroidBuild(path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read generated Android BuildConfig: %w", err)
	}
	text := string(contents)
	if !strings.Contains(text, "USE_FAKE_TRANSPORT = false") {
		return errors.New("Android build still uses fake transport")
	}
	if !strings.Contains(text, `DEFAULT_CORE_URL = "http://10.0.2.2:7342"`) {
		return errors.New("Android build does not contain the emulator Core URL")
	}
	return nil
}

func runCommand(directory string, environment []string, name string, arguments ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("required command %q was not found in PATH", name)
	}
	fmt.Printf("\n> %s %s\n", name, strings.Join(arguments, " "))
	command := exec.Command(path, arguments...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func replaceArtifact(source, destination string) error {
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("expected build output %s: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	return copyPath(source, destination)
}

func copyPath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(destination, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported artifact entry %s (%s)", source, info.Mode())
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func environmentMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		name, contents, found := strings.Cut(value, "=")
		if found {
			result[strings.ToUpper(name)] = contents
		}
	}
	return result
}

func withoutEnvironment(values []string, names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[strings.ToUpper(name)] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		name, _, found := strings.Cut(value, "=")
		if !found {
			continue
		}
		if _, remove := blocked[strings.ToUpper(name)]; !remove {
			result = append(result, value)
		}
	}
	return result
}

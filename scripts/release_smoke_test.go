package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/veqri/veqri/internal/buildinfo"
)

func TestDefaultBinaryPathUsesWindowsExecutableSuffix(t *testing.T) {
	if got := defaultBinaryPath("veqri", "windows"); got != filepath.Join("build", "bin", "veqri.exe") {
		t.Fatalf("defaultBinaryPath(windows) = %q", got)
	}
	if got := defaultBinaryPath("veqri", "linux"); got != filepath.Join("build", "bin", "veqri") {
		t.Fatalf("defaultBinaryPath(linux) = %q", got)
	}
}

func TestMergeEnvironmentOverridesWithoutDuplicates(t *testing.T) {
	baseName := "VEQRI_URL"
	if runtime.GOOS == "windows" {
		baseName = strings.ToLower(baseName)
	}
	merged := mergeEnvironment(
		[]string{"PATH=/bin", baseName + "=http://old.invalid"},
		map[string]string{"VEQRI_URL": "http://127.0.0.1:7342"},
	)
	count := 0
	for _, entry := range merged {
		if strings.EqualFold(strings.SplitN(entry, "=", 2)[0], "VEQRI_URL") {
			count++
			if !strings.HasSuffix(entry, "=http://127.0.0.1:7342") {
				t.Fatalf("VEQRI_URL entry = %q", entry)
			}
		}
	}
	if count != 1 {
		t.Fatalf("VEQRI_URL entries = %d, want 1: %v", count, merged)
	}
}

func TestBoundedTextTrimsAndBounds(t *testing.T) {
	if got := boundedText("  short  ", 10); got != "short" {
		t.Fatalf("boundedText(short) = %q", got)
	}
	if got := boundedText("123456", 4); got != "1234…" {
		t.Fatalf("boundedText(long) = %q", got)
	}
}

func TestBuildInfoFromPayloadAcceptsDevelopmentAndReleaseIdentity(t *testing.T) {
	t.Parallel()

	tests := []buildinfo.Info{
		buildinfo.Development(),
		{
			Version: "1.2.3-rc.4", Commit: "0123456789abcdef0123456789abcdef01234567",
			BuildTime: "2026-07-14T10:30:45Z",
		},
	}
	for _, want := range tests {
		payload := map[string]any{
			"status": "ok", "version": want.Version, "commit": want.Commit, "build_time": want.BuildTime,
		}
		got, err := buildInfoFromPayload("test payload", payload)
		if err != nil {
			t.Fatalf("buildInfoFromPayload(%+v): %v", want, err)
		}
		if got != want {
			t.Fatalf("buildInfoFromPayload() = %+v, want %+v", got, want)
		}
	}
}

func TestBuildInfoFromPayloadFailsClosedAndMismatchIsReported(t *testing.T) {
	t.Parallel()

	for _, payload := range []map[string]any{
		{"version": "0.1.0-dev", "commit": "unknown"},
		{"version": "1.2.3", "commit": "short", "build_time": "yesterday"},
		{"version": "1.2.3", "commit": "0123456789abcdef0123456789abcdef01234567", "build_time": "2026-07-14T12:30:45+02:00"},
		{"version": 123, "commit": "unknown", "build_time": "unknown"},
	} {
		if _, err := buildInfoFromPayload("test payload", payload); err == nil {
			t.Fatalf("invalid payload unexpectedly accepted: %+v", payload)
		}
	}
	want := buildinfo.Info{
		Version: "1.2.3", Commit: "0123456789abcdef0123456789abcdef01234567",
		BuildTime: "2026-07-14T10:30:45Z",
	}
	mismatches := []buildinfo.Info{want, want, want}
	mismatches[0].Version = "1.2.4"
	mismatches[1].Commit = "1123456789abcdef0123456789abcdef01234567"
	mismatches[2].BuildTime = "2026-07-14T10:30:46Z"
	for _, other := range mismatches {
		if err := requireMatchingBuildInfo(want, other, "CLI", "Core"); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("mismatch %+v error = %v", other, err)
		}
	}
}

package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

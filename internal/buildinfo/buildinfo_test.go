package buildinfo

import (
	"strings"
	"testing"
)

const (
	commit40 = "0123456789abcdef0123456789abcdef01234567"
	commit64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestDevelopmentAndCurrentDefaults(t *testing.T) {
	want := Info{Version: DevelopmentVersion, Commit: Unknown, BuildTime: Unknown}
	if got := Development(); got != want {
		t.Fatalf("Development() = %+v, want %+v", got, want)
	}
	got, err := Current()
	if err != nil {
		t.Fatalf("Current(): %v", err)
	}
	if got != want || !got.IsDevelopment() {
		t.Fatalf("Current() = %+v, want development identity", got)
	}
}

func TestParseReleaseNormalizesBuildTime(t *testing.T) {
	info, err := Parse("1.2.3-rc.1+build.7", commit64, "2026-07-14T14:34:56.987+02:00")
	if err != nil {
		t.Fatalf("Parse(): %v", err)
	}
	want := Info{
		Version: "1.2.3-rc.1+build.7", Commit: commit64,
		BuildTime: "2026-07-14T12:34:56Z",
	}
	if info != want {
		t.Fatalf("Parse() = %+v, want %+v", info, want)
	}
	if info.IsDevelopment() {
		t.Fatal("release identity reported development")
	}
	if err := info.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
}

func TestParseRejectsIncompleteOrInvalidReleaseMetadata(t *testing.T) {
	tests := map[string]Info{
		"development commit": {Version: DevelopmentVersion, Commit: commit40, BuildTime: Unknown},
		"empty version":      {Version: "", Commit: commit40, BuildTime: "2026-07-14T12:34:56Z"},
		"v prefix":           {Version: "v1.2.3", Commit: commit40, BuildTime: "2026-07-14T12:34:56Z"},
		"short version":      {Version: "1.2", Commit: commit40, BuildTime: "2026-07-14T12:34:56Z"},
		"leading zero":       {Version: "01.2.3", Commit: commit40, BuildTime: "2026-07-14T12:34:56Z"},
		"version whitespace": {Version: "1.2.3 ", Commit: commit40, BuildTime: "2026-07-14T12:34:56Z"},
		"invalid prerelease": {Version: "1.2.3-01", Commit: commit40, BuildTime: "2026-07-14T12:34:56Z"},
		"short commit":       {Version: "1.2.3", Commit: "0123456", BuildTime: "2026-07-14T12:34:56Z"},
		"uppercase commit":   {Version: "1.2.3", Commit: strings.ToUpper(commit40), BuildTime: "2026-07-14T12:34:56Z"},
		"unknown commit":     {Version: "1.2.3", Commit: Unknown, BuildTime: "2026-07-14T12:34:56Z"},
		"unknown build time": {Version: "1.2.3", Commit: commit40, BuildTime: Unknown},
		"time whitespace":    {Version: "1.2.3", Commit: commit40, BuildTime: " 2026-07-14T12:34:56Z"},
		"invalid time":       {Version: "1.2.3", Commit: commit40, BuildTime: "2026-07-14"},
	}
	for name, info := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(info.Version, info.Commit, info.BuildTime); err == nil {
				t.Fatalf("Parse(%+v) unexpectedly succeeded", info)
			}
			if err := info.Validate(); err == nil {
				t.Fatalf("Validate(%+v) unexpectedly succeeded", info)
			}
		})
	}
}

func TestCurrentRejectsInvalidLinkerMetadata(t *testing.T) {
	previousVersion, previousCommit, previousBuildTime := version, commit, buildTime
	t.Cleanup(func() { version, commit, buildTime = previousVersion, previousCommit, previousBuildTime })
	version, commit, buildTime = "1.2", commit40, "2026-07-14T12:34:56Z"
	if _, err := Current(); err == nil {
		t.Fatal("Current() accepted invalid linker metadata")
	}
}

func TestPlatformVersion(t *testing.T) {
	tests := map[string]string{
		DevelopmentVersion:       "0.1.0",
		"1.2.3":                  "1.2.3",
		"1.2.3-rc.1":             "1.2.3",
		"1.2.3-rc.1+build.7":     "1.2.3",
		"1.2":                    "",
		"not-a-semantic-version": "",
	}
	for versionValue, want := range tests {
		if got := (Info{Version: versionValue}).PlatformVersion(); got != want {
			t.Errorf("PlatformVersion(%q) = %q, want %q", versionValue, got, want)
		}
	}
}

func TestLDFlagsAreNormalizedAndComplete(t *testing.T) {
	info := Info{Version: "1.2.3", Commit: commit40, BuildTime: "2026-07-14T14:34:56.987+02:00"}
	flags, err := info.LDFlags()
	if err != nil {
		t.Fatalf("LDFlags(): %v", err)
	}
	want := "-X=" + packagePath + ".version=1.2.3 " +
		"-X=" + packagePath + ".commit=" + commit40 + " " +
		"-X=" + packagePath + ".buildTime=2026-07-14T12:34:56Z"
	if flags != want {
		t.Fatalf("LDFlags() = %q, want %q", flags, want)
	}
	if _, err := (Info{Version: "1.2.3", Commit: Unknown, BuildTime: Unknown}).LDFlags(); err == nil {
		t.Fatal("LDFlags() accepted incomplete release metadata")
	}
}

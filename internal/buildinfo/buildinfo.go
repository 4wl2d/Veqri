// Package buildinfo owns the canonical Veqri build identity shared by every
// Go entry point and release artifact.
package buildinfo

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	DevelopmentVersion = "0.1.0-dev"
	Unknown            = "unknown"

	VersionEnvironment   = "VEQRI_VERSION"
	CommitEnvironment    = "VEQRI_COMMIT"
	BuildTimeEnvironment = "VEQRI_BUILD_TIME"

	packagePath = "github.com/veqri/veqri/internal/buildinfo"
)

var (
	version   = DevelopmentVersion
	commit    = Unknown
	buildTime = Unknown
)

var releaseCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Info is the public, transport-safe identity of one Veqri build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// Development returns the identity used by ordinary local and pull-request
// builds when no release linker metadata is supplied.
func Development() Info {
	return Info{Version: DevelopmentVersion, Commit: Unknown, BuildTime: Unknown}
}

// Parse validates build metadata and normalizes a release build time to UTC
// with second precision. Development is valid only as the exact default
// identity; partial or mixed release metadata fails closed.
func Parse(versionValue, commitValue, buildTimeValue string) (Info, error) {
	info := Info{Version: versionValue, Commit: commitValue, BuildTime: buildTimeValue}
	if info.IsDevelopment() {
		return info, nil
	}
	if versionValue == DevelopmentVersion {
		return Info{}, errors.New("development version requires unknown commit and build time")
	}
	if !validBareSemVer(versionValue) {
		return Info{}, fmt.Errorf("version %q is not a strict bare SemVer", versionValue)
	}
	if !releaseCommitPattern.MatchString(commitValue) {
		return Info{}, errors.New("commit must be a full lowercase 40- or 64-character hexadecimal revision")
	}
	if strings.TrimSpace(buildTimeValue) != buildTimeValue {
		return Info{}, errors.New("build time cannot contain surrounding whitespace")
	}
	parsedBuildTime, err := time.Parse(time.RFC3339, buildTimeValue)
	if err != nil {
		return Info{}, fmt.Errorf("build time must be RFC3339: %w", err)
	}
	info.BuildTime = parsedBuildTime.UTC().Truncate(time.Second).Format(time.RFC3339)
	return info, nil
}

// Current returns the validated identity embedded by the Go linker.
func Current() (Info, error) {
	return Parse(version, commit, buildTime)
}

// Validate reports whether the identity is either the exact development
// identity or a complete, valid release identity.
func (i Info) Validate() error {
	_, err := Parse(i.Version, i.Commit, i.BuildTime)
	return err
}

// IsDevelopment reports whether all three fields match the development
// identity. A development version mixed with release metadata is not a
// development build and is rejected by Validate.
func (i Info) IsDevelopment() bool {
	return i == Development()
}

// PlatformVersion returns the numeric MAJOR.MINOR.PATCH portion suitable for
// platform metadata fields that cannot represent SemVer prerelease or build
// identifiers. Invalid versions return an empty string.
func (i Info) PlatformVersion() string {
	if !validBareSemVer(i.Version) {
		return ""
	}
	if index := strings.IndexAny(i.Version, "-+"); index >= 0 {
		return i.Version[:index]
	}
	return i.Version
}

// LDFlags returns one validated linker-flag value that can be passed as the
// argument to `go build -ldflags` or Wails' `-ldflags` option.
func (i Info) LDFlags() (string, error) {
	normalized, err := Parse(i.Version, i.Commit, i.BuildTime)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"-X=" + packagePath + ".version=" + normalized.Version,
		"-X=" + packagePath + ".commit=" + normalized.Commit,
		"-X=" + packagePath + ".buildTime=" + normalized.BuildTime,
	}, " "), nil
}

func validBareSemVer(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "v") {
		return false
	}
	core := value
	if index := strings.IndexAny(core, "-+"); index >= 0 {
		core = core[:index]
	}
	if len(strings.Split(core, ".")) != 3 {
		return false
	}
	return semver.IsValid("v" + value)
}

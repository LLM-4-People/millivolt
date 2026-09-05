// Package millivolt exposes the release identity shared by every build path.
package millivolt

import (
	_ "embed"
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
)

// VERSION is the only semantic-version source. Release versions omit build
// metadata: the source revision is reported separately, and '+' is not a valid
// container-tag character. Prerelease identifiers follow SemVer 2.0.0.
//
//go:embed VERSION
var versionText string

// revisionOverride transports verified source identity into builds whose
// context excludes .git. It never overrides VERSION or asserts a clean tree.
// Set with -X github.com/LLM-4-People/millivolt.revisionOverride=<full commit>.
var revisionOverride string

const (
	versionNumber = `(0|[1-9][0-9]*)`
	// Container distribution tags have a mechanical 128-character limit.
	maxReleaseTagLength = 128
)

var (
	versionPattern  = regexp.MustCompile(`^` + versionNumber + `\.` + versionNumber + `\.` + versionNumber + `(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	revisionPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// Version returns the embedded application version, without the file newline.
func Version() string { return strings.TrimSuffix(versionText, "\n") }

// ValidateVersion enforces the release naming policy, including numeric
// prerelease identifiers without leading zeroes. It accepts no whitespace,
// leading v, or build metadata suffix.
func ValidateVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("invalid release version %q: expected MAJOR.MINOR.PATCH with an optional SemVer prerelease", version)
	}
	if _, prerelease, found := strings.Cut(version, "-"); found {
		for _, identifier := range strings.Split(prerelease, ".") {
			if len(identifier) > 1 && identifier[0] == '0' && strings.Trim(identifier, "0123456789") == "" {
				return fmt.Errorf("invalid release version %q: numeric prerelease identifier has a leading zero", version)
			}
		}
	}
	return nil
}

// ValidateRevision accepts a full lowercase Git SHA-1 or SHA-256 object ID.
func ValidateRevision(revision string) error {
	if !revisionPattern.MatchString(revision) {
		return fmt.Errorf("invalid source revision: expected a full lowercase Git object ID")
	}
	return nil
}

// BuildInfo identifies the running binary. A nil Modified means the source
// tree's cleanliness is unknown, not clean. Revision is "unknown" when absent.
type BuildInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Modified  *bool  `json:"modified"`
	GoVersion string `json:"go_version"`
}

// CurrentBuild reads the standard Go build metadata. No subprocess, filesystem
// scan, clock stamp, configuration load, or network access is needed.
func CurrentBuild() (BuildInfo, error) {
	info, _ := debug.ReadBuildInfo()
	return buildInfo(Version(), revisionOverride, info)
}

func buildInfo(version, override string, info *debug.BuildInfo) (BuildInfo, error) {
	result := BuildInfo{Version: version, Revision: "unknown", GoVersion: runtime.Version()}
	if err := ValidateVersion(version); err != nil {
		return result, err
	}
	if info != nil {
		if info.GoVersion != "" {
			result.GoVersion = info.GoVersion
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if setting.Value != "" {
					result.Revision = setting.Value
				}
			case "vcs.modified":
				if setting.Value == "true" || setting.Value == "false" {
					modified := setting.Value == "true"
					result.Modified = &modified
				}
			}
		}
	}
	if override != "" {
		if err := ValidateRevision(override); err != nil {
			return result, err
		}
		if result.Revision != "unknown" && result.Revision != override {
			return result, fmt.Errorf("source revision override disagrees with Go build metadata")
		}
		result.Revision = override
	}
	return result, nil
}

// ReleaseInfo is validated metadata for the container/release workflow. Tag is
// optional for default-branch builds; when present it must match VERSION exactly.
type ReleaseInfo struct {
	Version    string
	Revision   string
	Prerelease bool
}

func ReleaseMetadata(tag, revision string) (ReleaseInfo, error) {
	return releaseMetadata(Version(), tag, revision)
}

func releaseMetadata(version, tag, revision string) (ReleaseInfo, error) {
	result := ReleaseInfo{Version: version, Revision: revision, Prerelease: strings.Contains(version, "-")}
	if err := ValidateVersion(version); err != nil {
		return result, err
	}
	if len(version) > maxReleaseTagLength {
		return result, fmt.Errorf("release version exceeds the container tag limit of %d characters", maxReleaseTagLength)
	}
	if err := ValidateRevision(revision); err != nil {
		return result, err
	}
	if tag != "" && tag != "v"+version {
		return result, fmt.Errorf("release tag %q does not match VERSION (%s)", tag, version)
	}
	return result, nil
}

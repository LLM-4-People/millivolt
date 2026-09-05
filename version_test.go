package millivolt

import (
	"os"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

func TestEmbeddedVersion(t *testing.T) {
	contents, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != Version()+"\n" || versionText != string(contents) {
		t.Fatal("VERSION must be one canonical version followed by one newline")
	}
	if err := ValidateVersion(Version()); err != nil {
		t.Fatal(err)
	}
}

func TestVersionValidation(t *testing.T) {
	for _, version := range []string{"0.1.0", "1.2.3", "999999999999999999999.0.0", "0.1.0-alpha.1", "1.2.3-0", "1.2.3-01a", "1.2.3-alpha-01"} {
		if err := ValidateVersion(version); err != nil {
			t.Errorf("valid %q: %v", version, err)
		}
	}
	for _, version := range []string{"", "v0.1.0", "0.1", "01.2.3", "1.02.3", "1.2.03", " 1.2.3", "1.2.3\n", "1.2.3\r", "1.2.3+build", "1.2.3-", "1.2.3-.a", "1.2.3-a.", "1.2.3-a..b", "1.2.3-01", "1.2.3-a.01", "1.2.3-a_b", "1.2.3-α"} {
		if err := ValidateVersion(version); err == nil {
			t.Errorf("accepted invalid version %q", version)
		}
	}
}

func TestRevisionValidation(t *testing.T) {
	for _, revision := range []string{strings.Repeat("a", 40), strings.Repeat("1f", 32)} {
		if err := ValidateRevision(revision); err != nil {
			t.Error(err)
		}
	}
	for _, revision := range []string{"", "unknown", "abc1234", strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("A", 40), strings.Repeat("g", 40), strings.Repeat("a", 40) + "\n"} {
		if err := ValidateRevision(revision); err == nil {
			t.Errorf("accepted invalid revision %q", revision)
		}
	}
}

func TestBuildMetadata(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, modified := range []string{"true", "false", "unavailable"} {
		source := &debug.BuildInfo{GoVersion: "go1.26.6", Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: commit}, {Key: "vcs.modified", Value: modified},
		}}
		for _, override := range []string{"", commit} {
			got, err := buildInfo("0.1.0", override, source)
			if err != nil || got.Version != "0.1.0" || got.Revision != commit || got.GoVersion != source.GoVersion {
				t.Fatalf("metadata: %+v, %v", got, err)
			}
			if modified == "unavailable" {
				if got.Modified != nil {
					t.Fatal("unknown source state became known")
				}
			} else if got.Modified == nil || *got.Modified != (modified == "true") {
				t.Fatalf("wrong modified state: %+v", got)
			}
		}
	}
	for _, source := range []*debug.BuildInfo{nil, {}} {
		for _, override := range []string{"", commit} {
			got, err := buildInfo("0.1.0", override, source)
			wantRevision := override
			if wantRevision == "" {
				wantRevision = "unknown"
			}
			if err != nil || got.Revision != wantRevision || got.Modified != nil || got.GoVersion == "" {
				t.Fatalf("missing metadata: %+v, %v", got, err)
			}
		}
	}
	if _, err := buildInfo("bad", "", nil); err == nil {
		t.Fatal("accepted invalid embedded version")
	}
	if _, err := buildInfo("0.1.0", "bad", nil); err == nil {
		t.Fatal("accepted invalid revision override")
	}
	if _, err := buildInfo("0.1.0", commit, &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: strings.Repeat("b", 40)}}}); err == nil {
		t.Fatal("accepted source revision contradicting Go metadata")
	}
	first, err := CurrentBuild()
	if err != nil {
		t.Fatal(err)
	}
	second, err := CurrentBuild()
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("build identity changes between reads: %+v / %+v, %v", first, second, err)
	}
}

func TestReleaseMetadata(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, version := range []string{"0.1.0", "0.1.0-alpha.1"} {
		for _, tag := range []string{"", "v" + version} {
			got, err := releaseMetadata(version, tag, commit)
			if err != nil || got.Version != version || got.Revision != commit || got.Prerelease != strings.Contains(version, "-") {
				t.Fatalf("release metadata: %+v, %v", got, err)
			}
		}
	}
	for _, test := range []struct{ version, tag, revision string }{
		{"bad", "", commit}, {"0.1.0", "v0.2.0", commit}, {"0.1.0", "0.1.0", commit},
		{"0.1.0", "refs/tags/v0.1.0", commit}, {"0.1.0", "v0.1.0\n", commit},
		{"0.1.0", "", ""}, {"0.1.0", "", "123"},
		{"0.1.0-" + strings.Repeat("a", maxReleaseTagLength), "", commit},
	} {
		if _, err := releaseMetadata(test.version, test.tag, test.revision); err == nil {
			t.Errorf("accepted invalid release metadata: %+v", test)
		}
	}
	boundary := "0.1.0-" + strings.Repeat("a", maxReleaseTagLength-len("0.1.0-"))
	if _, err := releaseMetadata(boundary, "v"+boundary, commit); err != nil {
		t.Fatalf("valid container tag boundary: %v", err)
	}
}

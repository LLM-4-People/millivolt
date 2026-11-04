package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt"
)

func TestReleaseCommand(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, args := range [][]string{{"-revision", commit}, {"-revision", commit, "-tag", "v" + millivolt.Version()}} {
		var out, diagnostics bytes.Buffer
		if err := run(args, strings.NewReader(""), &out, &diagnostics); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("version=%s\nprerelease=%t\nrevision=%s\n", millivolt.Version(), strings.Contains(millivolt.Version(), "-"), commit)
		if out.String() != want || diagnostics.Len() != 0 {
			t.Fatalf("output %q; diagnostics %q", &out, &diagnostics)
		}
	}
	for _, args := range [][]string{nil, {"-revision", ""}, {"-revision", "bad"}, {"-revision", commit, "-tag", ""}, {"-revision", commit, "-tag", "v" + millivolt.Version() + ".invalid"}, {"-revision", commit, "unexpected"}, {"-unknown"}} {
		var out, diagnostics bytes.Buffer
		if err := run(args, strings.NewReader(""), &out, &diagnostics); err == nil || out.Len() != 0 {
			t.Fatalf("invalid args %q: err %v, output %q", args, err, &out)
		}
	}
	if err := run([]string{"-revision", commit}, strings.NewReader(""), failingWriter{}, &bytes.Buffer{}); !errors.Is(err, errWrite) {
		t.Fatalf("output failure lost: %v", err)
	}
}

func TestReleaseCommandNewerThan(t *testing.T) {
	for _, newer := range []string{"0.0.0", "0.0.0-alpha"} {
		var out, diagnostics bytes.Buffer
		if err := run([]string{"-newer-than", newer}, strings.NewReader(""), &out, &diagnostics); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("version=%s\nnewer_than=%s\n", millivolt.Version(), newer)
		if out.String() != want || diagnostics.Len() != 0 {
			t.Fatalf("output %q; diagnostics %q", &out, &diagnostics)
		}
	}
	for _, args := range [][]string{
		{"-newer-than", "bad"},
		{"-newer-than", millivolt.Version()},
		{"-newer-than", "999.0.0"},
		{"-newer-than", "0.0.0", "-revision", strings.Repeat("a", 40)},
		{"-newer-than", "0.0.0", "-tag", "v" + millivolt.Version()},
	} {
		var out, diagnostics bytes.Buffer
		if err := run(args, strings.NewReader(""), &out, &diagnostics); err == nil || out.Len() != 0 {
			t.Fatalf("invalid args %q: err %v, output %q", args, err, &out)
		}
	}
}

// TestReleaseCommandNewest pins the single owner of "newest release version":
// SemVer precedence (not tag-name or version sort), ignoring a stray tag that
// is not a release version, and an empty answer for an empty or invalid input
// so a first release has no predecessor.
func TestReleaseCommandNewest(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		warns []string
	}{
		{"empty", "", "", nil},
		{"blank lines only", "\n  \n\n", "", nil},
		{"single", "1.2.3\n", "1.2.3\n", nil},
		{"leading v", "v1.2.3\n", "1.2.3\n", nil},
		{"semver precedence, not lexical", "0.4.9\n0.4.10\n0.4.8\n", "0.4.10\n", nil},
		{"prerelease sorts below its release", "1.0.0-rc1\n1.0.0\n0.9.9\n", "1.0.0\n", nil},
		{"alphanumeric prerelease compares lexically", "1.0.0-rc10\n1.0.0-rc9\n", "1.0.0-rc9\n", nil},
		{"invalid tag ignored with a warning", "1.0.0\nv1.0\nvendor-2024\n", "1.0.0\n", []string{`ignoring "1.0"`, `ignoring "vendor-2024"`}},
		{"only invalid tags", "v1.0\nnope\n", "", []string{`ignoring "1.0"`, `ignoring "nope"`}},
		{"duplicates and trailing spaces", " 2.0.0 \n2.0.0\n", "2.0.0\n", nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			err := run([]string{"-newest"}, strings.NewReader(test.input), &out, &diagnostics)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if out.String() != test.want {
				t.Fatalf("output %q, want %q", &out, test.want)
			}
			for _, warn := range test.warns {
				if !strings.Contains(diagnostics.String(), warn) {
					t.Fatalf("diagnostics %q missing %q", &diagnostics, warn)
				}
			}
			if len(test.warns) == 0 && diagnostics.Len() != 0 {
				t.Fatalf("unexpected diagnostics %q", &diagnostics)
			}
		})
	}
	// -newest is a mode of its own: mixing it with a metadata flag must fail
	// rather than silently ignoring the other request.
	for _, args := range [][]string{
		{"-newest", "-revision", strings.Repeat("a", 40)},
		{"-newest", "-tag", "v" + millivolt.Version()},
		{"-newest", "-newer-than", "0.0.0"},
	} {
		var out, diagnostics bytes.Buffer
		if err := run(args, strings.NewReader("1.0.0\n"), &out, &diagnostics); err == nil || out.Len() != 0 {
			t.Fatalf("invalid args %q: err %v, output %q", args, err, &out)
		}
	}
	// A failure on the stdout write must surface, not be reported as success.
	var diagnostics bytes.Buffer
	if err := run([]string{"-newest"}, strings.NewReader("1.0.0\n"), failingWriter{}, &diagnostics); !errors.Is(err, errWrite) {
		t.Fatalf("output failure lost: %v", err)
	}
}

var errWrite = errors.New("fixture write failure")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

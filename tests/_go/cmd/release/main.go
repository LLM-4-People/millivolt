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
		if err := run(args, &out, &diagnostics); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("version=%s\nprerelease=%t\nrevision=%s\n", millivolt.Version(), strings.Contains(millivolt.Version(), "-"), commit)
		if out.String() != want || diagnostics.Len() != 0 {
			t.Fatalf("output %q; diagnostics %q", &out, &diagnostics)
		}
	}
	for _, args := range [][]string{nil, {"-revision", ""}, {"-revision", "bad"}, {"-revision", commit, "-tag", ""}, {"-revision", commit, "-tag", "v" + millivolt.Version() + ".invalid"}, {"-revision", commit, "unexpected"}, {"-unknown"}} {
		var out, diagnostics bytes.Buffer
		if err := run(args, &out, &diagnostics); err == nil || out.Len() != 0 {
			t.Fatalf("invalid args %q: err %v, output %q", args, err, &out)
		}
	}
	if err := run([]string{"-revision", commit}, failingWriter{}, &bytes.Buffer{}); !errors.Is(err, errWrite) {
		t.Fatalf("output failure lost: %v", err)
	}
}

func TestReleaseCommandNewerThan(t *testing.T) {
	for _, newer := range []string{"0.0.0", "0.0.0-alpha"} {
		var out, diagnostics bytes.Buffer
		if err := run([]string{"-newer-than", newer}, &out, &diagnostics); err != nil {
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
		if err := run(args, &out, &diagnostics); err == nil || out.Len() != 0 {
			t.Fatalf("invalid args %q: err %v, output %q", args, err, &out)
		}
	}
}

var errWrite = errors.New("fixture write failure")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

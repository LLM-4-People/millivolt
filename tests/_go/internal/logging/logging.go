package logging

import (
	"bytes"
	"log"
	"testing"
)

// capture points the standard logger at a buffer for one test and restores the
// process sink afterwards.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	})
	return &buf
}

func TestPrintfPrefixesARequestID(t *testing.T) {
	buf := capture(t)
	Printf("req-1", "translated %d", 7)
	if got, want := buf.String(), "request req-1: translated 7\n"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestPrintfLeavesProcessLinesUnprefixed(t *testing.T) {
	buf := capture(t)
	Printf("", "process line")
	if got, want := buf.String(), "process line\n"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestPrintfDoesNotAliasTheCallersArguments(t *testing.T) {
	buf := capture(t)
	Printf("req-2", "%s", "value")
	if got := buf.String(); got != "request req-2: value\n" {
		t.Fatalf("line = %q", got)
	}
	// A second call must not inherit the first call's id through a shared
	// backing array in append.
	buf.Reset()
	Printf("", "clean")
	if got := buf.String(); got != "clean\n" {
		t.Fatalf("second line = %q, want the unprefixed line", got)
	}
}

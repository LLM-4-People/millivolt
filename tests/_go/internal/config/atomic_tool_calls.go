package config

import (
	"strings"
	"testing"
)

// TestAtomicToolCallsAndAppendRetries pins the two settings that make an
// append-behind-relayed-bytes rescue sound, and - the load-bearing part - that
// their UNSOUND combination cannot be configured at all. The append rescue
// re-sends behind bytes already on the wire, so it is only safe while no
// tool-call byte reached the client, and atomic_tool_calls is the only thing
// that guarantees that. A comment cannot hold that invariant; Validate can.
func TestAtomicToolCallsAndAppendRetries(t *testing.T) {
	d := Default()
	// Both default OFF, so a deployment's wire is byte-identical to the
	// pre-atomic relay until an operator opts in. Atomic withholding is not a
	// free win: for a provider that sends neither finish_reason nor [DONE] it
	// discards a tool call the eager relay would have delivered, and it stops
	// tool-call delivery being progressive. That trade is the operator's.
	if d.AtomicToolCalls {
		t.Fatal("atomic_tool_calls must default off: it can discard a completed tool call from a no-terminator provider")
	}
	if d.AppendRetries != 0 {
		t.Fatalf("default append_retries = %d, want 0 (it duplicates visible text and re-bills, so it is opt-in)", d.AppendRetries)
	}
	// The shipped defaults must be a valid configuration.
	if err := d.Validate(); err != nil {
		t.Fatalf("Default() must validate: %v", err)
	}

	for _, raw := range []string{
		"atomic_tool_calls: true",
		"append_retries: 0",
		"atomic_tool_calls: true\nappend_retries: 3",
	} {
		c, err := loadStormConfig(t, raw)
		if err != nil {
			t.Errorf("valid YAML %q: %v", raw, err)
			continue
		}
		if err := c.Validate(); err != nil {
			t.Errorf("valid YAML %q rejected by Validate: %v", raw, err)
		}
	}

	for _, bad := range []int{-1, AppendRetriesMax + 1} {
		c := Default()
		c.AppendRetries = bad
		err := c.Validate()
		if err == nil {
			t.Errorf("append_retries %d accepted, want rejection", bad)
			continue
		}
		if !strings.Contains(err.Error(), "append_retries") {
			t.Errorf("append_retries %d error must name the key, got %v", bad, err)
		}
	}

	// The unsound combination: appending behind a relayed tool call can merge
	// both attempts' arguments for the same tool-call index, which yields a
	// malformed tool call or a spurious execution.
	for _, n := range []int{1, 2, AppendRetriesMax} {
		c := Default()
		c.AtomicToolCalls = false
		c.AppendRetries = n
		err := c.Validate()
		if err == nil {
			t.Fatalf("append_retries %d with atomic_tool_calls false must be rejected", n)
		}
		if !strings.Contains(err.Error(), "atomic_tool_calls") {
			t.Errorf("the rejection must name the setting that makes it sound, got %v", err)
		}
	}
	// ...and the safe form of the same budget is accepted.
	c := Default()
	c.AtomicToolCalls = true
	c.AppendRetries = AppendRetriesMax
	if err := c.Validate(); err != nil {
		t.Fatalf("the sound combination must be accepted: %v", err)
	}

	for _, key := range []string{"atomic_tool_calls", "append_retries"} {
		found := false
		for _, f := range Schema() {
			if f.Key != key {
				continue
			}
			found = true
			if !f.HotReload {
				t.Errorf("%s must hot-reload (it only gates new streams and rescue decisions)", key)
			}
			if f.Category != "queue" {
				t.Errorf("%s category = %q, want queue", key, f.Category)
			}
		}
		if !found {
			t.Errorf("%s missing from the settings schema", key)
		}
	}
}

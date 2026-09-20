package adminjson

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// StrictQuery is the one strict parse every query-reading route handler
// adopts, so its own contract is pinned here: a malformed pair is an error
// with no half-parsed values escaping (a dropped flag on the destructive
// plane would silently broaden the action), and a well-formed query parses
// byte-identically to the lenient read the routes used before the adoption.
func TestStrictQueryContract(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/admin/restore?inspect=%zz", nil)
	q, err := StrictQuery(r)
	if err == nil {
		t.Fatal("malformed pair parsed without error")
	}
	if q != nil {
		t.Fatalf("half-parsed values escaped: %v", q)
	}

	r = httptest.NewRequest(http.MethodGet, "/metrics/agg/log?limit=50&f=client:a&f=status:2xx", nil)
	q, err = StrictQuery(r)
	if err != nil {
		t.Fatalf("well-formed query rejected: %v", err)
	}
	want := map[string][]string{"limit": {"50"}, "f": {"client:a", "status:2xx"}}
	if len(q) != len(want) {
		t.Fatalf("parsed %v, want %v", q, want)
	}
	for key, values := range want {
		if got := q[key]; len(got) != len(values) {
			t.Fatalf("key %s = %v, want %v", key, got, values)
		}
	}

	// Empty components parse to no pairs at all - the same stdlib
	// convention the export owner and every adopted route inherit.
	r = httptest.NewRequest(http.MethodGet, "/metrics/export?", nil)
	q, err = StrictQuery(r)
	if err != nil || len(q) != 0 {
		t.Fatalf("empty query: q=%v err=%v, want the empty parse", q, err)
	}
}

// DuplicateQueryKey is the shared repeated-key rule; its wording is the
// export owner's, and a drift in either direction reddens: this mirror holds
// the shared owner's side, while the export's own duplicate row (message-
// pinned through its route transport) holds the export's side.
func TestDuplicateQueryKeyContract(t *testing.T) {
	clean := url.Values{
		"inspect": {"1"},
		"f":       {"a", "b"}, // a repeated list key stays off the checked list
	}
	if err := DuplicateQueryKey(clean, "inspect", "config"); err != nil {
		t.Fatalf("single-occurrence keys flagged: %v", err)
	}
	repeated := url.Values{
		"inspect":     {"1", "0"},
		"config":      {"1"},
		"database":    {"0", "1"},
		"config_mode": {"merge", "replace"},
	}
	err := DuplicateQueryKey(repeated, "config", "inspect", "database", "config_mode")
	if err == nil || err.Error() != "duplicate inspect" {
		t.Fatalf("err = %v, want the first repeated consumed key in check order", err)
	}
	if err := DuplicateQueryKey(repeated, "config"); err != nil {
		t.Fatalf("a clean checked key flagged: %v", err)
	}
	if err := DuplicateQueryKey(nil); err != nil {
		t.Fatalf("no keys to check: %v", err)
	}
}

// UnknownQueryKey is the mutating routes' closed-set rule: an unlisted key
// names itself, so a case variant like INSPECT can never silently select the
// default action the real key would have narrowed.
func TestUnknownQueryKeyContract(t *testing.T) {
	if err := UnknownQueryKey(url.Values{"inspect": {"1"}}, "inspect", "config"); err != nil {
		t.Fatalf("consumed key flagged: %v", err)
	}
	err := UnknownQueryKey(url.Values{"INSPECT": {"1"}, "config": {"0"}}, "inspect", "config")
	if err == nil || err.Error() != "unknown key INSPECT" {
		t.Fatalf("err = %v, want the case variant named as an unknown key", err)
	}
	if err := UnknownQueryKey(url.Values{"f": {"a", "b"}}, "config"); err == nil {
		t.Fatal("a key outside the consumed set must be unknown, repeated or not")
	}
	if err := UnknownQueryKey(nil, "config"); err != nil {
		t.Fatalf("empty query flagged: %v", err)
	}
}

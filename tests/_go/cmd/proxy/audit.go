package main

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestAuditLogShape pins the one audit rendering: the action and every
// key/value pair, values quoted so a filter value with a space stays one
// token, and an odd trailing field still rendered.
func TestAuditLogShape(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	auditLog("purge", "outcome", "ok", "filter", "client=alpha beta", "deleted", 3)
	got := strings.TrimSpace(buf.String())
	want := `audit: purge outcome="ok" filter="client=alpha beta" deleted="3"`
	// log.Printf prefixes its own timestamp; the audit document starts after
	// the standard logger's separator.
	if _, after, ok := strings.Cut(got, "audit: "); ok {
		got = "audit: " + after
	}
	if got != want {
		t.Fatalf("audit line = %q, want %q", got, want)
	}
}

// TestDescribeFilter pins the audit rendering of a purge selection and its nil
// case: the bodyless full deletion must read "all", and a real filter must name
// exactly the constraints, never a JSON blob.
func TestDescribeFilter(t *testing.T) {
	if got := describeFilter(nil); got != "all" {
		t.Fatalf("nil filter = %q, want all", got)
	}
	if got := describeFilter(&storage.PurgeFilter{}); got != "all" {
		t.Fatalf("empty filter = %q, want all", got)
	}
	f := storage.PurgeFilter{Provider: "alpha", Client: "beta gamma", HasError: true, BeforeMs: 99}
	got := describeFilter(&f)
	for _, want := range []string{"provider=alpha", "client=beta gamma", "has_error=1", "before_ms=99"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeFilter = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "model=") {
		t.Errorf("describeFilter = %q names an unset field", got)
	}
	if got := reviewedText(nil); got != "-" {
		t.Errorf("reviewedText(nil) = %q, want -", got)
	}
	reviewed := int64(12)
	if got := reviewedText(&reviewed); got != strconv.Itoa(12) {
		t.Errorf("reviewedText(12) = %q, want 12", got)
	}
}

// TestRejectedParameterErrorsStillAudit pins one audit line per terminal
// outcome for the earliest refusals too: a query key on the body-only purge
// route, an unknown backup/restore key and a malformed restore mode are all
// operator mistakes that must be reconstructable, with the unparsed selection
// named as such instead of the line disappearing.
func TestRejectedParameterErrorsStillAudit(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	logMux := http.NewServeMux()
	registerLogRoutes(logMux, metrics.NewBuffer(4), nil)
	backupMux := http.NewServeMux()
	registerBackupRoutes(backupMux)
	serve := func(mux *http.ServeMux, method, target string) int {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(method, target, nil))
		return rr.Code
	}
	for _, tc := range []struct {
		name   string
		action string
		marker string
		call   func() int
	}{
		{"purge query key", "purge", `filter="unparsed"`, func() int {
			return serve(logMux, http.MethodPost, "/admin/purge?provider=alpha")
		}},
		{"backup unknown key", "backup", `members="unparsed"`, func() int {
			return serve(backupMux, http.MethodGet, "/admin/backup?CONFIG=1")
		}},
		{"restore unknown key", "restore", `members="unparsed"`, func() int {
			return serve(backupMux, http.MethodPost, "/admin/restore?CONFIG=1")
		}},
		{"restore malformed mode", "restore", `members="config" config_mode="unparsed"`, func() int {
			return serve(backupMux, http.MethodPost, "/admin/restore?config=1&config_mode=bogus")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			if code := tc.call(); code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", code)
			}
			out := buf.String()
			line := "audit: " + tc.action + ` outcome="failed"`
			if strings.Count(out, "audit: ") != 1 || !strings.Contains(out, line) ||
				!strings.Contains(out, tc.marker) || !strings.Contains(out, `error="`) {
				t.Fatalf("audit output %q, want one %q line naming %s and the error", out, line, tc.marker)
			}
		})
	}
}

// TestDescribeMembers pins the archive selection rendering, including both
// sides of the combined case.
func TestDescribeMembers(t *testing.T) {
	for _, tc := range []struct {
		cfg, db bool
		want    string
	}{
		{true, true, "config+database"},
		{true, false, "config"},
		{false, true, "database"},
		{false, false, "none"},
	} {
		if got := describeMembers(tc.cfg, tc.db); got != tc.want {
			t.Errorf("describeMembers(%v, %v) = %q, want %q", tc.cfg, tc.db, got, tc.want)
		}
	}
}

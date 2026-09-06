package storage

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The Python publication policy must fail when the real storage schema gains
// an unreviewed field. Build its source through Store.Open, not duplicated DDL,
// and reopen the derived file through the same production migration owner.
func TestDocumentationCopyUsesCanonicalSchemaAndSurvivesOpen(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for documentation-copy integration; scripts/check.sh requires it")
	}
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.db"), filepath.Join(dir, "docs.db")
	store, err := Open(source, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const secret = "private-fixture-content-never-publish"
	start := time.Date(2026, 2, 2, 2, 40, 0, 0, time.UTC)
	for i, status := range []int{200, 429, 499, 503} {
		record := &metrics.Record{
			ID: fmt.Sprintf("private-%d", i), Provider: secret, Model: secret,
			ProviderModel: secret, Client: secret, KeyHash: secret,
			ConversationID: "private-parent", Stream: true,
			Start: start.Add(time.Duration(i) * time.Minute), StatusCode: status,
			FirstTokenAt: start.Add(time.Second), LastTokenAt: start.Add(2 * time.Second),
			FinalAttemptAt: start, FirstAnswerAt: start.Add(time.Second),
			DurationMs: 2000, TTFTMs: 1000, DecodeTPS: 20, OverallTPS: 10,
			Usage: metrics.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120,
				CacheReadTokens: 3, CacheWrite: 4, ReasoningTokens: 5},
			Cost: 0.125, ToolCalls: 2, ToolNames: []string{secret, secret},
			ErrorType: secret, ErrorCode: secret, ErrorMsg: secret,
			PromptPreview: secret, ResponsePreview: secret, Debug: true, DebugSessionID: secret,
			ClientMeta:      metrics.ClientMeta{OS: secret, TimeoutMs: 99},
			ResponseHeaders: map[string][]string{"fixture-header": {secret}},
			ReqMaxTokens:    ptr(0), ReqTemperature: ptr(0.0), ReqParallelTools: ptr(false), ReqSeed: ptr(int64(0)),
			Attempts: []metrics.RetryAttempt{
				{StatusCode: 429, ErrorType: "rate_limit", ErrorMsg: secret, RetryAfterMs: 3, At: start},
				{StatusCode: 0, ErrorType: "transport", ErrorMsg: secret, At: start},
			},
		}
		if i == 1 {
			record.ConversationID, record.ParentConversationID = "private-child", "private-parent"
		}
		if i == 2 {
			record.Client, record.KeyHash = "second-client", "second-key"
		}
		if i == 3 {
			record.Attempts = append(record.Attempts, metrics.RetryAttempt{StatusCode: 503, ErrorType: secret, At: start})
		}
		store.Record(record)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDebugCapture(t.Context(), "private-0", secret, start.UnixMilli(), 0, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("INSERT INTO meta VALUES ('paused_keys', ?)", secret); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadRecent(t.Context(), 10)
	if err != nil || len(before) != 4 {
		t.Fatalf("source records = %d, error = %v", len(before), err)
	}
	// Keep the source store open: the helper must take a consistent online
	// snapshot of the WAL-backed source, never copy its base file directly.
	scripts, err := filepath.Abs("../../scripts")
	if err != nil {
		t.Fatal(err)
	}
	runPython := func(args ...string) {
		t.Helper()
		if output, err := exec.CommandContext(t.Context(), python, append([]string{"-B", "-O"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("documentation-copy helper failed: %v\n%s", err, output)
		}
	}
	runPython(filepath.Join(scripts, "backup_db.py"), "--docs-safe", source, destination)
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secret)) {
		t.Fatal("derived database retained private fixture bytes")
	}
	derived, err := Open(destination, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	after, err := derived.LoadRecent(t.Context(), 10)
	if err != nil || len(after) != len(before) {
		t.Fatalf("derived records = %d, error = %v", len(after), err)
	}
	for i, want := range before {
		got := after[i]
		// Compare every top-level scalar/pointer metric and canonical usage/time
		// value. Text and ClientMeta are deliberately not publication metrics.
		wv, gv := reflect.ValueOf(*want), reflect.ValueOf(*got)
		for n := range wv.NumField() {
			name, field := wv.Type().Field(n).Name, wv.Field(n)
			if name == "Debug" {
				continue
			}
			switch field.Kind() {
			case reflect.Bool, reflect.Int, reflect.Int64, reflect.Float64, reflect.Pointer:
			default:
				if field.Type() != reflect.TypeFor[time.Time]() && name != "Usage" {
					continue
				}
			}
			if !reflect.DeepEqual(field.Interface(), gv.Field(n).Interface()) {
				t.Errorf("record %d metric %s changed during publication", i, name)
			}
		}
		if got.IsError() != want.IsError() || got.HasRateLimit() != want.HasRateLimit() {
			t.Errorf("record %d changed error/429 membership", i)
		}
		if len(got.Attempts) != len(want.Attempts) {
			t.Fatalf("record %d changed retry count", i)
		}
		for n, attempt := range got.Attempts {
			previous := want.Attempts[n]
			if attempt.StatusCode != previous.StatusCode || attempt.RetryAfterMs != previous.RetryAfterMs || !attempt.At.Equal(previous.At) {
				t.Errorf("record %d retry %d changed numeric/timing facts", i, n)
			}
			if n < 2 && attempt.ErrorType != previous.ErrorType {
				t.Errorf("record %d changed retry error suppression class", i)
			}
		}
		if len(got.ToolNames) != 2 || got.ToolNames[0] != got.ToolNames[1] || got.Model != got.ProviderModel {
			t.Errorf("record %d changed model/tool identity memberships", i)
		}
	}
	if after[0].ConversationID != after[1].ParentConversationID || after[0].ConversationID != after[2].ConversationID || after[0].KeyHash == after[2].KeyHash {
		t.Fatal("conversation relationships or namespace distinctions changed")
	}
	// Migration/projection metadata added by the live reader must not invalidate
	// provenance, while captures and copied operator metadata remain forbidden.
	runPython("-c", "import sys; sys.path.insert(0, sys.argv[1]); from backup_db import verify_docs_copy; verify_docs_copy(sys.argv[2])", scripts, destination)
}

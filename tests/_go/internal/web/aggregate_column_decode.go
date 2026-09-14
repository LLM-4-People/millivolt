package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestExplorerAbsorbsCorruptAttemptsColumn pins the dashboard side of the
// shared attempts-column decoder: a corrupted column on one row must not
// fail the explorer scan, must drop only that row's absorbed-attempt error
// signature (the row stops being an error), and must leave every other row's
// signature and the full-scope footer counts intact.
func TestExplorerAbsorbsCorruptAttemptsColumn(t *testing.T) {
	d := config.Default()
	path := filepath.Join(t.TempDir(), "corrupt-attempts.db")
	s, err := storage.Open(path, storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Millisecond,
		QueryTimeout:  time.Second,
		WriteTrackCap: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Non-corrupt row: its own final-error signature.
	s.Record(mkRec("sig-500", now.Add(-2*time.Minute), 500, "server_error", nil, 0, 500, 10, 10, 0, 0))
	// Corruption target: an absorbed 502 attempt contributes an error
	// signature and makes the final-200 row an error while the column
	// decodes.
	s.Record(mkRec("att-502", now.Add(-time.Minute), 200, "",
		[]metrics.RetryAttempt{{StatusCode: 502, ErrorType: "provider_error", At: now}}, 300, 900, 10, 10, 0, 0.01))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// Pre-corruption sanity: both signatures are grouped and both rows are
	// errors.
	api := NewAggAPI(metrics.NewBuffer(8), s, time.Second)
	e, err := api.explorer(t.Context(), "error", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Total != 2 || e.ErrorTot != 2 || len(e.Groups) != 2 || e.Scope.Matches != 2 || e.Scope.Errors != 2 {
		t.Fatalf("pre-corruption fold = total %d errors %d groups %d scope %d/%d, want 2/2/2/2/2",
			e.Total, e.ErrorTot, len(e.Groups), e.Scope.Matches, e.Scope.Errors)
	}

	// Corrupt the attempts column through a second handle; the data-version
	// bump invalidates the memo so the next explorer call refolds.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`UPDATE requests SET attempts = '{' WHERE id = ?`, "att-502"); err != nil {
		t.Fatal(err)
	}
	other.Close()

	e, err = api.explorer(t.Context(), "error", nil, "")
	if err != nil {
		t.Fatalf("explorer scan with a corrupted attempts column: %v", err)
	}
	// Only the non-corrupt row's signature remains grouped; the corrupted
	// row lost its absorbed attempt and is no longer an error, while the
	// scope still matches every folded row.
	if len(e.Groups) != 1 || e.Groups[0].Name != "server_error|500|" {
		t.Fatalf("post-corruption groups = %+v, want only the non-corrupt row's signature", e.Groups)
	}
	if e.ErrorTot != 1 {
		t.Fatalf("post-corruption error_total = %d, want 1", e.ErrorTot)
	}
	if e.Scope.Matches != 2 || e.Scope.Errors != 1 {
		t.Fatalf("post-corruption scope = %d/%d, want matches 2 errors 1", e.Scope.Matches, e.Scope.Errors)
	}
	s.Close()
}

// TestBootstrapSignalsTotalsDegraded pins the storage signal wiring: a store
// whose boot scan failed reports totals_degraded=true in the bootstrap
// payload (the contract for the planned degraded banner; no frontend render
// yet), and a healthy store reports false.
func TestBootstrapSignalsTotalsDegraded(t *testing.T) {
	d := config.Default()
	bootSignal := func(queryTimeout time.Duration) map[string]any {
		t.Helper()
		s, err := storage.Open(filepath.Join(t.TempDir(), "boot.db"), storage.Options{
			WriteChanCap:  d.StorageWriteChanCap,
			BatchCap:      d.StorageBatchCap,
			FlushInterval: time.Millisecond,
			QueryTimeout:  queryTimeout,
			WriteTrackCap: 64,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		api := NewAggAPI(metrics.NewBuffer(8), s, time.Second)
		rr := httptest.NewRecorder()
		api.HandleBootstrap(rr, httptest.NewRequest(http.MethodGet, "/metrics/bootstrap", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /metrics/bootstrap = %d %s", rr.Code, rr.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		storageSection, ok := doc["storage"].(map[string]any)
		if !ok {
			t.Fatalf("bootstrap carries no storage section: %v", doc)
		}
		return storageSection
	}
	if got := bootSignal(time.Nanosecond)["totals_degraded"]; got != true {
		t.Fatalf("degraded boot: storage.totals_degraded = %v, want true", got)
	}
	if got := bootSignal(time.Second)["totals_degraded"]; got != false {
		t.Fatalf("healthy boot: storage.totals_degraded = %v, want false", got)
	}
}

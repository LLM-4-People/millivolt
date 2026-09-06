package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
	"github.com/LLM-4-People/millivolt/internal/web"
)

func TestOperatorCrossOriginBoundary(t *testing.T) {
	h := protectOperatorRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, path := range []string{"/admin/restart", "/admin/pause", "/admin/config", "/admin/throttle", "/admin/debug", "/admin/reload", "/metrics/purge", "/metrics/purge/count", "/v1/chat/completions"} {
		for _, origin := range []string{"", "http://proxy.example", "https://foreign.example", "null"} {
			r := httptest.NewRequest(http.MethodPost, "http://proxy.example"+path, nil)
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := http.StatusNoContent
			if path != "/v1/chat/completions" && origin != "" && origin != "http://proxy.example" {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Errorf("%s origin=%q: status=%d want=%d", path, origin, w.Code, want)
			}
		}
	}
	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/restart", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-site browser mutation passed")
	}
}

// livePayload decodes the bootstrap/SSE payload wrapper for snapshot
// assertions (mirrors the internal/metrics feed-test fixture).
type livePayload struct {
	Records     []*metrics.Record `json:"records"`
	Seq         int64             `json:"seq"`
	OldestSeq   int64             `json:"oldest_seq"`
	Incremental bool              `json:"incremental"`
}

func decodeLivePayload(t *testing.T, raw []byte) livePayload {
	t.Helper()
	var p livePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v\n%s", err, raw)
	}
	return p
}

func fullSnapshot(t *testing.T, b *metrics.Buffer) livePayload {
	t.Helper()
	raw, err := b.SnapshotJSONSince(0)
	if err != nil {
		t.Fatal(err)
	}
	p := decodeLivePayload(t, raw)
	if p.Incremental {
		t.Fatalf("since=0 must be a FULL snapshot, got incremental")
	}
	return p
}

// TestApplySnapshotLimitGatedOnDurableStore is the regression for the
// reload-path bug: reloadConfig re-applied the full-snapshot cap gated only
// on the buffer (always non-nil), never on store presence - so an
// in-memory-only instance (db_path empty / -db-path none) got its full
// snapshots capped to 8×dash_log_rows on the FIRST reload while the ring
// held history_size records, and the /metrics/agg/log paging fallback is
// dead without a store: older rows became unreachable until restart. The
// gate must mirror boot: capped only when durable storage is on, unlimited
// otherwise (then the ring IS all history); a delta is never capped.
func TestApplySnapshotLimitGatedOnDurableStore(t *testing.T) {
	const rows = 1                         // smallest band value; 8×rows cap
	const capN = web.BootRingCapMul * rows // 8
	const total = capN + 2                 // strictly more than the cap

	b := metrics.NewBuffer(total)
	for i := 0; i < total; i++ {
		b.Record(&metrics.Record{
			ID: "r" + strconv.Itoa(i), Provider: "p", StatusCode: 200, Start: time.Now(),
		})
	}

	// No store: the ring IS all history - the full snapshot must carry
	// EVERY record (the buggy reload wiring capped it to capN).
	applySnapshotLimit(b, nil, rows)
	if p := fullSnapshot(t, b); len(p.Records) != total {
		t.Errorf("no store: full snapshot has %d records, want all %d (unlimited)",
			len(p.Records), total)
	}

	// A real store: full snapshots cap to the newest capN records, and the
	// delta path is unaffected (it may exceed the cap - capping a delta
	// would be a silent miss).
	d := config.Default()
	store, err := storage.Open(filepath.Join(t.TempDir(), "gate.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	applySnapshotLimit(b, store, rows)
	p := fullSnapshot(t, b)
	if len(p.Records) != capN {
		t.Errorf("with store: full snapshot has %d records, want capped %d", len(p.Records), capN)
	}
	wantFirst := "r" + strconv.Itoa(total-capN)
	if p.Records[0].ID != wantFirst || p.Records[len(p.Records)-1].ID != "r"+strconv.Itoa(total-1) {
		t.Errorf("with store: capped window = %s..%s, want newest %d (%s..)",
			p.Records[0].ID, p.Records[len(p.Records)-1].ID, capN, wantFirst)
	}
	raw, err := b.SnapshotJSONSince(1)
	if err != nil {
		t.Fatal(err)
	}
	dp := decodeLivePayload(t, raw)
	if !dp.Incremental || len(dp.Records) != total-1 {
		t.Errorf("delta after cap: inc=%v n=%d, want incremental %d (uncapped; exceeds the %d cap)",
			dp.Incremental, len(dp.Records), total-1, capN)
	}

	// The gate owns both states: back to no store → unlimited again.
	applySnapshotLimit(b, nil, rows)
	if p := fullSnapshot(t, b); len(p.Records) != total {
		t.Errorf("no store after store: full snapshot has %d records, want all %d restored",
			len(p.Records), total)
	}
}

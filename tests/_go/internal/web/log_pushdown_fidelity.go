package web

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// logPushdownFixture is the pushdown-fidelity fixture: every scoped value is
// present in a different column, so swapping one pushed field for another
// (client -> provider, key -> conversation, ...) returns the wrong rows
// instead of merely changing performance.
func logPushdownFixture(t *testing.T) *storage.Store {
	t.Helper()
	const total = 5_000
	d := config.Default()
	s, err := storage.Open(filepath.Join(t.TempDir(), "log-pushdown.db"), storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: time.Millisecond,
		QueryTimeout:  time.Minute,
		WriteTrackCap: total * 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := time.Date(2026, 1, 2, 12, 0, 0, 0, time.Local)
	for i := 0; i < total; i++ {
		rec := &metrics.Record{
			ID:         fmt.Sprintf("row-%05d", i),
			Provider:   fmt.Sprintf("provider-%02d", i%13),
			Model:      "model-a",
			Client:     fmt.Sprintf("client-%02d", i%37),
			KeyHash:    fmt.Sprintf("key-%02d", i%19),
			StatusCode: 200,
			Start:      base.Add(time.Duration(i) * time.Millisecond),
		}
		// Distinct target values in four different columns, repeated across
		// the whole history. No target spelling equals another column's
		// value. The matches overlap rows but each field is independent, so
		// no scope's total lands exactly on the page boundary.
		rec.ConversationID = fmt.Sprintf("conv-%02d", i%17)
		if i%11 == 0 {
			rec.Client = "client-target"
		}
		if i%23 == 4 {
			rec.KeyHash = "key-target"
		}
		if i%29 == 7 {
			rec.ConversationID = "conv-target"
		}
		s.Record(rec)
		if i%512 == 511 {
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	return s
}

// pushdownOraclePage answers one page directly from the full-record read. It
// deliberately does not call logPageQuery/planLogPage or the Go scope matcher:
// the expected page comes from the record's stored field, which is the contract
// the SQL pushdown must preserve.
func pushdownOraclePage(all []storage.LogCursor, before *storage.LogCursor, limit int) ([]storage.LogCursor, bool) {
	below := func(c storage.LogCursor, at *storage.LogCursor) bool {
		if at == nil {
			return true
		}
		return c.StartMs < at.StartMs || c.StartMs == at.StartMs && c.ID < at.ID
	}
	var page []storage.LogCursor
	for _, c := range all {
		if !below(c, before) {
			continue
		}
		page = append(page, c)
		if len(page) == limit {
			break
		}
	}
	return page, len(page) == limit && len(all) > 0 && func() bool {
		// A full page has a next page when at least one oracle row sorts
		// below its last returned cursor.
		last := page[len(page)-1]
		for _, c := range all {
			if below(c, &last) {
				return true
			}
		}
		return false
	}()
}

// TestLogPagePushdownFieldFidelity pages client, key and conversation scopes
// against the independent full-record oracle above. The fixture is spread over
// 5,000 rows and every page chain runs to its short/truncation page, so the
// test covers SQL narrowing, cursor continuation and the end-of-history signal.
func TestLogPagePushdownFieldFidelity(t *testing.T) {
	s := logPushdownFixture(t)
	allRecords, err := s.QueryBefore(t.Context(), nil, nil, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(allRecords) != 5_000 {
		t.Fatalf("oracle read %d records, want 5000", len(allRecords))
	}
	oracle := func(dim, target string) []storage.LogCursor {
		out := make([]storage.LogCursor, 0, len(allRecords))
		for _, rec := range allRecords {
			match := false
			switch dim {
			case "client":
				match = rec.Client == target
			case "key":
				match = rec.KeyHash == target
			case "conversation":
				match = rec.ConversationID == target
			}
			if match {
				out = append(out, storage.LogCursor{StartMs: rec.Start.UnixMilli(), ID: rec.ID})
			}
		}
		return out
	}
	const limit = 25
	for _, tc := range []struct{ dim, target string }{
		{"client", "client-target"},
		{"key", "key-target"},
		{"conversation", "conv-target"},
	} {
		t.Run(tc.dim, func(t *testing.T) {
			want := oracle(tc.dim, tc.target)
			if len(want) <= 2*limit {
				t.Fatalf("fixture has %d %s matches, want several pages at limit %d", len(want), tc.dim, limit)
			}
			h := http.HandlerFunc(NewAggAPI(metrics.NewBuffer(1), s, time.Minute).HandleLogPage)
			var before *storage.LogCursor
			pages := 0
			for {
				target := "/metrics/agg/log?limit=" + strconv.Itoa(limit) + "&f=" + tc.dim + ":" + url.QueryEscape(tc.target)
				if before != nil {
					target += fmt.Sprintf("&before_ms=%d&before_id=%s", before.StartMs, url.QueryEscape(before.ID))
				}
				page := get(t, h, target)
				rows, ok := page["records"].([]any)
				if !ok {
					t.Fatalf("page %d records = %v", pages, page["records"])
				}
				wantRows, wantMore := pushdownOraclePage(want, before, limit)
				if len(rows) != len(wantRows) {
					t.Fatalf("page %d returned %d rows, want %d", pages, len(rows), len(wantRows))
				}
				gotIDs := make([]string, len(rows))
				for i, row := range rows {
					gotIDs[i] = row.(map[string]any)["id"].(string)
				}
				wantIDs := make([]string, len(wantRows))
				for i, row := range wantRows {
					wantIDs[i] = row.ID
				}
				if !slices.Equal(gotIDs, wantIDs) {
					t.Fatalf("page %d ids = %v, want %v (SQL pushdown did not preserve the %s field)", pages, gotIDs, wantIDs, tc.dim)
				}
				if page["more"] != wantMore {
					t.Fatalf("page %d more = %v, want %v", pages, page["more"], wantMore)
				}
				if len(wantRows) == 0 {
					break
				}
				last := wantRows[len(wantRows)-1]
				if gotMs := int64(page["cursor_ms"].(float64)); gotMs != last.StartMs || page["cursor_id"] != last.ID {
					t.Fatalf("page %d cursor = %d/%v, want %d/%s", pages, gotMs, page["cursor_id"], last.StartMs, last.ID)
				}
				before = &storage.LogCursor{StartMs: last.StartMs, ID: last.ID}
				if !wantMore {
					break
				}
				pages++
				if pages > 100 {
					t.Fatal("page chain did not terminate")
				}
			}
			if pages < 2 {
				t.Fatalf("%s chain ended after %d pages, want cursor continuation", tc.dim, pages+1)
			}
		})
	}
}

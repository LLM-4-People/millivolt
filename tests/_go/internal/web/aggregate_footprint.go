package web

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// This file owns the projection's reproducible memory measurements: retained
// bytes per stored record (the number the dashboard footprint scales with),
// the transient allocation volume that drives peak RSS during the startup
// preload, and the process RSS after readiness. The documented instance-level
// footprint tables in docs/operations.md need a live instance; these
// benchmarks measure the same mechanism deterministically from a seeded
// private scratch database. Never point them at a live instance's database:
// storage.Open may migrate it.

// Small sizes expose per-record steady state; the large size matches the
// documented measured-example scale and would expose superlinear growth in
// the dictionaries, the request-id set or the metric orders.
var footprintSizes = [...]int{10_000, 100_000}

var (
	footprintProviders = [...]string{"fixture-anthropic", "fixture-openai", "fixture-bedrock"}
	footprintModels    = [...]string{"fixture-opus-4", "fixture-sonnet-4", "fixture-haiku-3",
		"gpt-fixture-4o", "gpt-fixture-4o-mini", "fixture-gemini-pro",
		"fixture-llama-70b", "fixture-mistral-large", "fixture-qwen-max",
		"fixture-grok-2", "fixture-deepseek-chat", "fixture-glm-4"}
	footprintClients = [...]string{"fixture-desktop", "fixture-openai-python", "fixture-cline",
		"fixture-continue", "fixture-curl", "fixture-sdk-node",
		"fixture-sdk-java", "fixture-webhook"}
	footprintKeys = [...]string{"9f2c4a1b0d3e5f67", "1e8d7c6b5a493827", "a0b1c2d3e4f50617",
		"7c6b5a49382d1e0f", "3f5e7d9c1b3a2c40"}
	footprintTools = [...]string{"web_search", "file_read", "file_write", "bash_exec",
		"list_directory", "code_search", "git_status", "git_diff", "run_tests",
		"fetch_url", "sql_query", "http_request", "parse_json", "edit_file", "summarize_doc"}
)

// footprintBase is a fixed epoch so every seeded database and every benchmark
// run decodes identical rows; measurements never depend on wall-clock.
var footprintBase = time.Unix(1_700_000_000, 0)

// footprintRecord builds one deterministic provider-neutral record whose
// decode shape matches a realistic workload: unique request ids, repeated
// low-cardinality labels, shared conversations with parent declarations,
// tool names on a third of rows, absorbed attempts on error rows, and a
// small fraction of invalid speed samples that exercise the metric-order gate.
func footprintRecord(i int) *metrics.Record {
	r := &metrics.Record{
		ID:         fmt.Sprintf("req-%032d", i),
		Provider:   footprintProviders[i%len(footprintProviders)],
		Model:      footprintModels[i%len(footprintModels)],
		Client:     footprintClients[i%len(footprintClients)],
		KeyHash:    footprintKeys[i%len(footprintKeys)],
		Start:      footprintBase.Add(-time.Duration(i) * time.Second),
		StatusCode: 200,
		DurationMs: int64(100 + i%1500),
		TTFTMs:     int64(1 + i%3000),
		OverallTPS: float64(1 + i%500),
		Cost:       float64(i%97) * 0.001,
		Usage: metrics.Usage{InputTokens: int64(100 + i%900), OutputTokens: int64(50 + i%2000),
			CacheReadTokens: int64(i % 4096), ReasoningTokens: int64(i % 512)},
		AnswerTokens: int64(20 + i%800),
		Stream:       i%3 == 0,
	}
	if i%25 == 0 {
		// Invalid speed samples must stay out of the sorted metric orders.
		r.OverallTPS, r.DecodeTPS = 0, 0
	}
	if i%5 == 0 {
		// Five consecutive rows share one conversation label.
		r.ConversationID = fmt.Sprintf("conv-%06d", i/25)
	}
	if i%7 == 0 {
		parent := i / 25
		if parent > 0 {
			parent--
		}
		r.ParentConversationID = fmt.Sprintf("conv-%06d", parent)
	}
	if i%10 < 3 {
		names := make([]string, 1+i%3)
		for j := range names {
			names[j] = footprintTools[(i+j)%len(footprintTools)]
		}
		r.ToolNames = names
		r.ToolCalls = len(names)
	}
	if i%20 == 0 {
		r.StatusCode = 500
		r.ErrorType = "upstream_error"
		r.ErrorCode = "E500"
		r.ErrorMsg = "fixture upstream returned an internal error after the retry budget"
		r.Attempts = []metrics.RetryAttempt{{
			StatusCode: 503, ErrorType: "http_503",
			ProviderRequestID: fmt.Sprintf("req-%032d", i),
			At:                r.Start.Add(-time.Second),
		}}
	}
	return r
}

// footprintStore seeds a private scratch database and returns it with the
// configuration the options came from. Record never blocks, so the seed is
// chunked by the queue capacity and flushed between chunks, exactly like the
// storage writer benchmark's discipline; the dropped counter and a counted
// projection scan then verify the fixture fail-closed instead of assuming it.
func footprintStore(b *testing.B, n int) (*storage.Store, *config.Config, string) {
	b.Helper()
	d := config.Default()
	opts := storage.Options{WriteChanCap: d.StorageWriteChanCap, BatchCap: d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval, QueryTimeout: d.StorageQueryTimeout}
	path := filepath.Join(b.TempDir(), "footprint.db")
	s, err := storage.Open(path, opts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	for base := 0; base < n; base += opts.WriteChanCap {
		end := min(base+opts.WriteChanCap, n)
		for i := base; i < end; i++ {
			s.Record(footprintRecord(i))
		}
		if err := s.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	if dropped := s.Dropped(); dropped != 0 {
		b.Fatalf("footprint fixture dropped %d records", dropped)
	}
	var counted int
	if _, _, err := s.StreamProjection(b.Context(), "id", storage.ProjectionCursor{}, func(rows *sql.Rows) error {
		counted++
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	if counted != n {
		b.Fatalf("footprint fixture seeded %d rows, want %d", counted, n)
	}
	return s, d, path
}

// footprintHeap settles the heap before reading live bytes; the second GC
// lets the scavenger reclaim both dead objects and their spans.
func footprintHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func footprintTotalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// BenchmarkProjectionFootprint reports the projection's retained bytes per
// stored record. The "built" metric measures the real owner path
// (AggAPI.PreloadHistory -> historyProjection.refresh); the staged breakdown
// mirrors refresh's build order (scan, intern plus request-id set, metric
// orders) to attribute the bytes. If a projection change makes the staged
// sum drift from "built", the staging here must be updated with it.
func BenchmarkProjectionFootprint(b *testing.B) {
	for _, n := range footprintSizes {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			s, d, _ := footprintStore(b, n)
			for b.Loop() {
				// The api and its ring predate the baseline so the "built"
				// delta carries projection state only, not the fixed ring/
				// api allocation that would otherwise differ per size.
				api := NewAggAPI(metrics.NewBuffer(d.HistorySize), s, d.StorageQueryTimeout)
				api.ModelCanon = d.ModelCanon
				base := footprintHeap()
				alloc := footprintTotalAlloc()
				if err := api.PreloadHistory(context.Background()); err != nil {
					b.Fatal(err)
				}
				built := footprintHeap() - base
				// Go's GC is precise about locals: without this anchor the
				// projection is unreachable right after PreloadHistory
				// returns, and the heap snapshot above would measure a heap
				// that already collected it.
				runtime.KeepAlive(api)
				transient := footprintTotalAlloc() - alloc
				api = nil

				base = footprintHeap()
				var incoming []contrib
				if _, _, err := s.StreamProjection(context.Background(), rowColumns,
					storage.ProjectionCursor{}, func(rows *sql.Rows) error {
						c, err := scanContrib(rows, nil)
						if err == nil {
							incoming = append(incoming, c)
						}
						return err
					}); err != nil {
					b.Fatal(err)
				}
				if len(incoming) != n {
					b.Fatalf("staged scan decoded %d rows, want %d", len(incoming), n)
				}
				scanned := footprintHeap() - base
				p := projectionData{ids: make(map[string]struct{}, len(incoming))}
				for i := range incoming {
					c := &incoming[i]
					c.rowIndex = uint32(i + 1)
					p.dimensions.intern(c)
					if c.id != "" {
						p.ids[c.id] = struct{}{}
					}
				}
				interned := footprintHeap() - base
				p.rows = growRows(nil, incoming)
				p.metrics.append(p.rows, 0)
				ordered := footprintHeap() - base
				// Same liveness anchor as above: the staged projection must
				// survive the snapshot's GC; this anchor is its last use.
				runtime.KeepAlive(&p)

				// Stage deltas can be slightly negative when the settling GC
				// reclaims noise beyond what the stage allocated, so deltas
				// are interpreted as signed instead of wrapping uint64.
				per := func(delta uint64) float64 { return float64(int64(delta)) / float64(n) }
				b.ReportMetric(per(built), "built-B/record")
				b.ReportMetric(per(transient), "transient-B/record")
				b.ReportMetric(per(scanned), "scanned-B/record")
				b.ReportMetric(per(interned), "interned-B/record")
				b.ReportMetric(per(ordered), "ordered-B/record")
				b.ReportMetric(float64(unsafe.Sizeof(contrib{})), "contrib-struct-B")
				b.ReportMetric(float64(unsafe.Sizeof(errEnt{})), "errEnt-struct-B")
				b.ReportMetric(float64(unsafe.Sizeof(metricSample[int64]{})), "metricSample-struct-B")
			}
		})
	}
}

// BenchmarkStoreDiskBytes reports the durable SQLite bytes retained per
// stored record: the database file plus its WAL sidecar after the final
// flush, before Close folds the WAL back in. Row payload, the unique id
// index, the three secondary indexes, the projection triggers' state and the
// marshaled response-header JSON are all inside the number; it is the
// disk-side twin of the projection benchmarks above.
func BenchmarkStoreDiskBytes(b *testing.B) {
	for _, n := range footprintSizes {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			_, _, path := footprintStore(b, n)
			for b.Loop() {
				var total int64
				for _, suffix := range []string{"", "-wal", "-shm"} {
					if fi, err := os.Stat(path + suffix); err == nil {
						total += fi.Size()
					}
				}
				per := float64(total) / float64(n)
				b.ReportMetric(per, "B/record")
				b.ReportMetric(float64(total)/(1<<20), "MiB-total")
			}
		})
	}
}

// BenchmarkProjectionProcessRSS reproduces the documented idle "after
// readiness" row: seed, preload, settle, then read this process's resident
// pages. The benchmark binary carries the test framework and every linked
// package, so absolute MiB is a stable relative measure, not the deployed
// proxy's exact RSS. That fixed binary/runtime constant weighs once per
// process, so the per-100k normalization is only comparable at one size
// across revisions; across sizes, compare the MiB difference between the two
// rows instead.
func BenchmarkProjectionProcessRSS(b *testing.B) {
	for _, n := range footprintSizes {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			s, d, _ := footprintStore(b, n)
			for b.Loop() {
				api := NewAggAPI(metrics.NewBuffer(d.HistorySize), s, d.StorageQueryTimeout)
				api.ModelCanon = d.ModelCanon
				if err := api.PreloadHistory(context.Background()); err != nil {
					b.Fatal(err)
				}
				runtime.GC()
				runtime.GC()
				rss, ok := footprintRSSMiB()
				if !ok {
					b.Skip("/proc/self/statm is unavailable on this platform")
				}
				b.ReportMetric(rss, "MiB")
				b.ReportMetric(rss*100_000/float64(n), "MiB-per-100k")
				// The projection must stay live through the settling GCs and
				// the RSS read above; see the liveness note in the footprint
				// benchmark.
				runtime.KeepAlive(api)
			}
		})
	}
}

func footprintRSSMiB() (float64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(pages) * float64(os.Getpagesize()) / (1 << 20), true
}

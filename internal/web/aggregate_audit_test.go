package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func TestKPICommitBetweenFormerIndependentReads(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "race.db"), storage.Options{WriteChanCap: 8, BatchCap: 8, FlushInterval: time.Hour, QueryTimeout: time.Second, WriteTrackCap: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := metrics.NewBuffer(8)
	s.Recorder(b).Record(&metrics.Record{ID: "race", Start: time.Now(), StatusCode: 200})
	a := NewAggAPI(b, s, time.Second)
	// The former KPI read totals, then compiled model rules before reading
	// written IDs. A commit there deterministically omitted both copies. KPI
	// now uses one atomic accounting capture and has no model-rule work.
	a.ModelCanon = func() config.ModelCanon {
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		return config.ModelCanon{}
	}
	if got := a.kpi(); got.Requests != 1 {
		t.Fatalf("racing committed request omitted: %+v", got)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := a.kpi(); got.Requests != 1 {
		t.Fatalf("committed request doubled: %+v", got)
	}
}

func TestAggregateUnrepresentableNumbersFailExplicitly(t *testing.T) {
	for _, kind := range []string{"count", "cost"} {
		t.Run(kind, func(t *testing.T) {
			b := metrics.NewBuffer(8)
			for _, id := range []string{"first", "second"} {
				r := &metrics.Record{ID: id, Provider: "neutral.example", Start: time.Now(), StatusCode: 200}
				if kind == "count" {
					r.Usage.InputTokens = math.MaxInt64
				} else {
					r.Cost = 1e308
				}
				b.Record(r)
			}
			a := NewAggAPI(b, nil, time.Second)
			for _, tc := range []struct {
				url     string
				handler http.HandlerFunc
			}{{"/metrics/bootstrap", a.HandleBootstrap}, {"/metrics/agg/chart", a.HandleAggChart}, {"/metrics/agg/explorer?dim=provider", a.HandleAggExplorer}} {
				w := httptest.NewRecorder()
				tc.handler(w, httptest.NewRequest("GET", tc.url, nil))
				if w.Code != 500 || !json.Valid(w.Body.Bytes()) {
					t.Fatalf("%s returned %d %s", tc.url, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestAggregateLargeRepresentableCostKeepsUnavailableRatio(t *testing.T) {
	b := metrics.NewBuffer(8)
	b.Record(&metrics.Record{ID: "cost", Provider: "neutral.example", Start: time.Now(), StatusCode: 200, Cost: 1e308, Usage: metrics.Usage{InputTokens: 1}})
	a := NewAggAPI(b, nil, time.Second)
	p := get(t, http.HandlerFunc(a.HandleBootstrap), "/metrics/bootstrap")
	kpi := p["kpi"].(map[string]any)
	if kpi["cost"] != float64(1e308) || kpi["cost_per_mtok"] != nil {
		t.Fatalf("unrepresentable ratio fabricated: %v", kpi)
	}
	rows := get(t, http.HandlerFunc(a.HandleAggExplorer), "/metrics/agg/explorer?dim=provider")["groups"].([]any)
	if rows[0].(map[string]any)["cost_per_mtok"] != nil {
		t.Fatal("explorer ratio must be unavailable")
	}
	b.Reset()
	b.Record(&metrics.Record{ID: "sum", Provider: "neutral.example", Start: time.Now(), StatusCode: 200, Cost: 1, Usage: metrics.Usage{InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64}})
	if ratio := a.kpi().CostPerMTok; ratio == nil || *ratio <= 0 {
		t.Fatal("floating ratio denominator wrapped input+output")
	}
}

func TestBootstrapStorageSignal(t *testing.T) {
	a := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	p := get(t, http.HandlerFunc(a.HandleBootstrap), "/metrics/bootstrap")["storage"].(map[string]any)
	if p["enabled"] != false || p["dropped"] != float64(0) {
		t.Fatalf("ring-only signal=%v", p)
	}
	s := testStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.Record(&metrics.Record{ID: "closed"})
	a = NewAggAPI(metrics.NewBuffer(4), s, time.Second)
	p = get(t, http.HandlerFunc(a.HandleBootstrap), "/metrics/bootstrap")["storage"].(map[string]any)
	if p["enabled"] != true || p["dropped"] != float64(1) {
		t.Fatalf("durable signal=%v", p)
	}
}

func TestObserverModelDictionaryAndTimeAcrossBootstrapAndArchive(t *testing.T) {
	s := testStore(t)
	b := metrics.NewBuffer(8)
	a := NewAggAPI(b, s, time.Second)
	a.ModelCanon = func() config.ModelCanon {
		return config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	}
	r := &metrics.Record{ID: "raw", Model: "MODEL", Start: time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local).UTC(), StatusCode: 200}
	s.Recorder(b).Record(r)
	b.PublishLive("begin", &metrics.Record{ID: "pending", Model: "PENDING", Start: r.Start})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	boot := get(t, http.HandlerFunc(a.HandleBootstrap), "/metrics/bootstrap")
	canon := boot["model_canon"].(map[string]any)
	if canon["names"].(map[string]any)["MODEL"] != "model" || canon["names"].(map[string]any)["PENDING"] != "pending" {
		t.Fatalf("incomplete dictionary=%v", canon)
	}
	page := get(t, http.HandlerFunc(a.HandleLogPage), "/metrics/agg/log?limit=10")
	if page["model_canon"].(map[string]any)["revision"] != canon["revision"] {
		t.Fatal("observer rule identity drift")
	}
	for _, payload := range []map[string]any{boot, page} {
		row := payload["records"].([]any)[0].(map[string]any)
		if row["model"] != "MODEL" || row["time_bucket"] != "work" {
			t.Fatalf("wire record=%v", row)
		}
	}
	if r.Model != "MODEL" {
		t.Fatal("observer mutated live record")
	}
	a.ModelCanon = func() config.ModelCanon { return config.ModelCanon{} }
	changed := a.ObserveModelNames([]string{"MODEL"}).(modelObserverState)
	if changed.Revision == canon["revision"] || changed.Names["MODEL"] != "MODEL" {
		t.Fatalf("rule change not applied: %+v", changed)
	}
}

func TestObserverRulesOnlyTravelWithFullSnapshots(t *testing.T) {
	a := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	a.ModelCanon = func() config.ModelCanon {
		return config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	}
	records := []*metrics.Record{{Model: "  PADDED  "}}
	full := a.ObserveModels(true, records).(modelObserverState)
	delta := a.ObserveModels(false, records).(modelObserverState)
	if full.Rules == nil || delta.Rules != nil || delta.Names["  PADDED  "] != "padded" || full.Revision != delta.Revision {
		t.Fatalf("full=%+v delta=%+v", full, delta)
	}
	data, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["rules"]; ok {
		t.Fatal("event repeated static rules")
	}
}

func BenchmarkObserverSerialization(b *testing.B) {
	for _, count := range []int{1, 800} {
		buf := metrics.NewBuffer(count)
		api := NewAggAPI(buf, nil, time.Second)
		rules := config.Default().ModelCanon()
		api.ModelCanon = func() config.ModelCanon { return rules }
		for i := 0; i < count; i++ {
			buf.Record(&metrics.Record{ID: fmt.Sprint(i), Model: fmt.Sprintf("MODEL-%d", i%16), Start: time.UnixMilli(1800000000000), StatusCode: 200})
		}
		snap := buf.SnapshotSince(0)
		for _, observe := range []bool{false, true} {
			b.Run(fmt.Sprintf("rows%d/observed%t", count, observe), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					var value any = snap
					if observe {
						value = metrics.ObserveSnapshot(snap, api.ObserveModels(true, snap.Records))
					}
					if _, err := json.Marshal(value); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkObserverEventSerialization(b *testing.B) {
	api := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
	rules := config.Default().ModelCanon()
	api.ModelCanon = func() config.ModelCanon { return rules }
	rec := &metrics.Record{ID: "record", Model: "MODEL", Start: time.UnixMilli(1800000000000), StatusCode: 200}
	for _, observe := range []bool{false, true} {
		b.Run(fmt.Sprintf("observed%t", observe), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var value any = rec
				if observe {
					value = metrics.ObservedRecord{Record: rec, TimeBucket: metrics.TimeBucket(rec.Start), ModelCanon: api.ObserveModels(false, []*metrics.Record{rec})}
				}
				if _, err := json.Marshal(value); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

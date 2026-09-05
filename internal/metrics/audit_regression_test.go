package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUsageRejectsUnrepresentableAndFractionalCounts(t *testing.T) {
	for _, value := range []string{"9223372036854775808", "9223372036854775807", "1.5", `"9223372036854775808"`, `"NaN"`, "-1"} {
		var root any
		if err := json.Unmarshal([]byte(`{"prompt_tokens":`+value+`,"input_tokens":7}`), &root); err != nil {
			t.Fatal(err)
		}
		if got := ParseUsage(root, nil).InputTokens; got != 7 {
			t.Errorf("count %s accepted: %d", value, got)
		}
	}
}

func TestCheckedMetricArithmetic(t *testing.T) {
	if n, err := SumCounts(math.MaxInt64-1, 1); err != nil || n != math.MaxInt64 {
		t.Fatalf("valid sum=%d,%v", n, err)
	}
	for _, values := range [][]int64{{math.MaxInt64, 1}, {-1, 1}} {
		if _, err := SumCounts(values...); err == nil {
			t.Fatalf("invalid sum accepted: %v", values)
		}
	}
	for _, values := range [][]float64{{math.MaxFloat64, math.MaxFloat64}, {math.NaN()}, {math.Inf(1)}, {-1}} {
		if _, err := SumValues(values...); err == nil {
			t.Fatalf("invalid values accepted: %v", values)
		}
	}
	if v := ScaledRatio(1e308, 1, 1e6); v != nil {
		t.Fatalf("unrepresentable ratio=%v", *v)
	}
	if v := ScaledRatio(1e308, 1e10, 1e6); v == nil || math.IsInf(*v, 0) {
		t.Fatalf("representable scaled ratio=%v", v)
	}
}

func TestScaledRatioExtremeIntermediates(t *testing.T) {
	for _, tc := range []struct{ n, d, s, want float64 }{
		{1e-320, 1e6, 1e6, 1e-320},
		{1e-320, 100, 100, 1e-320},
		{1e308, 1e-308, 1e-308, 1e308},
		{0, 1, 1, 0},
	} {
		got := ScaledRatio(tc.n, tc.d, tc.s)
		if got == nil || math.Abs(*got-tc.want) > math.Abs(tc.want)*1e-15 {
			t.Errorf("ratio(%g,%g,%g)=%v want %g", tc.n, tc.d, tc.s, got, tc.want)
		}
	}
	for _, tc := range [][3]float64{
		{math.SmallestNonzeroFloat64, 1e308, 1},
		{1e308, 1, 1e308},
		{1, math.Inf(1), 1},
		{math.Inf(1), 1, 1},
		{1, 1, math.Inf(1)},
		{math.NaN(), 1, 1}, {1, math.NaN(), 1}, {1, 1, math.NaN()},
	} {
		if got := ScaledRatio(tc[0], tc[1], tc[2]); got != nil {
			t.Errorf("invalid ratio%v=%g", tc, *got)
		}
	}
}

func TestJSONKeyWhitespaceEscapesAndStringDecoys(t *testing.T) {
	for _, raw := range []string{"{\"error\"\r\n : {\"message\":\"failed\"}}", `{"err\u006fr":{"message":"failed"}}`} {
		if typ, _, _ := ParseErrorEnvelope([]byte(raw)); typ == "" || !HasErrorKey([]byte(raw)) {
			t.Errorf("error skipped: %s", raw)
		}
	}
	for _, raw := range []string{`{"content":"say \"error\": now"}`, `{"error_kind":"failed"}`, `{"err\u006fr"`} {
		if HasErrorKey([]byte(raw)) {
			t.Errorf("decoy matched: %s", raw)
		}
	}
	if tail := JSONKey([]byte(`{"content":"fake \"usage\":{}","usage" : {"n":7}}`), "usage"); string(tail) != `{"n":7}}` {
		t.Fatalf("wrong value tail: %s", tail)
	}
}

func TestPrometheusUsesDocumentedNearestRank(t *testing.T) {
	if got := Percentile([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1000}, 95); got != 1000 {
		t.Fatalf("p95=%v, want nearest rank 11", got)
	}
}

func TestSubscriberOverflowRetiresOnlySlowChannel(t *testing.T) {
	b := NewBuffer(subChanCap + 8)
	slow, fast := b.Subscribe(), b.Subscribe()
	defer b.Unsubscribe(slow)
	defer b.Unsubscribe(fast)
	for i := 0; i < subChanCap+2; i++ {
		b.Record(&Record{ID: fmt.Sprint(i)})
		if ev, ok := <-fast; !ok || ev.Seq != int64(i+1) {
			t.Fatalf("healthy subscriber lost %d", i)
		}
	}
	var last int64
	for ev := range slow {
		last = ev.Seq
	}
	if last != subChanCap {
		t.Fatalf("slow cursor=%d", last)
	}
	snap := b.SnapshotRequest(httptest.NewRequest("GET", fmt.Sprintf("/?feed=%s&since=%d", b.FeedID(), last), nil))
	if !snap.Incremental || len(snap.Records) != 2 {
		t.Fatalf("gap not replayed: %+v", snap)
	}
	live := b.SubscribeLive()
	defer b.UnsubscribeLive(live)
	for i := 0; i < subChanCap+1; i++ {
		b.PublishLive("begin", &Record{ID: fmt.Sprintf("pending-%d", i)})
	}
	for range live {
	}
	if got := len(b.SnapshotSince(last).InFlightRecords); got != subChanCap+1 {
		t.Fatalf("pending registry lost overflowed lifecycle: %d", got)
	}
}

func TestStreamOverflowClosesBeforeSkippingCursor(t *testing.T) {
	b := NewBuffer(subChanCap + 8)
	w := &initialFeedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), resume: make(chan struct{})}
	entered := w.entered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { b.HandleStream(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx)); close(done) }()
	<-entered
	for i := 0; i < subChanCap+2; i++ {
		b.Record(&Record{ID: fmt.Sprint(i)})
	}
	close(w.resume)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("overflow stream did not retire")
	}
	var last int64
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "id: ") {
			last, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		}
	}
	if last != subChanCap {
		t.Fatalf("cursor advanced through gap: %d", last)
	}
	if got := len(b.SnapshotSince(last).Records); got != 2 {
		t.Fatalf("reconnect replay=%d", got)
	}
}

func TestObservedTimeBucketUsesServerZoneWithoutMutatingRecord(t *testing.T) {
	r := &Record{ID: "tz", Start: time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local).UTC()}
	view := ObserveRecords([]*Record{r})
	if view[0].TimeBucket != "work" || view[0].Record != r {
		t.Fatalf("observer=%+v", view[0])
	}
	if raw, _ := json.Marshal(r); strings.Contains(string(raw), "time_bucket") {
		t.Fatal("wire metadata leaked into base record")
	}
}

func TestStreamObserverMetadataCoversEveryEventShape(t *testing.T) {
	b := NewBuffer(8)
	b.SetModelObserver(func(full bool, groups ...[]*Record) any {
		names := map[string]string{}
		for _, records := range groups {
			for _, r := range records {
				names[r.Model] = strings.ToLower(r.Model)
			}
		}
		return map[string]any{"revision": "fixture", "names": names}
	})
	start := time.Date(2026, 9, 2, 10, 0, 0, 0, time.Local)
	b.Record(&Record{ID: "initial", Model: "INITIAL", Start: start})
	w := &initialFeedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), resume: make(chan struct{}), written: make(chan struct{}, 8)}
	entered := w.entered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { b.HandleStream(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx)); close(done) }()
	<-entered
	r := &Record{ID: "next", Model: "NEXT", Start: start}
	b.PublishLive("begin", r)
	b.Record(r)
	close(w.resume)
	for range 4 {
		select {
		case <-w.written:
		case <-time.After(time.Second):
			t.Fatal("missing observed event")
		}
	}
	b.CloseFeeds()
	<-done
	phase := ""
	seen := map[string]bool{}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			phase = strings.TrimPrefix(line, "event: ")
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatal(err)
		}
		mc, ok := payload["model_canon"].(map[string]any)
		if !ok || mc["revision"] != "fixture" {
			t.Fatalf("%s metadata=%v", phase, payload)
		}
		row := payload
		if phase == "snapshot" {
			row = payload["records"].([]any)[0].(map[string]any)
		} else if phase != "record" {
			row = payload["record"].(map[string]any)
		}
		if row["time_bucket"] != "work" {
			t.Fatalf("%s time bucket=%v", phase, row)
		}
		if mc["names"].(map[string]any)[row["model"].(string)] == nil {
			t.Fatalf("%s missing model mapping", phase)
		}
		seen[phase] = true
	}
	for _, phase := range []string{"snapshot", "begin", "record", "end"} {
		if !seen[phase] {
			t.Fatalf("missing phase %s", phase)
		}
	}
}

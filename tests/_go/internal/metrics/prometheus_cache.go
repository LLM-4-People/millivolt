package metrics

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// prometheusTestExposition renders the ring shape through the injected
// callback so tests can prove both whether a render ran and which records it
// saw.
func prometheusTestExposition(records []*Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "records=%d", len(records))
	for _, r := range records {
		b.WriteString("|")
		b.WriteString(r.Provider)
	}
	return b.String()
}

func TestPrometheusExpositionReusesUnchangedRevision(t *testing.T) {
	buf := NewBuffer(8)
	buf.Record(&Record{Provider: "alpha"})

	renders := 0
	render := func(records []*Record) (string, error) {
		renders++
		return prometheusTestExposition(records), nil
	}

	first, err := buf.PrometheusExposition(render)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	if got, want := first, "records=1|alpha"; got != want {
		t.Fatalf("first body = %q, want %q", got, want)
	}
	if renders != 1 {
		t.Fatalf("renders after first scrape = %d, want 1", renders)
	}

	second, err := buf.PrometheusExposition(render)
	if err != nil {
		t.Fatalf("cached scrape: %v", err)
	}
	if second != first {
		t.Fatalf("cached body = %q, want the identical %q", second, first)
	}
	if renders != 1 {
		t.Fatalf("unchanged scrape invoked render %d times, want the cached 1", renders)
	}
}

func TestPrometheusExpositionInvalidatesOnRingTransitions(t *testing.T) {
	t.Run("record", func(t *testing.T) {
		buf := NewBuffer(8)
		renders := 0
		render := func(records []*Record) (string, error) {
			renders++
			return prometheusTestExposition(records), nil
		}
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=0" {
			t.Fatalf("empty scrape = %q, %v; want records=0, nil", got, err)
		}
		buf.Record(&Record{Provider: "alpha"})
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=1|alpha" {
			t.Fatalf("post-record scrape = %q, %v; want records=1|alpha, nil", got, err)
		}
		if renders != 2 {
			t.Fatalf("renders = %d, want 2 (one per revision)", renders)
		}
	})

	t.Run("eviction", func(t *testing.T) {
		buf := NewBuffer(1)
		buf.Record(&Record{Provider: "alpha"})
		renders := 0
		render := func(records []*Record) (string, error) {
			renders++
			return prometheusTestExposition(records), nil
		}
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=1|alpha" {
			t.Fatalf("initial scrape = %q, %v; want records=1|alpha, nil", got, err)
		}
		buf.Record(&Record{Provider: "beta"}) // evicts alpha
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=1|beta" {
			t.Fatalf("post-eviction scrape = %q, %v; want records=1|beta, nil", got, err)
		}
		if renders != 2 {
			t.Fatalf("renders = %d, want 2 (eviction must invalidate)", renders)
		}
	})

	t.Run("purge", func(t *testing.T) {
		buf := NewBuffer(4)
		buf.Record(&Record{Provider: "alpha"})
		buf.Record(&Record{Provider: "beta"})
		renders := 0
		render := func(records []*Record) (string, error) {
			renders++
			return prometheusTestExposition(records), nil
		}
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=2|alpha|beta" {
			t.Fatalf("initial scrape = %q, %v; want records=2|alpha|beta, nil", got, err)
		}
		if removed := buf.RemoveWhere(func(*Record) bool { return true }); removed != 2 {
			t.Fatalf("removed = %d, want 2", removed)
		}
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=0" {
			t.Fatalf("post-purge scrape = %q, %v; want records=0, nil", got, err)
		}
		if renders != 2 {
			t.Fatalf("renders = %d, want 2 (purge must invalidate)", renders)
		}
	})

	t.Run("backfill", func(t *testing.T) {
		buf := NewBuffer(4)
		renders := 0
		render := func(records []*Record) (string, error) {
			renders++
			return prometheusTestExposition(records), nil
		}
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=0" {
			t.Fatalf("empty scrape = %q, %v; want records=0, nil", got, err)
		}
		buf.Backfill([]*Record{{Provider: "alpha"}})
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=1|alpha" {
			t.Fatalf("post-backfill scrape = %q, %v; want records=1|alpha, nil", got, err)
		}
		if renders != 2 {
			t.Fatalf("renders = %d, want 2 (backfill must invalidate)", renders)
		}
	})

	t.Run("reset", func(t *testing.T) {
		buf := NewBuffer(4)
		buf.Record(&Record{Provider: "alpha"})
		renders := 0
		render := func(records []*Record) (string, error) {
			renders++
			return prometheusTestExposition(records), nil
		}
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=1|alpha" {
			t.Fatalf("initial scrape = %q, %v; want records=1|alpha, nil", got, err)
		}
		buf.Reset()
		if got, err := buf.PrometheusExposition(render); err != nil || got != "records=0" {
			t.Fatalf("post-reset scrape = %q, %v; want records=0, nil", got, err)
		}
		if renders != 2 {
			t.Fatalf("renders = %d, want 2 (reset must invalidate)", renders)
		}
	})
}

func TestPrometheusExpositionIgnoresPendingRevision(t *testing.T) {
	buf := NewBuffer(4)
	buf.Record(&Record{Provider: "alpha"})
	renders := 0
	render := func(records []*Record) (string, error) {
		renders++
		return prometheusTestExposition(records), nil
	}
	first, err := buf.PrometheusExposition(render)
	if err != nil {
		t.Fatalf("initial render: %v", err)
	}
	// A pending begin moves only the pending revision; the exposition covers
	// the finalized ring, so the cached body must still answer this revision.
	buf.PublishLive("begin", &Record{ID: "pending", Provider: "beta"})
	second, err := buf.PrometheusExposition(render)
	if err != nil {
		t.Fatalf("scrape after pending begin: %v", err)
	}
	if second != first {
		t.Fatalf("pending change altered the exposition: got %q, want %q", second, first)
	}
	if renders != 1 {
		t.Fatalf("renders = %d, want the cached 1", renders)
	}
}

func TestPrometheusExpositionRenderErrorIsNotCached(t *testing.T) {
	buf := NewBuffer(4)
	buf.Record(&Record{Provider: "alpha"})
	boom := errors.New("render failed")

	renders := 0
	_, err := buf.PrometheusExposition(func([]*Record) (string, error) {
		renders++
		return "partial body", boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("failed render error = %v, want %v", err, boom)
	}
	if renders != 1 {
		t.Fatalf("renders = %d, want 1", renders)
	}

	// The failed body was never published, so the same revision must retry.
	body, err := buf.PrometheusExposition(func(records []*Record) (string, error) {
		renders++
		return "good body", nil
	})
	if err != nil {
		t.Fatalf("retry render: %v", err)
	}
	if body != "good body" {
		t.Fatalf("retry body = %q, want good body", body)
	}
	if renders != 2 {
		t.Fatalf("renders after retry = %d, want 2", renders)
	}

	// The successful retry is now the cached body for this revision.
	body, err = buf.PrometheusExposition(func([]*Record) (string, error) {
		renders++
		return "unexpected", nil
	})
	if err != nil || body != "good body" {
		t.Fatalf("cached retry scrape = %q, %v; want good body, nil", body, err)
	}
	if renders != 2 {
		t.Fatalf("unchanged scrape invoked render %d times, want 2", renders)
	}
}

func TestPrometheusExpositionConcurrentScrapesShareOneBuild(t *testing.T) {
	buf := NewBuffer(16)
	for range 8 {
		buf.Record(&Record{Provider: "alpha", StatusCode: 200, TTFTMs: 10})
	}
	const want = "stable exposition"

	var builds atomic.Int32
	render := func([]*Record) (string, error) {
		builds.Add(1)
		// Hold the cold build long enough that every caller has joined it.
		time.Sleep(5 * time.Millisecond)
		return want, nil
	}

	const workers = 32
	bodies := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bodies[i], errs[i] = buf.PrometheusExposition(render)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Fatalf("renders = %d, want one shared build", got)
	}
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("scrape %d: %v", i, errs[i])
		}
		if bodies[i] != want {
			t.Fatalf("scrape %d body = %q, want %q", i, bodies[i], want)
		}
	}
}

func TestHandlePrometheusServesCachedBodyUntilRingChanges(t *testing.T) {
	buf := NewBuffer(8)
	buf.Record(&Record{Provider: "alpha", StatusCode: 200, TTFTMs: 100})
	scrape := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
		w := httptest.NewRecorder()
		HandlePrometheus(w, req, buf)
		if got, want := w.Code, http.StatusOK; got != want {
			t.Fatalf("status = %d, want %d\n%s", got, want, w.Body.String())
		}
		if got, want := w.Header().Get("Content-Type"), "text/plain; version=0.0.4"; got != want {
			t.Errorf("Content-Type = %q, want %q", got, want)
		}
		if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
			t.Errorf("Cache-Control = %q, want %q", got, want)
		}
		return w.Body.String()
	}

	first := scrape()
	second := scrape()
	if second != first {
		t.Fatalf("unchanged scrape body drifted:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(first, "llm_proxy_requests_total 1\n") {
		t.Fatalf("first scrape missing one request:\n%s", first)
	}

	buf.Record(&Record{Provider: "beta", StatusCode: 500})
	third := scrape()
	if third == first {
		t.Fatalf("record change did not refresh the exposition:\n%s", third)
	}
	for _, want := range []string{
		"llm_proxy_requests_total 2\n",
		"llm_proxy_errors_total 1\n",
		"llm_proxy_ttft_seconds_count 1\n",
		`provider="beta"`,
	} {
		if !strings.Contains(third, want) {
			t.Errorf("refreshed exposition missing %q:\n%s", want, third)
		}
	}
}

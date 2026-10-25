package metrics

import (
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// HandlePrometheus renders the /metrics/prometheus exposition. It reuses the
// last body while the finalized ring revision is unchanged, so repeated
// scrapes skip the snapshot, aggregate, and TTFT sort. Callers register this on
// the mux.
func HandlePrometheus(w http.ResponseWriter, r *http.Request, buf *Buffer) {
	if !RejectUnlessGet(w, r) {
		return
	}
	// Operator-plane JSON lives behind the credential: the response carries
	// request totals and must never be cached by a shared intermediary.
	w.Header().Set("Cache-Control", "no-store")
	body, err := buf.PrometheusExposition(renderPrometheusExposition)
	if err != nil {
		log.Printf("metrics: aggregate: %v", err)
		http.Error(w, "metrics cannot be represented", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// Best-effort write: the client may have already disconnected; nothing
	// actionable remains at that point.
	_, _ = io.WriteString(w, body)
}

// prometheusSamplePool recycles the flat TTFT scratch between scrapes. The
// exposition is O(ring log ring) on every revision change, and a live ring
// changes on every finalized request, so the 8 MB sample slice at the
// million-record ceiling was fresh garbage per scrape. The disposed result
// never escapes renderPrometheusExposition, so the slice is safe to reuse; the
// pool drops it at GC like any other pooled buffer, and a miss allocates the
// same slice the fold always did. This is a scratch reuse, never a cached
// exposition: stale bodies remain impossible.
var prometheusSamplePool = sync.Pool{New: func() any { return new([]int64) }}

// renderPrometheusExposition folds one immutable ring snapshot into the
// exposition text. A failed aggregate returns no partial body, so the handler
// can fail the whole scrape.
func renderPrometheusExposition(records []*Record) (string, error) {
	samples := prometheusSamplePool.Get().(*[]int64)
	agg := aggregateInto(records, *samples)
	*samples = agg.TTFTSamples
	defer prometheusSamplePool.Put(samples)
	if agg.Err != nil {
		return "", agg.Err
	}

	// Size the body once: the fixed preamble, one line per provider and the
	// five summary lines. The estimate only avoids Builder growth; a longer
	// label still grows the builder normally.
	providers := sortedKeys(agg.ProviderCounts)
	size := 1024 + len(providers)*48
	for _, p := range providers {
		size += len(p)
	}
	var b strings.Builder
	b.Grow(size)
	writeHeader := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	// All totals derive from the bounded ring snapshot (they shrink on
	// eviction/reset/purge), so they are gauges - declaring them counters
	// would break Prometheus monotonicity and rate()/increase().
	writeHeader("llm_proxy_requests_total", "Total LLM proxy requests (current ring).", "gauge")
	fmt.Fprintf(&b, "llm_proxy_requests_total %d\n", agg.Total)

	writeHeader("llm_proxy_input_tokens_total", "Total input tokens (current ring).", "gauge")
	fmt.Fprintf(&b, "llm_proxy_input_tokens_total %d\n", agg.TotalInput)

	writeHeader("llm_proxy_output_tokens_total", "Total output tokens (current ring).", "gauge")
	fmt.Fprintf(&b, "llm_proxy_output_tokens_total %d\n", agg.TotalOutput)

	writeHeader("llm_proxy_cache_read_tokens_total", "Total cache-read tokens (current ring).", "gauge")
	fmt.Fprintf(&b, "llm_proxy_cache_read_tokens_total %d\n", agg.TotalCache)

	writeHeader("llm_proxy_errors_total", "Total requests with an error (current ring).", "gauge")
	fmt.Fprintf(&b, "llm_proxy_errors_total %d\n", agg.ErrorCount)

	writeHeader("llm_proxy_tool_calls_total", "Total tool calls observed (current ring).", "gauge")
	fmt.Fprintf(&b, "llm_proxy_tool_calls_total %d\n", agg.ToolCalls)

	// Per-provider request counts (low-cardinality dimension). Aggregate owns
	// the single record pass, including no-TTFT records.
	writeHeader("llm_proxy_requests_by_provider", "Requests grouped by provider (current ring).", "gauge")
	for _, p := range providers {
		fmt.Fprintf(&b, "llm_proxy_requests_by_provider{provider=\"%s\"} %d\n", prometheusLabelValue(p), agg.ProviderCounts[p])
	}

	// TTFT summary across all requests with a captured TTFT. Aggregate has
	// already excluded no-token records (TTFTMs == 0 means "absent") and
	// returned one flat record-ordered sample slice plus its checked sum.
	writeHeader("llm_proxy_ttft_seconds", "Time to first token in seconds.", "summary")
	ttfts := agg.TTFTSamples
	slices.Sort(ttfts)
	if len(ttfts) > 0 {
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds{quantile=\"%s\"} %g\n", prometheusLabelValue("0.5"), Percentile(ttfts, 50)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds{quantile=\"%s\"} %g\n", prometheusLabelValue("0.95"), Percentile(ttfts, 95)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds{quantile=\"%s\"} %g\n", prometheusLabelValue("0.99"), Percentile(ttfts, 99)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds_sum %g\n", float64(agg.TTFTSumMs)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds_count %d\n", len(ttfts))
	}

	return b.String(), nil
}

func sortedKeys(m map[string]int) []string {
	return slices.Sorted(maps.Keys(m))
}

// prometheusLabelValue escapes one label value for the Prometheus text
// exposition format. The format requires escaping only backslash, double
// quote, and line feed; every other valid UTF-8 sequence is legal raw. Invalid
// UTF-8 bytes are replaced with U+FFFD so one client-minted label can never
// make the whole scrape unparseable. Ordinary values return unchanged.
func prometheusLabelValue(s string) string {
	if utf8.ValidString(s) && !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

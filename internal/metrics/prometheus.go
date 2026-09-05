package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// HandlePrometheus renders the /metrics/prometheus exposition. It snapshots
// the ring buffer and emits aggregates. Callers register this on the mux.
func HandlePrometheus(w http.ResponseWriter, r *http.Request, buf *Buffer) {
	if !rejectUnlessGet(w, r) {
		return
	}
	records := buf.Snapshot()
	agg := Aggregate(records)
	if agg.Err != nil {
		http.Error(w, "metrics cannot be represented", http.StatusInternalServerError)
		return
	}

	var b strings.Builder
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

	// Per-provider request counts (low-cardinality dimension). ByStatus is
	// status-code keyed; per-provider counts come from the snapshot directly.
	writeHeader("llm_proxy_requests_by_provider", "Requests grouped by provider (current ring).", "gauge")
	providerCounts := make(map[string]int)
	for _, r := range records {
		if r != nil {
			providerCounts[r.Provider]++
		}
	}
	for _, p := range sortedKeys(providerCounts) {
		fmt.Fprintf(&b, "llm_proxy_requests_by_provider{provider=%q} %d\n", p, providerCounts[p])
	}

	// TTFT summary across all requests with a captured TTFT. ByProvider
	// already excludes no-token records (TTFTMs == 0 means "absent").
	writeHeader("llm_proxy_ttft_seconds", "Time to first token in seconds.", "summary")
	var ttfts []int64
	for _, vals := range agg.ByProvider {
		ttfts = append(ttfts, vals...)
	}
	sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })
	if len(ttfts) > 0 {
		var sum int64
		for _, v := range ttfts {
			var err error
			sum, err = SumCounts(sum, v)
			if err != nil {
				http.Error(w, "metrics cannot be represented", http.StatusInternalServerError)
				return
			}
		}
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds{quantile=\"0.5\"} %g\n", Percentile(ttfts, 50)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds{quantile=\"0.95\"} %g\n", Percentile(ttfts, 95)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds{quantile=\"0.99\"} %g\n", Percentile(ttfts, 99)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds_sum %g\n", float64(sum)/1e3)
		fmt.Fprintf(&b, "llm_proxy_ttft_seconds_count %d\n", len(ttfts))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	// Best-effort write: the client may have already disconnected; nothing
	// actionable remains at that point.
	w.Write([]byte(b.String()))
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

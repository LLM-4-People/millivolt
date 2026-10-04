package web

import (
	"database/sql"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// TestRowColumnsScanContribLockstep pins the lockstep between rowColumns
// (the SELECT list the chart and explorer scans stream) and scanContrib (the
// row decoder), which until now was comment-only: the two must carry the same
// columns in the same order, and every decoded contrib field must round-trip
// the value the writer stored. One record with a distinct sentinel per field
// fails loudly if either side drops, adds or reorders a column, instead of
// silently decoding zero values into an aggregate.
func TestRowColumnsScanContribLockstep(t *testing.T) {
	s := testStoreWithQueryTimeout(t, time.Second)

	start := time.UnixMilli(1_700_000_123_456)
	attemptAt := start.Add(10 * time.Millisecond)
	rec := &metrics.Record{
		ID: "sentinel-id", Provider: "sentinel.example", Model: "sentinel-model",
		KeyHash: "sentinel-key", UserAgent: "sentinel-agent", Client: "sentinel-client",
		ConversationID: "sentinel-conversation", ParentConversationID: "sentinel-parent",
		StatusCode: 502, Start: start, End: start.Add(time.Second),
		TTFTMs: 66, DurationMs: 1000, OverallTPS: 7.7, Cost: 0.55,
		ErrorType: "sentinel_error_type", ErrorCode: "sentinel_error_code", ErrorMsg: "sentinel error message",
		ToolCalls: 3, ToolNames: []string{"sentinel-tool-a", "sentinel-tool-b"},
		Usage: metrics.Usage{InputTokens: 11, OutputTokens: 22, CacheReadTokens: 33, ReasoningTokens: 44},
		// One flow-control attempt (the 429 predicate's probe) and one
		// absorbed failure (the error-signature path's probe), both with
		// distinct sentinels so the attempts column decode is pinned too.
		Attempts: []metrics.RetryAttempt{
			{StatusCode: 429, ErrorType: "sentinel_rate_limit", At: attemptAt},
			{StatusCode: 503, ErrorType: "sentinel_absorbed_type", ErrorCode: "sentinel_absorbed_code", ErrorMsg: "sentinel absorbed message", At: attemptAt},
		},
		// Exact millisecond boundaries, so the stored columns round-trip
		// bit-for-bit and the derived decode window is a distinct value.
		FirstTokenAt: start.Add(100 * time.Millisecond),
		LastTokenAt:  start.Add(350 * time.Millisecond),
	}
	s.Record(rec)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	var decoded []contrib
	if _, _, err := s.StreamProjection(t.Context(), rowColumns, storage.ProjectionCursor{}, func(rows *sql.Rows) error {
		c, err := scanContrib(rows, nil) // raw spelling; the canonizer is not under test
		if err != nil {
			return err
		}
		decoded = append(decoded, c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 {
		t.Fatalf("decoded %d rows, want the one sentinel row", len(decoded))
	}
	c := decoded[0]

	if c.id != "sentinel-id" {
		t.Errorf("id = %q", c.id)
	}
	if c.start != start.UnixMilli() {
		t.Errorf("start = %d, want %d", c.start, start.UnixMilli())
	}
	if c.status != 502 {
		t.Errorf("status = %d, want 502", c.status)
	}
	if c.client != "sentinel-client" {
		t.Errorf("client = %q", c.client)
	}
	if c.prov != "sentinel.example" {
		t.Errorf("prov = %q", c.prov)
	}
	if c.model != "sentinel-model" {
		t.Errorf("model = %q, want the raw spelling (nil canonizer)", c.model)
	}
	if c.key != "sentinel-key" {
		t.Errorf("key = %q", c.key)
	}
	if c.conv != "sentinel-conversation" {
		t.Errorf("conv = %q", c.conv)
	}
	if c.parentConv != "sentinel-parent" {
		t.Errorf("parentConv = %q", c.parentConv)
	}
	if c.in != 11 || c.out != 22 || c.cacheR != 33 || c.reason != 44 {
		t.Errorf("tokens in/out/cache/read-reasoning = %d/%d/%d/%d, want 11/22/33/44", c.in, c.out, c.cacheR, c.reason)
	}
	if c.tools != 3 {
		t.Errorf("tools = %d, want 3", c.tools)
	}
	if c.cost != 0.55 {
		t.Errorf("cost = %v, want 0.55", c.cost)
	}
	if c.ttft != 66 {
		t.Errorf("ttft = %d, want 66", c.ttft)
	}
	if c.tps != 7.7 {
		t.Errorf("tps = %v, want 7.7 (overall_tps wins the speed pick)", c.tps)
	}
	if c.dec != 250 {
		t.Errorf("dec = %d, want 250 (last_token_at - first_token_at)", c.dec)
	}
	if len(c.toolsL) != 2 || c.toolsL[0] != "sentinel-tool-a" || c.toolsL[1] != "sentinel-tool-b" {
		t.Errorf("toolsL = %v, want both sentinel tool names", c.toolsL)
	}
	if !c.isErr {
		t.Error("isErr = false, want true (final 502 with an error_type)")
	}
	if !c.has429 {
		t.Error("has429 = false, want true (the absorbed 429 attempt)")
	}
	// The error signature: the final 502 entry first, the absorbed 503
	// attempt second; the 429 attempt never becomes an error entry.
	if len(c.ent) != 2 {
		t.Fatalf("ent = %+v, want the final and absorbed-503 entries only", c.ent)
	}
	if fin := c.ent[0]; fin.typ != "sentinel_error_type" || fin.code != "sentinel_error_code" ||
		fin.msg != "sentinel error message" || fin.absorbed || fin.at != start.UnixMilli() {
		t.Errorf("final entry = %+v, want the sentinel final 502 signature", fin)
	}
	if att := c.ent[1]; att.typ != "sentinel_absorbed_type" || att.code != "sentinel_absorbed_code" ||
		att.msg != "sentinel absorbed message" || !att.absorbed || att.at != attemptAt.UnixMilli() {
		t.Errorf("absorbed entry = %+v, want the sentinel 503 attempt signature", att)
	}
}

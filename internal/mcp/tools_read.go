package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// QueryInput is a single bounded SELECT.
type QueryInput struct {
	SQL string `json:"sql" jsonschema:"one SELECT statement; the proxy rejects anything else, any ';', and ATTACH/DETACH/PRAGMA/LOAD_EXTENSION"`
	// MaxRows caps the returned rows client-side. 0 uses this server's default.
	MaxRows int `json:"max_rows,omitempty" jsonschema:"optional row cap; the result is truncated with an explicit marker, never silently"`
}

// QueryOutput carries the rows plus the truncation the model must see.
type QueryOutput struct {
	Rows       []map[string]any `json:"rows" jsonschema:"result rows, each a flat column-name to value object"`
	RowCount   int              `json:"row_count" jsonschema:"rows returned after truncation"`
	Bytes      int              `json:"bytes" jsonschema:"approximate encoded size of the returned rows"`
	Truncation Truncation       `json:"truncation" jsonschema:"whether rows were withheld, and how to continue"`
}

// queryAdvice is the one way to continue past a query clamp. It names the two
// levers that actually reduce volume, because `query` has no cursor: narrow the
// projection, or aggregate instead of listing.
const queryAdvice = "narrow the SELECT (fewer columns, a WHERE range, or a GROUP BY aggregate) and re-run; " +
	"this tool has no cursor, so paging is done in SQL with keyset pagination"

// queryBytes is the approximate encoded size of the returned rows, reported so
// a model can size its next request without guessing from the row count.
func queryBytes(rows []map[string]any) int {
	total := 0
	for _, row := range rows {
		if encoded, err := json.Marshal(row); err == nil {
			total += len(encoded)
		}
	}
	return total
}

// Query runs one bounded SELECT and truncates client-side with an explicit
// marker. The proxy's own row and byte limits still apply (413); this cap is
// the volume a model can read.
func (s *Service) query(ctx context.Context, in QueryInput) (*QueryOutput, error) {
	limit := in.MaxRows
	if limit <= 0 {
		limit = s.limits.QueryMaxRows
	}
	if limit > maxQueryRowsCeiling {
		return nil, fmt.Errorf("max_rows must be at most %d", maxQueryRowsCeiling)
	}
	result, err := s.client.querySQL(ctx, in.SQL)
	if err != nil {
		return nil, err
	}
	kept, truncation := clamp(result, limit, "rows", queryAdvice)
	// The row cap is not a volume bound: a short wide row can be larger than a
	// long narrow one, so the encoded-size clamp runs after it and is reported
	// whenever it withheld something. Its total stays the proxy's own row count,
	// so a result cut by both limits cannot read as complete.
	bySize, sizeTruncation := clampRowsToBytes(kept, len(result), s.limits.QueryMaxBytes, queryAdvice)
	if sizeTruncation.Truncated {
		return &QueryOutput{Rows: bySize, RowCount: len(bySize), Bytes: queryBytes(bySize), Truncation: sizeTruncation}, nil
	}
	return &QueryOutput{Rows: kept, RowCount: len(kept), Bytes: queryBytes(kept), Truncation: truncation}, nil
}

// maxQueryRowsCeiling is an internal sanity bound so a typo cannot ask for a
// billion rows. It is not a proxy limit; the proxy enforces its own.
const maxQueryRowsCeiling = 100000

// ExploreInput is a faceted breakdown request.
type ExploreInput struct {
	Dim string `json:"dim" jsonschema:"facet dimension: client, provider, model, conversation, key, status, time, tool or error"`
	Scope
}

// ExploreOutput is one explorer's breakdown.
type ExploreOutput struct {
	Dim                 string           `json:"dim" jsonschema:"the dimension that was broken down"`
	Total               int64            `json:"total" jsonschema:"in-scope requests, excluding the active dimension's own filter"`
	Errors              int64            `json:"error_total" jsonschema:"in-scope requests that are errors"`
	Groups              []map[string]any `json:"groups" jsonschema:"up to 24 groups; percentiles are null below 4 samples"`
	Truncation          Truncation       `json:"truncation" jsonschema:"whether groups were withheld"`
	Rail                map[string]int   `json:"rail" jsonschema:"distinct value count per dimension, following the full filter set"`
	Scope               map[string]int64 `json:"scope" jsonschema:"matches and errors under every filter, including the active dimension's own"`
	ConversationSummary map[string]int   `json:"conversation_summary" jsonschema:"main, sub and unresolved conversation counts"`
}

// Explore returns the faceted breakdown over all matching history.
func (s *Service) explore(ctx context.Context, in ExploreInput) (*ExploreOutput, error) {
	if err := validateDimension(in.Dim); err != nil {
		return nil, err
	}
	if err := in.Scope.Validate(); err != nil {
		return nil, err
	}
	values := in.Scope.merge(url.Values{"dim": []string{in.Dim}})
	var payload struct {
		Dim                 string           `json:"dim"`
		Total               int64            `json:"total"`
		ErrorTotal          int64            `json:"error_total"`
		Groups              []map[string]any `json:"groups"`
		Rail                map[string]int   `json:"rail"`
		Scope               map[string]int64 `json:"scope"`
		ConversationSummary map[string]int   `json:"conversation_summary"`
	}
	// The explorer folds ALL history like the chart, so it is bounded by the
	// same full-history query timeout rather than only by the generic client
	// timeout every call carries.
	queryCtx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()
	if err := s.client.getJSON(queryCtx, routeExplorer, values, &payload); err != nil {
		return nil, err
	}
	groups, truncation := clamp(payload.Groups, explorerMaxGroups, "groups", exploreAdvice)
	return &ExploreOutput{
		Dim:                 payload.Dim,
		Total:               payload.Total,
		Errors:              payload.ErrorTotal,
		Groups:              groups,
		Truncation:          truncation,
		Rail:                object(payload.Rail),
		Scope:               object(payload.Scope),
		ConversationSummary: object(payload.ConversationSummary),
	}, nil
}

func validateDimension(dim string) error {
	for _, known := range Dimensions {
		if dim == known {
			return nil
		}
	}
	return fmt.Errorf("dim %q is unknown; use one of %s", dim, strings.Join(Dimensions, ", "))
}

// exploreAdvice names the one lever that reduces an explorer's group count:
// the filter set. The proxy caps groups at the top of the response, so there is
// no per-call group limit to raise.
const exploreAdvice = "narrow the filter set (add or drop a dim:id filter) and re-run; " +
	"the group cap is the proxy's, so use query with a GROUP BY for a wider breakdown"

// chartAdvice names the two levers for a dropped bucket. A shorter window
// coarsens the same span; the buckets are already server-authoritative, so
// there is no per-call bucket limit to raise.
const chartAdvice = "request a shorter window, or read the series from query grouped on a started_at time bucket"

// snapshotAdvice points at the durable log page, because the snapshot's recent
// records have no cursor of their own.
const snapshotAdvice = "older records come from the records tool, paging with before_ms and before_id"

// prometheusAdvice points at the source of the metric, because a dropped
// exposition line cannot be resumed.
const prometheusAdvice = "ask query for the underlying rows, or read the aggregate the metric is built from"

// ChartInput is a time-series request.
type ChartInput struct {
	// Window is "all" for all history, or a positive integer number of MINUTES.
	// The proxy rejects "007", "+5" and "5.0".
	Window string `json:"window,omitempty" jsonschema:"'all', empty, or a canonical positive integer number of minutes (no sign, padding or decimal)"`
	Scope
}

// ChartOutput is the server-authoritative time series.
type ChartOutput struct {
	NowMs       int64            `json:"now_ms" jsonschema:"server clock at render time, unix milliseconds"`
	FromMs      int64            `json:"from_ms" jsonschema:"clock-aligned window start, unix milliseconds"`
	BucketMs    int64            `json:"bucket_ms" jsonschema:"exact bucket width in milliseconds"`
	TTFTp       []*float64       `json:"ttft_p" jsonschema:"period-wide time-to-first-token percentiles [p50, p95, p99]"`
	TPSp        []*float64       `json:"tps_p" jsonschema:"period-wide generation speed percentiles [p50, p95, p99]"`
	TTFTStat    []*float64       `json:"ttft_stat" jsonschema:"period-wide time-to-first-token [avg, min, max] over every captured sample"`
	TPSStat     []*float64       `json:"tps_stat" jsonschema:"period-wide generation speed [avg, min, max] over every captured sample"`
	CostPerMTok *float64         `json:"cost_per_mtok" jsonschema:"blended USD per million tokens, cost-reporting requests only"`
	Buckets     []map[string]any `json:"buckets" jsonschema:"at most 31 buckets; t is the bucket start in unix milliseconds"`
	Truncation  Truncation       `json:"truncation" jsonschema:"whether buckets were withheld"`
}

// Chart returns the scoped time series. Bucket geometry is server-authoritative
// at the configured cadence; the browser never refolds events into it.
func (s *Service) chart(ctx context.Context, in ChartInput) (*ChartOutput, error) {
	if err := in.Scope.Validate(); err != nil {
		return nil, err
	}
	// An empty window means all history. The proxy defaults to it, but stating
	// it keeps the request self-describing and the tool output honest about
	// what range it answered.
	base := url.Values{"window": []string{windowOrAll(in.Window)}}
	values := in.Scope.merge(base)
	queryCtx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()
	var payload struct {
		NowMs       int64            `json:"now_ms"`
		FromMs      int64            `json:"from_ms"`
		BucketMs    int64            `json:"bucket_ms"`
		TTFTP       []*float64       `json:"ttft_p"`
		TPSP        []*float64       `json:"tps_p"`
		TTFTStat    []*float64       `json:"ttft_stat"`
		TPSStat     []*float64       `json:"tps_stat"`
		CostPerMTok *float64         `json:"cost_per_mtok"`
		Buckets     []map[string]any `json:"buckets"`
	}
	if err := s.client.getJSON(queryCtx, routeChart, values, &payload); err != nil {
		return nil, err
	}
	buckets, truncation := clamp(payload.Buckets, chartMaxBuckets, "buckets", chartAdvice)
	return &ChartOutput{
		NowMs: payload.NowMs, FromMs: payload.FromMs, BucketMs: payload.BucketMs,
		TTFTp: list(payload.TTFTP), TPSp: list(payload.TPSP),
		TTFTStat: list(payload.TTFTStat), TPSStat: list(payload.TPSStat),
		CostPerMTok: payload.CostPerMTok,
		Buckets:     buckets, Truncation: truncation,
	}, nil
}

// windowOrAll makes the chart window explicit: an empty window means all
// history, which is the proxy's own default but is clearer stated.
func windowOrAll(window string) string {
	if window == "" {
		return "all"
	}
	return window
}

// RecordsInput pages the durable newest-first log.
type RecordsInput struct {
	Scope
	// BeforeMs and BeforeID are the paired keyset cursor from a previous page.
	// Send both or neither.
	BeforeMs int64  `json:"before_ms,omitempty" jsonschema:"cursor timestamp in unix milliseconds; must be sent together with before_id"`
	BeforeID string `json:"before_id,omitempty" jsonschema:"cursor record id; must be sent together with before_ms"`
	Limit    int    `json:"limit,omitempty" jsonschema:"page size, 10 to 500; smaller than this server's default is usually better"`
}

// RecordsOutput is one page plus the pagination state that decides whether the
// caller should continue.
type RecordsOutput struct {
	Records  []map[string]any `json:"records" jsonschema:"one durable record per row, newest first"`
	Returned int              `json:"returned" jsonschema:"records in this page"`
	More     bool             `json:"more" jsonschema:"the proxy believes older rows exist"`
	// Exhausted is true when paging further cannot make progress: either the
	// cursor did not advance (the proxy scans a bounded number of rows per
	// call, so a page that cannot advance the cursor is exhaustion, not a
	// reason to loop) or `more` is false. An EMPTY page is deliberately not
	// exhaustion: an empty page with an advanced cursor is the normal shape of
	// "the scan budget ran out before a match", and treating it as the end of
	// history silently drops every match behind it.
	Exhausted    bool           `json:"exhausted" jsonschema:"true when the cursor did not advance or more is false; stop paging"`
	NextBeforeMs int64          `json:"next_before_ms,omitempty" jsonschema:"copy the pair into before_ms and before_id to fetch the next older page"`
	NextBeforeID string         `json:"next_before_id,omitempty" jsonschema:"copy the pair into before_ms and before_id to fetch the next older page"`
	Truncation   Truncation     `json:"truncation" jsonschema:"whether records were withheld"`
	ModelCanon   *ModelCanonMap `json:"model_canon,omitempty" jsonschema:"the raw-to-canonical model name mapping the server applied to this page"`
	Storage      StorageInfo    `json:"storage" jsonschema:"durable storage signal; without it this page is not the whole history"`
	Note         string         `json:"note" jsonschema:"what this page does and does not cover"`
}

// noStorageNote is what the page says when durable storage is off. With db_path
// empty the proxy skips its scan loop entirely and answers an empty page with
// more false, while the in-memory ring still holds recent history: reporting
// that as the end of history would be a confident wrong answer.
const noStorageNote = "durable storage is disabled on this proxy (db_path is empty), so this empty page is NOT the end of history: " +
	"the log route scans nothing. The in-memory ring still holds recent requests; read them with the snapshot tool"

// Records returns one durable page. The caller drives the cursor; this tool
// never loops on its own, and it reports exhaustion explicitly so a caller
// cannot mistake a bounded scan for the end of history.
func (s *Service) records(ctx context.Context, in RecordsInput) (*RecordsOutput, error) {
	if err := in.Scope.Validate(); err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = s.limits.PageSize
	}
	if limit < logPageMin || limit > logPageMax {
		return nil, fmt.Errorf("limit must be %d..%d", logPageMin, logPageMax)
	}
	if (in.BeforeMs == 0) != (in.BeforeID == "") {
		return nil, fmt.Errorf("before_ms and before_id must be sent together, exactly once each")
	}
	storage, err := s.client.storageSignal(ctx)
	if err != nil {
		return nil, err
	}
	page, err := s.client.recordsPage(ctx, in.Scope.query(), in.BeforeMs, in.BeforeID, limit)
	if err != nil {
		return nil, err
	}
	kept, truncation := clamp(page.Records, limit, "records", cursorAdvice)
	advanced := cursorAdvanced(in.BeforeMs, in.BeforeID, page.CursorMs, page.CursorID)
	out := &RecordsOutput{
		Records:      kept,
		Returned:     len(kept),
		More:         page.More,
		Exhausted:    exhausted(page.More, advanced),
		NextBeforeMs: page.CursorMs,
		NextBeforeID: page.CursorID,
		Truncation:   truncation,
		ModelCanon:   page.ModelCanon,
		Storage:      storage,
		Note:         "one newest-first page of DURABLE history; page with the next cursor pair, and stop when exhausted is true",
	}
	if !storage.Enabled {
		out.Note = noStorageNote
	}
	return out, nil
}

// cursorAdvanced reports whether the proxy's returned cursor differs from the
// one that was sent. Identical cursors mean the next call would repeat this
// page exactly.
func cursorAdvanced(sentMs int64, sentID string, gotMs int64, gotID string) bool {
	if sentID == "" {
		return gotID != ""
	}
	return gotMs != sentMs || gotID != sentID
}

// exhausted is the one owner of the paging-stop rule: a page whose cursor does
// not advance is the end of what this scan budget can reach, whatever `more`
// claims, and so is a page the proxy says is the last one.
//
// A page that returned NOTHING is deliberately not exhaustion. The proxy scans a
// bounded number of rows per call and advances its cursor on every row it
// SCANNED, including rows the scope filter excluded, so an empty page with an
// advanced cursor is the ordinary shape of "the budget ran out before a match".
// Its own handler test pins that shape as a page that must be continued: page
// one is zero records with more true, page two returns the rows. A
// non-advancing cursor is already a sufficient loop guard on its own.
func exhausted(more, advanced bool) bool {
	if !advanced {
		return true
	}
	return !more
}

// SnapshotInput reads the live bootstrap snapshot.
type SnapshotInput struct {
	// MaxRecords caps the recent records carried in the response. 0 uses this
	// server's default.
	MaxRecords int `json:"max_records,omitempty" jsonschema:"optional cap on the recent-record array; the payload is truncated with an explicit marker"`
}

// SnapshotOutput is the bootstrap snapshot's readable subset.
type SnapshotOutput struct {
	Seq           int64            `json:"seq" jsonschema:"ring sequence number"`
	FeedID        string           `json:"feed_id" jsonschema:"feed epoch; a change means reconnect for a fresh snapshot"`
	InFlight      int64            `json:"in_flight" jsonschema:"requests currently in flight"`
	TotalRequests int64            `json:"total_requests" jsonschema:"since-inception finalized requests"`
	TotalErrors   int64            `json:"total_errors" jsonschema:"since-inception error count under the canonical predicate"`
	Storage       StorageInfo      `json:"storage" jsonschema:"durable storage signal; captures and paging need it"`
	Records       []map[string]any `json:"records" jsonschema:"most recent finalized records, newest first"`
	Truncation    Truncation       `json:"truncation" jsonschema:"whether records were withheld"`
	OlderHistory  string           `json:"older_history" jsonschema:"how to reach records older than this snapshot"`
}

// Snapshot returns the live bootstrap snapshot. The full payload is capped at
// 8 * dash_log_rows records when durable storage backs the ring, so older
// history needs the durable log page.
func (s *Service) snapshot(ctx context.Context, in SnapshotInput) (*SnapshotOutput, error) {
	limit := in.MaxRecords
	if limit <= 0 {
		limit = s.limits.PageSize
	}
	var payload bootstrapSnapshot
	if err := s.client.getJSON(ctx, routeBootstrap, nil, &payload); err != nil {
		return nil, err
	}
	kept, truncation := clamp(payload.Records, limit, "records", snapshotAdvice)
	return &SnapshotOutput{
		Seq:           payload.Seq,
		FeedID:        payload.Feed,
		InFlight:      payload.Counters.InFlight,
		TotalRequests: payload.Counters.TotalReq,
		TotalErrors:   payload.Counters.TotalErr,
		Storage: StorageInfo{
			Enabled:        payload.Storage.Enabled,
			Dropped:        payload.Storage.Dropped,
			TotalsDegraded: payload.Storage.TotalsDegraded,
		},
		Records:      kept,
		Truncation:   truncation,
		OlderHistory: "records older than this window come from the records tool, paging with before_ms and before_id",
	}, nil
}

// PrometheusInput takes no arguments.
type PrometheusInput struct{}

// PrometheusOutput is the text exposition, bounded for a model.
type PrometheusOutput struct {
	Text       string     `json:"text" jsonschema:"Prometheus text exposition, truncated with an explicit marker"`
	Lines      int        `json:"lines" jsonschema:"lines in the returned text"`
	Truncation Truncation `json:"truncation" jsonschema:"whether exposition lines were withheld"`
}

// Prometheus returns the scrape text. It covers the in-memory ring, NOT the
// durable since-inception totals the dashboard aggregates use.
func (s *Service) prometheus(ctx context.Context, _ PrometheusInput) (*PrometheusOutput, error) {
	body, err := s.client.getText(ctx, routePrometheus, nil)
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(body), "\n")
	lines := strings.Split(text, "\n")
	kept, truncation := clamp(lines, prometheusMaxLines, "exposition lines", prometheusAdvice)
	if len(kept) == 1 && kept[0] == "" {
		kept = nil
	}
	return &PrometheusOutput{Text: strings.Join(kept, "\n"), Lines: len(kept), Truncation: truncation}, nil
}

// prometheusMaxLines bounds the exposition a model reads. An internal guardrail
// on output volume, not a proxy limit.
const prometheusMaxLines = 400

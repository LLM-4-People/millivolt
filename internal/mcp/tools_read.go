package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
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
	Truncation Truncation       `json:"truncation" jsonschema:"whether rows were withheld, and how to continue"`
	Storage    bool             `json:"storage_enabled" jsonschema:"false when durable storage is off and this surface cannot work"`
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
	kept, truncation := clamp(result, limit, "rows")
	return &QueryOutput{Rows: kept, RowCount: len(kept), Truncation: truncation, Storage: true}, nil
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
	if err := s.client.getJSON(ctx, routeExplorer, values, &payload); err != nil {
		return nil, err
	}
	groups, truncation := clamp(payload.Groups, explorerMaxGroups, "groups")
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
	TTFTp       []*float64       `json:"ttft_p" jsonschema:"period-wide time-to-first-token percentiles"`
	TPSp        []*float64       `json:"tps_p" jsonschema:"period-wide generation speed percentiles"`
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
		CostPerMTok *float64         `json:"cost_per_mtok"`
		Buckets     []map[string]any `json:"buckets"`
	}
	if err := s.client.getJSON(queryCtx, routeChart, values, &payload); err != nil {
		return nil, err
	}
	buckets, truncation := clamp(payload.Buckets, chartMaxBuckets, "buckets")
	return &ChartOutput{
		NowMs: payload.NowMs, FromMs: payload.FromMs, BucketMs: payload.BucketMs,
		TTFTp: list(payload.TTFTP), TPSp: list(payload.TPSP), CostPerMTok: payload.CostPerMTok,
		Buckets: buckets, Truncation: truncation,
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
	// Exhausted is true when paging further cannot make progress, even if More
	// is true: the proxy scans a bounded number of rows per call, so a short or
	// empty page with a non-advancing cursor is exhaustion, not a reason to
	// loop.
	Exhausted    bool       `json:"exhausted" jsonschema:"true when the cursor cannot advance; stop paging"`
	NextBeforeMs int64      `json:"next_before_ms,omitempty" jsonschema:"pass both next cursor fields to fetch the next older page"`
	NextBeforeID string     `json:"next_before_id,omitempty" jsonschema:"pass both next cursor fields to fetch the next older page"`
	Truncation   Truncation `json:"truncation" jsonschema:"whether records were withheld"`
	ModelCanon   any        `json:"model_canon,omitempty" jsonschema:"raw-to-canonical model name mapping the server applied"`
}

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
	page, err := s.client.recordsPage(ctx, in.Scope.query(), in.BeforeMs, in.BeforeID, limit)
	if err != nil {
		return nil, err
	}
	kept, truncation := clamp(page.Records, limit, "records")
	advanced := cursorAdvanced(in.BeforeMs, in.BeforeID, page.CursorMs, page.CursorID)
	return &RecordsOutput{
		Records:      kept,
		Returned:     len(kept),
		More:         page.More,
		Exhausted:    exhausted(page.More, advanced, len(kept)),
		NextBeforeMs: page.CursorMs,
		NextBeforeID: page.CursorID,
		Truncation:   truncation,
	}, nil
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

// exhausted is the one owner of the non-advancing-cursor rule: a page that
// cannot advance the cursor, or that returned nothing, is the end of what this
// scan budget can reach, whatever `more` claims.
func exhausted(more, advanced bool, returned int) bool {
	if returned == 0 || !advanced {
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
	kept, truncation := clamp(payload.Records, limit, "records")
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
	kept, truncation := clamp(lines, prometheusMaxLines, "exposition lines")
	if len(kept) == 1 && kept[0] == "" {
		kept = nil
	}
	return &PrometheusOutput{Text: strings.Join(kept, "\n"), Lines: len(kept), Truncation: truncation}, nil
}

// prometheusMaxLines bounds the exposition a model reads. An internal guardrail
// on output volume, not a proxy limit.
const prometheusMaxLines = 400

// fmtDuration renders a duration for a tool message.
func fmtDuration(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s" }

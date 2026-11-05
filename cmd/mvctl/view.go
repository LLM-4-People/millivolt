package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func cmdStatus(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl status"); err != nil {
		return exitUsage(err)
	}
	healthCtx, cancel := ctx.bounded()
	_, healthErr := ctx.C.GetText(healthCtx, mcp.RouteHealthz, nil)
	cancel()

	raw, snapshot, err := ctx.bootstrap()
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	pretty := heading("millivolt status") + "\n" + kv([][2]string{
		{"state", stateText(healthErr == nil)},
		{"url", ctx.C.Origin()},
		{"dashboard build", shortVersion(snapshot.DashboardVersion)},
		{"requests", num(snapshot.KPI.Requests)},
		{"errors", num(snapshot.KPI.Errors)},
		{"in flight", num(snapshot.KPI.InFlight)},
		{"cost", cost(snapshot.KPI.Cost)},
		{"tokens", num(snapshot.KPI.InputTokens + snapshot.KPI.OutputTokens)},
		{"storage", storageText(snapshot)},
		{"paused", boolText(snapshot.Pause.Paused)},
	})
	result := Result{Pretty: pretty, Data: bootstrapProjection(raw)}
	if healthErr != nil {
		result.Code = exitFailure
		return result, fmt.Errorf("millivolt /healthz did not answer: %w", healthErr)
	}
	return result, nil
}

func cmdOverview(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl overview"); err != nil {
		return exitUsage(err)
	}
	raw, snapshot, err := ctx.bootstrap()
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	rows := [][]string{
		{"Requests", num(snapshot.KPI.Requests), "Errors", num(snapshot.KPI.Errors)},
		{"In flight", num(snapshot.KPI.InFlight), "Cost", cost(snapshot.KPI.Cost)},
		{"Input tokens", num(snapshot.KPI.InputTokens), "Output tokens", num(snapshot.KPI.OutputTokens)},
		{"Cache read", num(snapshot.KPI.CacheReadTokens), "Reasoning", num(snapshot.KPI.ReasoningTokens)},
		{"Avg TTFT", durPtr(snapshot.KPI.AvgTTFTMs), "Avg TPS", numPtr(snapshot.KPI.AvgTPS)},
		{"Cost / request", costPtr(snapshot.KPI.CostPerReq), "Cost / MTok", costPtr(snapshot.KPI.CostPerMTok)},
	}
	pretty := heading("overview") + "\n" + table([]string{"metric", "value", "metric", "value"}, rows)
	pretty += "\n\n" + kv([][2]string{
		{"storage", storageText(snapshot)},
		{"dropped", num(snapshot.Storage.Dropped)},
		{"pause", boolText(snapshot.Pause.Paused) + fmt.Sprintf(" (%s queued)", num(snapshot.Pause.Queued))},
		{"throttles", num(float64(len(snapshot.Throttle.Throttles)))},
		{"debug", boolText(snapshot.Debug.Enabled)},
		{"dashboard build", shortVersion(snapshot.DashboardVersion)},
	})
	if snapshot.Storage.TotalsDegraded {
		pretty += "\n" + yellow("Totals are degraded: the boot scan did not finish, so headline numbers may be short.")
	}
	return Result{Pretty: pretty, Data: bootstrapProjection(raw)}, nil
}

func (c *Context) bootstrap() (map[string]any, Snapshot, error) {
	reqCtx, cancel := c.bounded()
	defer cancel()
	var raw map[string]any
	if err := c.C.GetJSON(reqCtx, mcp.RouteBootstrap, nil, &raw); err != nil {
		return nil, Snapshot{}, err
	}
	snapshot, err := decodeSnapshot(raw)
	if err != nil {
		return nil, Snapshot{}, fmt.Errorf("decode /metrics/bootstrap: %w", err)
	}
	return raw, snapshot, nil
}

// bootstrapProjection is the credential-safe state view status and overview
// print with --json. Request rows are omitted entirely: each carries prompt and
// response previews, response headers and retry attempts, and the dedicated
// requests command already reduces a row to its safe subset. Dash is the full
// effective config; config get is the one command that serves it. A new
// top-level bootstrap field must be added here deliberately, so an unknown
// future section cannot become an unreviewed passthrough.
func bootstrapProjection(raw map[string]any) map[string]any {
	keys := []string{
		"kpi", "storage", "pause", "throttle", "debug", "storm", "restart",
		"dashboard_version", "data_from_ms", "counters", "seq", "feed_id",
		"incremental", "pending_revision", "rate_limit_classes",
	}
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			out[key] = value
		}
	}
	return out
}

func cmdRequests(ctx *Context) (Result, error) {
	if err := ctx.requireArgs(1, "mvctl requests [N]"); err != nil {
		return exitUsage(err)
	}
	limit := ctx.G.Settings.RequestsPage
	if len(ctx.Args) == 1 {
		n, err := strconv.Atoi(ctx.Args[0])
		if err != nil {
			return exitUsage(fmt.Errorf("limit must be an integer"))
		}
		limit = n
	}
	if limit < config.DashLogRowsMin || limit > config.DashLogRowsMax {
		return exitUsage(fmt.Errorf("limit must be %d..%d", config.DashLogRowsMin, config.DashLogRowsMax))
	}
	beforeSet := ctx.Flags.Present("before-ms") || ctx.Flags.Present("before-id")
	if beforeSet && !(ctx.Flags.Present("before-ms") && ctx.Flags.Present("before-id")) {
		return exitUsage(errors.New("--before-ms and --before-id must be passed together"))
	}
	var beforeMs *int64
	var beforeID *string
	if beforeSet {
		value := ctx.Flags.Int("before-ms", 0)
		if value <= 0 {
			return exitUsage(errors.New("--before-ms must be greater than zero"))
		}
		id := ctx.Flags.String("before-id", "")
		beforeMs, beforeID = &value, &id
	}
	query := mcp.LogPageQuery(limit, beforeMs, beforeID)
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var page RequestPage
	if err := ctx.C.GetJSON(reqCtx, mcp.RouteLog, query, &page); err != nil {
		return Result{Code: exitFailure}, err
	}
	rows := make([][]string, 0, len(page.Records))
	for _, record := range page.Records {
		errorText := ""
		if record.ErrorType != "" {
			errorText = "!" + record.ErrorType
		} else if record.StatusCode >= 400 {
			errorText = "!" + strconv.Itoa(record.StatusCode)
		}
		rows = append(rows, []string{
			shortTime(record.Start), statusCode(record.StatusCode),
			truncate(record.Provider+"/"+record.Model, 34), truncate(record.Client, 16),
			dur(record.DurationMs), num(float64(record.Usage.InputTokens + record.Usage.OutputTokens)), errorText,
		})
	}
	pretty := heading(fmt.Sprintf("recent requests (%d)", len(page.Records)))
	if len(rows) > 0 {
		pretty += "\n" + table([]string{"time", "status", "provider/model", "client", "duration", "tokens", "error"}, rows)
	} else {
		pretty += "\n" + dim("No requests recorded. A ring-only instance keeps live rows only while they age out; enable storage for durable history.")
	}
	if page.More && page.CursorMs != nil {
		pretty += "\n" + dim(fmt.Sprintf("more available; next page: --before-ms %d --before-id %s",
			int64(*page.CursorMs), sanitizeCell(page.CursorID)))
	}
	return Result{Pretty: pretty, Data: page}, nil
}

func cmdQuery(ctx *Context) (Result, error) {
	sql := strings.Join(ctx.Args, " ")
	if strings.TrimSpace(sql) == "" {
		return exitUsage(errors.New(`usage: mvctl query "SELECT ..."`))
	}
	queryTimeout := ctx.G.Settings.QueryTimeout
	if ctx.Flags.Present("query-timeout") {
		queryTimeout = ctx.Flags.Duration("query-timeout", queryTimeout)
		if queryTimeout <= 0 {
			return exitUsage(errors.New("--query-timeout must be greater than zero"))
		}
	}
	reqCtx, cancel := ctx.boundedFor(queryTimeout)
	defer cancel()
	values, err := mcp.QueryValues(sql)
	if err != nil {
		return exitUsage(err)
	}
	var rows any
	if err := ctx.C.GetJSON(reqCtx, mcp.RouteQuery, values, &rows); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: prettyJSON(rows), Data: rows}, nil
}

func cmdPrometheus(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl prometheus"); err != nil {
		return exitUsage(err)
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	text, err := ctx.C.GetText(reqCtx, mcp.RoutePrometheus, nil)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	exposition := sanitizeText(strings.TrimRight(string(text), "\n"))
	return Result{Pretty: exposition, Data: map[string]any{"exposition": exposition}}, nil
}

func cmdWatch(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl watch"); err != nil {
		return exitUsage(err)
	}
	samples := ctx.G.Settings.WatchSamples
	if ctx.Flags.Present("samples") {
		samples = int(ctx.Flags.Int("samples", int64(samples)))
	}
	interval := ctx.G.Settings.WatchInterval
	if ctx.Flags.Present("interval") {
		interval = ctx.Flags.Duration("interval", interval)
	}
	if samples < ctx.G.Settings.WatchSamplesMin || samples > ctx.G.Settings.WatchSamplesMax {
		return exitUsage(fmt.Errorf("--samples must be %d..%d", ctx.G.Settings.WatchSamplesMin, ctx.G.Settings.WatchSamplesMax))
	}
	if interval <= 0 {
		return exitUsage(errors.New("--interval must be greater than zero"))
	}
	var (
		series []float64
		points []map[string]any
		last   Snapshot
		seen   bool
	)
	for i := 0; i < samples; i++ {
		if i > 0 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			if ctx.Ctx.Err() != nil {
				return Result{Pretty: watchPretty(series, last, seen), Data: watchData(series, points)}, context.Canceled
			}
		}
		_, snapshot, err := ctx.bootstrap()
		if err != nil {
			return Result{Code: exitFailure}, err
		}
		if i > 0 && snapshot.KPI.Requests >= last.KPI.Requests {
			series = append(series, snapshot.KPI.Requests-last.KPI.Requests)
		}
		last, seen = snapshot, true
		points = append(points, map[string]any{
			"at_ms":     time.Now().UnixMilli(),
			"requests":  snapshot.KPI.Requests,
			"errors":    snapshot.KPI.Errors,
			"in_flight": snapshot.KPI.InFlight,
			"cost":      snapshot.KPI.Cost,
		})
	}
	return Result{Pretty: watchPretty(series, last, seen), Data: watchData(series, points)}, nil
}

func watchPretty(series []float64, snapshot Snapshot, seen bool) string {
	pretty := heading("watch") + "\n" + spark(series, 48)
	if !seen {
		return pretty
	}
	return pretty + fmt.Sprintf("\nrequests %s  errors %s  in flight %s  cost %s",
		num(snapshot.KPI.Requests), num(snapshot.KPI.Errors),
		num(snapshot.KPI.InFlight), cost(snapshot.KPI.Cost))
}

func watchData(series []float64, points []map[string]any) map[string]any {
	return map[string]any{"series": series, "samples": points}
}

func cmdURL(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl url"); err != nil {
		return exitUsage(err)
	}
	origin, err := mcp.NormalizeProxyURL(ctx.G.BaseURL)
	if err != nil {
		return exitUsage(err)
	}
	return Result{Pretty: origin.String(), Data: map[string]any{"url": origin.String()}}, nil
}

func cmdHealth(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl health"); err != nil {
		return exitUsage(err)
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	if _, err := ctx.C.GetText(reqCtx, mcp.RouteHealthz, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{
		Pretty: green("healthy") + " " + ctx.C.Origin(),
		Data:   map[string]any{"healthy": true, "url": ctx.C.Origin()},
	}, nil
}

func stateText(healthy bool) string {
	if healthy {
		return green("up")
	}
	return red("unhealthy")
}

func boolText(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func shortVersion(value string) string {
	if value == "" {
		return "unknown"
	}
	if len(value) > 12 {
		return value[:12] + "..."
	}
	return value
}

func storageText(snapshot Snapshot) string {
	if !snapshot.Storage.Enabled {
		return "ring only"
	}
	if snapshot.Storage.Dropped > 0 {
		return fmt.Sprintf("durable, %s dropped", num(snapshot.Storage.Dropped))
	}
	return "durable"
}

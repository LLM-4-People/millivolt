package main

// History deletion. Every filter is parsed before the count call; an
// unparsable or zero value is a usage error rather than an omitted constraint.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func cmdPurge(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl purge [filters] [--all] [--yes]"); err != nil {
		return exitUsage(err)
	}
	filter, err := purgeFilter(ctx)
	if err != nil {
		return exitUsage(err)
	}
	all := ctx.Flags.Bool("all", false)
	if all && !filter.IsEmpty() {
		return exitUsage(errors.New("--all cannot be combined with a filter"))
	}
	if !all && filter.IsEmpty() {
		return exitUsage(errors.New("refusing an unfiltered purge; add a filter or pass --all"))
	}
	document := filter.Document()
	var countBody any = document
	if all {
		countBody = nil
	}
	count, err := purgeCount(ctx, countBody)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	preview := heading("purge preview") + "\n" + kv([][2]string{
		{"rows", displayValue(count)},
		{"filter", displayValue(document)},
		{"scope", purgeScope(all)},
	})
	result := Result{
		Pretty: preview,
		Data:   map[string]any{"preview": count, "filter": document, "applied": false},
	}
	if !ctx.G.Yes {
		result.Pretty += "\n\n" + yellow("Preview only. Re-run with --yes to delete.")
		return result, nil
	}
	// The delete is authorized by this run's preview, not by a count a previous
	// invocation saw. Re-count immediately before deleting and refuse when the
	// population moved: the operator confirmed the rows they were shown.
	recount, err := purgeCount(ctx, countBody)
	if err != nil {
		return Result{Code: exitFailure, Data: result.Data}, fmt.Errorf("re-count before deleting: %w", err)
	}
	if recount != count {
		return Result{Code: exitFailure, Data: result.Data}, fmt.Errorf(
			"the row count changed from %d to %d since the preview; re-run the preview and confirm again", count, recount)
	}
	deleteCtx, cancelDelete := ctx.bounded()
	defer cancelDelete()
	var deleteBody any = document
	if all {
		deleteBody = nil
	}
	var out map[string]any
	if _, err := ctx.C.PostJSON(deleteCtx, mcp.RoutePurge, nil, deleteBody, &out, nil); err != nil {
		return Result{Code: 1, Data: result.Data}, err
	}
	result.Pretty = preview + "\n\n" + green("history purged") + "\n" + prettyJSON(out)
	result.Data = map[string]any{"preview": count, "filter": document, "applied": true, "result": out}
	return result, nil
}

func purgeFilter(ctx *Context) (mcp.PurgeFilterInput, error) {
	var filter mcp.PurgeFilterInput
	for _, item := range []struct {
		flag   string
		target *string
	}{
		{"client", &filter.Client},
		{"provider", &filter.Provider},
		{"model", &filter.Model},
		{"conversation-id", &filter.ConversationID},
		{"error-type", &filter.ErrorType},
	} {
		value := strings.TrimSpace(ctx.Flags.String(item.flag, ""))
		if value == "" {
			if ctx.Flags.Present(item.flag) {
				return filter, fmt.Errorf("--%s requires a value", item.flag)
			}
			continue
		}
		*item.target = value
	}
	if ctx.Flags.Present("status") {
		status := ctx.Flags.Int("status", 0)
		if status <= 0 {
			return filter, errors.New("--status must be greater than zero; 0 means no filter and would broaden the purge")
		}
		filter.StatusCode = int(status)
	}
	for _, item := range []struct {
		flag   string
		target *int64
	}{
		{"before-ms", &filter.BeforeMs},
		{"after-ms", &filter.AfterMs},
	} {
		if !ctx.Flags.Present(item.flag) {
			continue
		}
		value := ctx.Flags.Int(item.flag, 0)
		if value <= 0 {
			return filter, fmt.Errorf("--%s must be greater than zero; 0 means no filter and would broaden the purge", item.flag)
		}
		*item.target = value
	}
	if ctx.Flags.Bool("has-error", false) {
		filter.HasError = true
	}
	if ctx.Flags.Bool("debug", false) {
		filter.HasDebug = true
	}
	if err := filter.Validate(); err != nil {
		return filter, err
	}
	return filter, nil
}

func purgeScope(all bool) string {
	if all {
		return "every recorded request"
	}
	return "requests matching every supplied filter"
}

// purgeCount reads the one number a purge preview or delete is authorized
// against. A missing, non-numeric, fractional, negative or non-finite value is
// an error, never zero: the count is the operator's only evidence of scope.
func purgeCount(ctx *Context, body any) (int64, error) {
	countCtx, cancel := ctx.bounded()
	defer cancel()
	var document map[string]any
	if _, err := ctx.C.PostJSON(countCtx, mcp.RoutePurgeCount, nil, body, &document, nil); err != nil {
		return 0, err
	}
	value, ok := document["count"]
	if !ok {
		return 0, errors.New("the proxy did not report a purge row count")
	}
	var count float64
	switch typed := value.(type) {
	case float64:
		count = typed
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, fmt.Errorf("the proxy reported an unreadable purge row count %q", typed)
		}
		count = parsed
	default:
		return 0, fmt.Errorf("the proxy reported a purge row count of type %T, want a number", value)
	}
	if math.IsNaN(count) || math.IsInf(count, 0) || count < 0 || count != math.Trunc(count) || count > math.MaxInt64 {
		return 0, fmt.Errorf("the proxy reported an invalid purge row count %v", count)
	}
	return int64(count), nil
}

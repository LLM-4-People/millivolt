package main

// Provider limits, quota gates and queue drains. Each command transports one
// proxy-owned grammar and guards the action with --yes.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func cmdThrottle(ctx *Context) (Result, error) {
	if len(ctx.Args) > 1 {
		return exitUsage(errors.New("usage: mvctl throttle [show|set|clear] --provider X"))
	}
	sub := "show"
	if len(ctx.Args) == 1 {
		sub = ctx.Args[0]
	}
	switch sub {
	case "show":
		if err := ctx.rejectFlags("provider", "concurrency", "requests", "request-window", "tokens", "token-window", "anti-burst", "ramp-duration"); err != nil {
			return exitUsage(err)
		}
		return throttleShow(ctx)
	case "set", "clear":
		return throttleChange(ctx, sub)
	default:
		return exitUsage(fmt.Errorf("unknown throttle subcommand %q", sub))
	}
}

func throttleShow(ctx *Context) (Result, error) {
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var raw map[string]any
	if err := ctx.C.GetJSON(reqCtx, mcp.RouteThrottle, nil, &raw); err != nil {
		return Result{Code: exitFailure}, err
	}
	pretty := heading("throttle") + "\n"
	throttles, _ := raw["throttles"].([]any)
	if len(throttles) == 0 {
		pretty += dim("no active provider limits")
	} else {
		rows := make([][]string, 0, len(throttles))
		for _, item := range throttles {
			limit, _ := item.(map[string]any)
			rows = append(rows, []string{
				displayValue(limit["provider"]), displayValue(limit["concurrency"]),
				displayValue(limit["requests"]), displayValue(limit["request_window"]),
				displayValue(limit["tokens"]), displayValue(limit["token_window"]),
				displayValue(limit["anti_burst"]), displayValue(limit["ramp_duration"]),
				displayValue(limit["in_flight"]), displayValue(limit["queued"]),
			})
		}
		pretty += table([]string{"provider", "concurrency", "requests", "window", "tokens", "token window", "anti-burst", "ramp", "in flight", "queued"}, rows)
	}
	return Result{Pretty: pretty, Data: raw}, nil
}

func throttleChange(ctx *Context, sub string) (Result, error) {
	provider := strings.TrimSpace(ctx.Flags.String("provider", ""))
	if provider == "" {
		return exitUsage(errors.New("--provider is required"))
	}
	throttle := mcp.SetThrottleInput{Provider: provider, Clear: sub == "clear"}
	if sub == "clear" {
		if err := ctx.rejectFlags("concurrency", "requests", "request-window", "tokens", "token-window", "anti-burst", "ramp-duration"); err != nil {
			return exitUsage(err)
		}
	} else {
		if ctx.Flags.Present("concurrency") {
			value := int(ctx.Flags.Int("concurrency", 0))
			throttle.Concurrency = &value
		}
		if ctx.Flags.Present("requests") {
			value := ctx.Flags.Int("requests", 0)
			throttle.Requests = &value
		}
		if ctx.Flags.Present("tokens") {
			value := ctx.Flags.Int("tokens", 0)
			throttle.Tokens = &value
		}
		for _, window := range []struct {
			flag   string
			target *string
		}{
			{"request-window", &throttle.RequestWindow},
			{"token-window", &throttle.TokenWindow},
		} {
			if !ctx.Flags.Present(window.flag) {
				continue
			}
			value := strings.TrimSpace(ctx.Flags.String(window.flag, ""))
			if value == "" {
				return exitUsage(fmt.Errorf("--%s requires a duration", window.flag))
			}
			*window.target = value
		}
		if ctx.Flags.Present("ramp-duration") {
			value := strings.TrimSpace(ctx.Flags.String("ramp-duration", ""))
			if value == "" {
				return exitUsage(errors.New("--ramp-duration requires a duration or an off spelling"))
			}
			throttle.RampDuration = &value
		}
		if ctx.Flags.Present("anti-burst") {
			value := ctx.Flags.Bool("anti-burst", false)
			throttle.AntiBurst = &value
		}
	}
	// Local validation is a usage error; the network-backed checks below are
	// failures. Keeping the two apart preserves exit 2 for a malformed flag set
	// even when the proxy is unreachable.
	if _, err := throttle.Document(); err != nil {
		return exitUsage(err)
	}
	// The preview performs the same provider guard and window-only stored-count
	// read the send does, so --yes cannot reveal a problem the preview did not
	// mention.
	guardCtx, cancelGuard := ctx.bounded()
	body, err := ctx.C.PrepareThrottle(guardCtx, throttle)
	cancelGuard()
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	action := sub + " the throttle for " + provider
	if result, proceed, err := guarded(ctx, action, body); !proceed {
		result.Pretty += "\n" + prettyJSON(body)
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	out, err := ctx.C.ApplyThrottle(reqCtx, throttle)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: green("throttle updated") + "\n" + prettyJSON(out), Data: out}, nil
}

// ---------------------------------------------------------------------------
// quota
// ---------------------------------------------------------------------------

func cmdQuota(ctx *Context) (Result, error) {
	if len(ctx.Args) > 1 {
		return exitUsage(errors.New("usage: mvctl quota [show|resume --provider X]"))
	}
	sub := "show"
	if len(ctx.Args) == 1 {
		sub = ctx.Args[0]
	}
	switch sub {
	case "show":
		if err := ctx.rejectFlags("provider"); err != nil {
			return exitUsage(err)
		}
		reqCtx, cancel := ctx.bounded()
		defer cancel()
		var raw map[string]any
		if err := ctx.C.GetJSON(reqCtx, mcp.RouteQuota, nil, &raw); err != nil {
			return Result{Code: exitFailure}, err
		}
		return Result{Pretty: heading("quota / storm") + "\n" + prettyJSON(raw), Data: raw}, nil
	case "resume":
		provider := strings.TrimSpace(ctx.Flags.String("provider", ""))
		if provider == "" {
			return exitUsage(errors.New("--provider is required"))
		}
		body, err := (mcp.ResumeQuotaInput{Provider: provider}).Document()
		if err != nil {
			return exitUsage(err)
		}
		if result, proceed, err := guarded(ctx, "resume the quota gate for "+provider, body); !proceed {
			result.Pretty += "\n" + prettyJSON(body)
			return result, err
		}
		reqCtx, cancel := ctx.bounded()
		defer cancel()
		var out map[string]any
		if _, err := ctx.C.PostJSON(reqCtx, mcp.RouteQuota, nil, body, &out, nil); err != nil {
			return Result{Code: exitFailure}, err
		}
		return Result{Pretty: green("quota gate resumed") + "\n" + prettyJSON(out), Data: out}, nil
	default:
		return exitUsage(fmt.Errorf("unknown quota subcommand %q", sub))
	}
}

// ---------------------------------------------------------------------------
// drain
// ---------------------------------------------------------------------------

func cmdDrain(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl drain --all|--provider X|--model X|--client X"); err != nil {
		return exitUsage(err)
	}
	body := map[string]any{}
	scope := 0
	for _, key := range []string{"provider", "model", "client"} {
		if value := strings.TrimSpace(ctx.Flags.String(key, "")); value != "" {
			body[key] = value
			scope++
		} else if ctx.Flags.Present(key) {
			return exitUsage(fmt.Errorf("--%s requires a value", key))
		}
	}
	all := ctx.Flags.Bool("all", false)
	if all && scope > 0 {
		return exitUsage(errors.New("--all cannot be combined with --provider, --model or --client"))
	}
	if all {
		body["all"] = true
		scope++
	}
	if scope != 1 {
		return exitUsage(errors.New("exactly one of --all, --provider, --model or --client is required"))
	}
	if result, proceed, err := guarded(ctx, "drain the request queue", body); !proceed {
		result.Pretty += "\n" + prettyJSON(body)
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var out map[string]any
	if _, err := ctx.C.PostJSON(reqCtx, mcp.RouteDrain, nil, body, &out, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: green("queue drained") + "\n" + prettyJSON(out), Data: out}, nil
}

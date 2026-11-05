package main

// Pause holds and debug capture share the operator's identity model: a hold or
// session is addressed by id, never by a scope that looks selective but clears
// or captures everything.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func cmdPause(ctx *Context) (Result, error) {
	if len(ctx.Args) > 1 {
		return exitUsage(errors.New("usage: mvctl pause [show|on|off]"))
	}
	sub := "show"
	if len(ctx.Args) == 1 {
		sub = ctx.Args[0]
	}
	switch sub {
	case "show":
		if err := ctx.rejectFlagsExcept(); err != nil {
			return exitUsage(err)
		}
		return pauseShow(ctx)
	case "on":
		return pauseOn(ctx)
	case "off":
		return pauseOff(ctx)
	default:
		return exitUsage(fmt.Errorf("unknown pause subcommand %q", sub))
	}
}

func pauseShow(ctx *Context) (Result, error) {
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var raw map[string]any
	if err := ctx.C.GetJSON(reqCtx, mcp.RoutePause, nil, &raw); err != nil {
		return Result{Code: exitFailure}, err
	}
	pretty := heading("pause") + "\n" + kv([][2]string{
		{"paused", boolText(boolField(raw, "paused"))},
		{"queued", displayValue(raw["queued"])},
		{"default max queued", displayValue(raw["default_max_queued"])},
		{"until", displayValue(raw["until"])},
	})
	if holds, ok := raw["holds"].([]any); ok && len(holds) > 0 {
		rows := make([][]string, 0, len(holds))
		for _, item := range holds {
			hold, _ := item.(map[string]any)
			rows = append(rows, []string{
				displayValue(hold["id"]), displayValue(hold["all"]), displayValue(hold["new"]),
				displayValue(hold["clients"]), displayValue(hold["providers"]),
				displayValue(hold["duration"]), displayValue(hold["until"]),
				displayValue(hold["queued"]), displayValue(hold["reason"]),
			})
		}
		pretty += "\n" + table([]string{"id", "all", "new", "clients", "providers", "duration", "until", "queued", "reason"}, rows)
	}
	return Result{Pretty: pretty, Data: raw}, nil
}

func pauseOn(ctx *Context) (Result, error) {
	clients, err := splitNames(ctx.Flags.String("client", ""))
	if err != nil {
		return exitUsage(fmt.Errorf("--client: %w", err))
	}
	providers, err := splitNames(ctx.Flags.String("provider", ""))
	if err != nil {
		return exitUsage(fmt.Errorf("--provider: %w", err))
	}
	all := ctx.Flags.Bool("all", false)
	newOnly := ctx.Flags.Bool("new", false)
	id := ctx.Flags.String("id", "")
	if all && (newOnly || len(clients) > 0 || len(providers) > 0) {
		return exitUsage(errors.New("--all cannot be combined with --new, --client or --provider"))
	}
	if newOnly && (len(clients) > 0 || len(providers) > 0) {
		return exitUsage(errors.New("--new cannot be combined with --client or --provider"))
	}
	if id == "" && !all && !newOnly && len(clients) == 0 && len(providers) == 0 {
		return exitUsage(errors.New("pause on requires --client, --provider, --new or --all"))
	}
	pause := mcp.SetPauseInput{
		Paused: true, HoldID: id, All: all, New: newOnly,
		Clients: clients, Providers: providers,
	}
	if ctx.Flags.Present("duration") {
		value := ctx.Flags.String("duration", "")
		pause.Duration = &value
	}
	if ctx.Flags.Present("max-queued") {
		value := int(ctx.Flags.Int("max-queued", 0))
		pause.MaxQueued = &value
	}
	body, err := pause.Document()
	if err != nil {
		return exitUsage(err)
	}
	action := "pause " + pauseScope(clients, providers, all, newOnly)
	if id != "" {
		action = "edit pause hold " + id
	}
	if result, proceed, err := guarded(ctx, action, body); !proceed {
		result.Pretty += "\n" + prettyJSON(body)
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var out map[string]any
	if _, err := ctx.C.PostJSON(reqCtx, mcp.RoutePause, nil, body, &out, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: green("pause updated") + "\n" + prettyJSON(out), Data: out}, nil
}

func pauseOff(ctx *Context) (Result, error) {
	if err := ctx.rejectFlagsExcept("id", "all"); err != nil {
		return exitUsage(err)
	}
	id := ctx.Flags.String("id", "")
	all := ctx.Flags.Bool("all", false)
	if id == "" && !all {
		return exitUsage(errors.New("pause off requires --id or --all; scope flags do not identify a hold on the proxy"))
	}
	if id != "" && all {
		return exitUsage(errors.New("--id and --all are mutually exclusive"))
	}
	body, err := (mcp.SetPauseInput{Paused: false, HoldID: id}).Document()
	if err != nil {
		return exitUsage(err)
	}
	action := "resume every pause hold"
	if id != "" {
		action = "resume pause hold " + id
	}
	if result, proceed, err := guarded(ctx, action, body); !proceed {
		result.Pretty += "\n" + prettyJSON(body)
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var out map[string]any
	if _, err := ctx.C.PostJSON(reqCtx, mcp.RoutePause, nil, body, &out, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: green("pause updated") + "\n" + prettyJSON(out), Data: out}, nil
}

func pauseScope(clients, providers []string, all, newOnly bool) string {
	switch {
	case all:
		return "every client and provider"
	case newOnly:
		return "new clients only"
	case len(clients) > 0 && len(providers) > 0:
		return "clients " + strings.Join(clients, ",") + " and providers " + strings.Join(providers, ",")
	case len(clients) > 0:
		return "clients " + strings.Join(clients, ",")
	default:
		return "providers " + strings.Join(providers, ",")
	}
}

func cmdDebug(ctx *Context) (Result, error) {
	if len(ctx.Args) > 1 {
		return exitUsage(errors.New("usage: mvctl debug [show|on|off]"))
	}
	sub := "show"
	if len(ctx.Args) == 1 {
		sub = ctx.Args[0]
	}
	switch sub {
	case "show":
		if err := ctx.rejectFlagsExcept(); err != nil {
			return exitUsage(err)
		}
		reqCtx, cancel := ctx.bounded()
		defer cancel()
		var raw map[string]any
		if err := ctx.C.GetJSON(reqCtx, mcp.RouteDebug, nil, &raw); err != nil {
			return Result{Code: exitFailure}, err
		}
		pretty := heading("debug capture") + "\n" + kv([][2]string{
			{"enabled", boolText(boolField(raw, "enabled"))},
			{"ttl", displayValue(raw["ttl"])},
			{"max bytes", displayValue(raw["max_bytes"])},
		})
		if sessions, ok := raw["sessions"].([]any); ok && len(sessions) > 0 {
			rows := make([][]string, 0, len(sessions))
			for _, item := range sessions {
				session, _ := item.(map[string]any)
				rows = append(rows, []string{
					displayValue(session["id"]), displayValue(session["clients"]),
					displayValue(session["providers"]), displayValue(session["models"]),
					displayValue(session["duration"]), displayValue(session["until"]),
					displayValue(session["captures"]),
				})
			}
			pretty += "\n" + table([]string{"id", "clients", "providers", "models", "duration", "until", "captures"}, rows)
		}
		pretty += "\n" + dim("Captured request and response bodies are sensitive even after header redaction.")
		return Result{Pretty: pretty, Data: raw}, nil
	case "on":
		return debugOn(ctx)
	case "off":
		return debugOff(ctx)
	default:
		return exitUsage(fmt.Errorf("unknown debug subcommand %q", sub))
	}
}

func debugOn(ctx *Context) (Result, error) {
	if ctx.Flags.Bool("all", false) {
		return exitUsage(errors.New("debug on requires a scope; --all is only for debug off"))
	}
	id := strings.TrimSpace(ctx.Flags.String("id", ""))
	clients, err := splitNames(ctx.Flags.String("client", ""))
	if err != nil {
		return exitUsage(fmt.Errorf("--client: %w", err))
	}
	providers, err := splitNames(ctx.Flags.String("provider", ""))
	if err != nil {
		return exitUsage(fmt.Errorf("--provider: %w", err))
	}
	models, err := splitNames(ctx.Flags.String("model", ""))
	if err != nil {
		return exitUsage(fmt.Errorf("--model: %w", err))
	}
	start := mcp.AuditStartInput{
		SessionID: id, Clients: clients, Providers: providers, Models: models,
	}
	if ctx.Flags.Present("duration") {
		value := ctx.Flags.String("duration", "")
		start.Duration = &value
	}
	body, err := start.Document()
	if err != nil {
		return exitUsage(err)
	}
	checkCtx, cancelCheck := ctx.bounded()
	err = ctx.C.CheckKnownCaptureScope(checkCtx, clients, providers, models)
	cancelCheck()
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	action := "start a scoped debug capture"
	if id != "" {
		action = "update debug session " + id
	}
	if result, proceed, err := guarded(ctx, action, body); !proceed {
		result.Pretty = yellow("Debug capture stores full request and response bodies, which stay sensitive even after header redaction.") +
			"\n" + result.Pretty + "\n" + prettyJSON(body)
		result.Data = map[string]any{"applied": false, "body": body}
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var out map[string]any
	if _, err := ctx.C.PostJSON(reqCtx, mcp.RouteDebug, nil, body, &out, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: yellow("Debug capture stores full request and response bodies.") + "\n" + green("debug updated") + "\n" + prettyJSON(out), Data: out}, nil
}

func debugOff(ctx *Context) (Result, error) {
	if err := ctx.rejectFlagsExcept("id", "all"); err != nil {
		return exitUsage(err)
	}
	id := strings.TrimSpace(ctx.Flags.String("id", ""))
	all := ctx.Flags.Bool("all", false)
	if id == "" && !all {
		return exitUsage(errors.New("debug off requires --id or --all; scope flags do not identify a session"))
	}
	if id != "" && all {
		return exitUsage(errors.New("--id and --all are mutually exclusive"))
	}
	body := (mcp.AuditStopInput{SessionID: id}).Document()
	action := "stop every debug session"
	if id != "" {
		action = "stop debug session " + id
	}
	if result, proceed, err := guarded(ctx, action, body); !proceed {
		result.Pretty += "\n" + prettyJSON(body)
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var out map[string]any
	if _, err := ctx.C.PostJSON(reqCtx, mcp.RouteDebug, nil, body, &out, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: green("debug updated") + "\n" + prettyJSON(out), Data: out}, nil
}

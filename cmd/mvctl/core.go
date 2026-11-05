package main

// The shared command core. These files are stdlib-only and are copied verbatim
// into the small upstream `cmd/mvctl` contribution; the standalone build adds
// more commands in extra.go and internal/.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Result struct {
	Pretty string
	Data   any
	Code   int
}

type Context struct {
	Ctx   context.Context
	G     Global
	C     *Client
	Args  []string
	Flags map[string]string
}

type Command struct {
	Name    string
	Group   string
	Usage   string
	Summary string
	Run     func(Context) (Result, error)
}

func flag(ctx Context, name, fallback string) string {
	if v, ok := ctx.Flags[name]; ok && v != "" {
		return v
	}
	return fallback
}

func wantYes(ctx Context) bool { return ctx.G.Yes }

func CoreCommands() []Command {
	return []Command{
		{Name: "status", Group: "view", Usage: "mvctl status", Summary: "Health, install and headline metrics", Run: cmdStatus},
		{Name: "overview", Group: "view", Usage: "mvctl overview", Summary: "Full KPI, storage and operator state", Run: cmdOverview},
		{Name: "requests", Group: "view", Usage: "mvctl requests [N]", Summary: "Recent proxied requests", Run: cmdRequests},
		{Name: "query", Group: "view", Usage: "mvctl query \"SELECT ...\"", Summary: "Bounded SQL over durable history", Run: cmdQuery},
		{Name: "prometheus", Group: "view", Usage: "mvctl prometheus", Summary: "Prometheus text exposition", Run: cmdPrometheus},
		{Name: "watch", Group: "view", Usage: "mvctl watch [--samples N]", Summary: "Sample live counters", Run: cmdWatch},
		{Name: "url", Group: "view", Usage: "mvctl url", Summary: "Print the proxy base URL", Run: cmdURL},
		{Name: "health", Group: "view", Usage: "mvctl health", Summary: "Check /healthz", Run: cmdHealth},
		{Name: "config", Group: "control", Usage: "mvctl config get|set|reload|keys|diff", Summary: "Read, validate and change live config", Run: cmdConfig},
		{Name: "pause", Group: "control", Usage: "mvctl pause [on|off]", Summary: "Show or change pause holds", Run: cmdPause},
		{Name: "throttle", Group: "control", Usage: "mvctl throttle [set|clear]", Summary: "Show or change provider limits", Run: cmdThrottle},
		{Name: "quota", Group: "control", Usage: "mvctl quota [resume]", Summary: "Show or resume a quota gate", Run: cmdQuota},
		{Name: "drain", Group: "control", Usage: "mvctl drain --all|--provider X", Summary: "Release parked requests", Run: cmdDrain},
		{Name: "debug", Group: "control", Usage: "mvctl debug [on|off]", Summary: "Show or change capture sessions", Run: cmdDebug},
		{Name: "purge", Group: "control", Usage: "mvctl purge [--client X] --yes", Summary: "Preview then delete history", Run: cmdPurge},
		{Name: "backup", Group: "control", Usage: "mvctl backup --out FILE", Summary: "Download a settings+data archive", Run: cmdBackup},
		{Name: "restore", Group: "control", Usage: "mvctl restore --file FILE [--yes]", Summary: "Inspect or apply an archive", Run: cmdRestore},
		{Name: "restart", Group: "control", Usage: "mvctl restart --yes", Summary: "Gracefully restart the proxy", Run: cmdRestart},
	}
}

func Dispatch(cmds []Command, ctx Context) (Result, error) {
	name := "help"
	rest := []string{}
	if len(ctx.Args) > 0 {
		name = ctx.Args[0]
		rest = ctx.Args[1:]
	}
	if name == "help" || name == "--help" || name == "-h" {
		return Result{Pretty: Help(cmds), Data: map[string]any{"commands": commandNames(cmds)}}, nil
	}
	for _, cmd := range cmds {
		if cmd.Name == name {
			ctx.Args = rest
			return cmd.Run(ctx)
		}
	}
	return Result{Code: 2}, fmt.Errorf("unknown command %q (try 'mvctl help')", name)
}

func commandNames(cmds []Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name)
	}
	return out
}

func Help(cmds []Command) string {
	groups := map[string][]Command{}
	var order []string
	for _, c := range cmds {
		if _, ok := groups[c.Group]; !ok {
			order = append(order, c.Group)
		}
		groups[c.Group] = append(groups[c.Group], c)
	}
	sort.Strings(order)
	var b strings.Builder
	b.WriteString(heading("mvctl — millivolt operator CLI"))
	b.WriteString("\n\nUsage: mvctl [global flags] <command> [args]\n")
	b.WriteString("Global: --json --no-color --yes --profile NAME --theme NAME --mode black|clear --url URL --token T\n\n")
	for _, g := range order {
		b.WriteString(bold(strings.ToUpper(g)) + "\n")
		for _, c := range groups[g] {
			b.WriteString(fmt.Sprintf("  %s%s  %s\n", pad(c.Name, 12, false), dim(c.Usage), dim(c.Summary)))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// view
// ---------------------------------------------------------------------------

func cmdStatus(ctx Context) (Result, error) {
	healthy := ctx.C.Health(ctx.Ctx)
	snap, err := ctx.C.Bootstrap(ctx.Ctx)
	if err != nil {
		return Result{Code: 1}, err
	}
	state := "down"
	if healthy {
		state = "up"
	}
	pretty := kv([][2]string{
		{"state", statusText(healthy, state)},
		{"url", ctx.G.BaseURL},
		{"version", shortVersion(snap.DashboardVersion)},
		{"requests", num(snap.KPI.Requests)},
		{"errors", num(snap.KPI.Errors)},
		{"in flight", num(snap.KPI.InFlight)},
		{"cost", cost(snap.KPI.Cost)},
		{"tokens", num(snap.KPI.InputTokens + snap.KPI.OutputTokens)},
		{"storage", storageText(snap)},
		{"paused", boolText(snap.Pause.Paused)},
	})
	data := map[string]any{
		"healthy": healthy, "url": ctx.G.BaseURL, "version": snap.DashboardVersion,
		"kpi": snap.KPI, "storage": snap.Storage, "pause": snap.Pause,
		"throttle": snap.Throttle, "debug": snap.Debug,
	}
	return Result{Pretty: heading("millivolt status") + "\n" + pretty, Data: data}, nil
}

func statusText(healthy bool, state string) string {
	if healthy {
		return green("●") + " " + state
	}
	return red("●") + " " + state
}

func boolText(v bool) string {
	if v {
		return yellow("yes")
	}
	return dim("no")
}

func shortVersion(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	if v == "" {
		return dim("unknown")
	}
	return v
}

func storageText(s Snapshot) string {
	if !s.Storage.Enabled {
		return dim("ring only")
	}
	if s.Storage.Dropped > 0 {
		return yellow(fmt.Sprintf("durable, %s dropped", num(s.Storage.Dropped)))
	}
	return green("durable")
}

func cmdOverview(ctx Context) (Result, error) {
	s, err := ctx.C.Bootstrap(ctx.Ctx)
	if err != nil {
		return Result{Code: 1}, err
	}
	rows := [][]string{
		{"Requests", num(s.KPI.Requests), "Errors", num(s.KPI.Errors)},
		{"In flight", num(s.KPI.InFlight), "Cost", cost(s.KPI.Cost)},
		{"Input tokens", num(s.KPI.InputTokens), "Output tokens", num(s.KPI.OutputTokens)},
		{"Cache read", num(s.KPI.CacheReadTokens), "Reasoning", num(s.KPI.ReasoningTokens)},
		{"Avg TTFT", durPtr(s.KPI.AvgTTFTMs), "Avg TPS", numPtr(s.KPI.AvgTPS)},
		{"Cost / req", cost(s.KPI.CostPerReq), "Cost / MTok", cost(s.KPI.CostPerMTok)},
	}
	pretty := heading("overview") + "\n" + table([]string{"metric", "value", "metric", "value"}, rows)
	pretty += "\n\n" + kv([][2]string{
		{"storage", storageText(s)},
		{"dropped", num(s.Storage.Dropped)},
		{"pause", boolText(s.Pause.Paused) + fmt.Sprintf(" (%s queued)", num(s.Pause.Queued))},
		{"throttles", num(float64(len(s.Throttle.Throttles)))},
		{"debug", boolText(s.Debug.Enabled)},
		{"dashboard", shortVersion(s.DashboardVersion)},
	})
	return Result{Pretty: pretty, Data: map[string]any{"kpi": s.KPI, "storage": s.Storage, "pause": s.Pause, "throttle": s.Throttle, "debug": s.Debug}}, nil
}

func durPtr(v *float64) string {
	if v == nil {
		return "—"
	}
	return dur(*v)
}

func numPtr(v *float64) string {
	if v == nil {
		return "—"
	}
	return num(*v)
}

func cmdRequests(ctx Context) (Result, error) {
	limit := 20
	if len(ctx.Args) > 0 {
		if n, err := strconv.Atoi(ctx.Args[0]); err == nil {
			limit = n
		}
	}
	page, err := ctx.C.Requests(ctx.Ctx, limit, 0, "")
	if err != nil {
		return Result{Code: 1}, err
	}
	rows := make([][]string, 0, len(page.Rows))
	for _, r := range page.Rows {
		errText := ""
		if r.ErrorType != "" {
			errText = "!" + r.ErrorType
		} else if r.Status >= 400 {
			errText = "!" + fmt.Sprintf("%d", r.Status)
		}
		rows = append(rows, []string{
			shortTime(r.Start), statusCode(r.Status),
			truncate(r.Provider+"/"+r.Model, 34), truncate(r.Client, 16),
			dur(r.DurationMs), num(r.InputTokens + r.OutputTokens), errText,
		})
	}
	pretty := heading(fmt.Sprintf("recent requests (%d)", len(page.Rows)))
	if len(rows) > 0 {
		pretty += "\n" + table([]string{"time", "status", "provider/model", "client", "duration", "tokens", "error"}, rows)
	} else {
		pretty += "\n" + dim("No requests recorded.")
	}
	if page.More {
		pretty += "\n" + dim("more available")
	}
	return Result{Pretty: pretty, Data: map[string]any{"rows": page.Rows, "more": page.More, "cursor_ms": page.CursorMs, "cursor_id": page.CursorID}}, nil
}

func cmdQuery(ctx Context) (Result, error) {
	sql := strings.Join(ctx.Args, " ")
	if sql == "" {
		return Result{Code: 2}, fmt.Errorf("usage: mvctl query \"SELECT ...\"")
	}
	rows, err := ctx.C.Query(ctx.Ctx, sql)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: toJSON(rows), Data: rows}, nil
}

func cmdPrometheus(ctx Context) (Result, error) {
	text, err := ctx.C.Text(ctx.Ctx, "/metrics/prometheus", nil)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: strings.TrimRight(text, "\n"), Data: map[string]any{"exposition": text}}, nil
}

func cmdWatch(ctx Context) (Result, error) {
	samples := 5
	interval := 1
	if v := flag(ctx, "samples", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			samples = n
		}
	}
	if v := flag(ctx, "interval", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			interval = n
		}
	}
	var series []float64
	var snapshot Snapshot
	for i := 0; i < samples; i++ {
		s, err := ctx.C.Bootstrap(ctx.Ctx)
		if err != nil {
			return Result{Code: 1}, err
		}
		delta := s.KPI.Requests - snapshot.KPI.Requests
		if i > 0 && delta >= 0 {
			series = append(series, delta)
		}
		snapshot = s
		if i < samples-1 {
			select {
			case <-ctx.Ctx.Done():
				i = samples
			case <-time.After(time.Duration(interval) * time.Second):
			}
		}
	}
	pretty := heading("watch") + "\n" + spark(series, 48) +
		fmt.Sprintf("\nrequests %s  errors %s  in flight %s  cost %s",
			num(snapshot.KPI.Requests), num(snapshot.KPI.Errors),
			num(snapshot.KPI.InFlight), cost(snapshot.KPI.Cost))
	return Result{
		Pretty: pretty,
		Data: map[string]any{
			"series": series, "total": snapshot.KPI.Requests,
			"errors": snapshot.KPI.Errors, "in_flight": snapshot.KPI.InFlight,
			"cost": snapshot.KPI.Cost,
		},
	}, nil
}

func cmdURL(ctx Context) (Result, error) {
	return Result{Pretty: ctx.G.BaseURL, Data: map[string]any{"url": ctx.G.BaseURL}}, nil
}

func cmdHealth(ctx Context) (Result, error) {
	ok := ctx.C.Health(ctx.Ctx)
	if !ok {
		return Result{Code: 1, Pretty: red("● unhealthy")}, fmt.Errorf("millivolt /healthz did not answer")
	}
	return Result{Pretty: green("● healthy") + " " + ctx.G.BaseURL, Data: map[string]any{"healthy": true}}, nil
}

// ---------------------------------------------------------------------------
// control
// ---------------------------------------------------------------------------

func cmdConfig(ctx Context) (Result, error) {
	sub := "get"
	if len(ctx.Args) > 0 {
		sub = ctx.Args[0]
	}
	doc, err := ctx.C.Config(ctx.Ctx)
	if err != nil {
		return Result{Code: 1}, err
	}
	switch sub {
	case "get":
		key := ""
		if len(ctx.Args) > 1 {
			key = ctx.Args[1]
		}
		if key != "" {
			v, ok := doc.Values[key]
			if !ok {
				return Result{Code: 1}, fmt.Errorf("config key %q not found", key)
			}
			return Result{Pretty: fmt.Sprintf("%s = %v", key, v), Data: map[string]any{key: v}}, nil
		}
		keys := make([]string, 0, len(doc.Values))
		for k := range doc.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rows := make([][]string, 0, len(keys))
		for _, k := range keys {
			rows = append(rows, []string{k, truncate(fmt.Sprintf("%v", doc.Values[k]), 60)})
		}
		pretty := heading("config") + "\n" + table([]string{"key", "value"}, rows)
		pretty += "\n\n" + kv([][2]string{
			{"revision", doc.Revision},
			{"path", doc.Path},
			{"writable", boolText(doc.Writable)},
			{"restart required", strings.Join(doc.RestartRequired, ", ")},
		})
		return Result{Pretty: pretty, Data: doc}, nil
	case "keys":
		needle := ""
		if len(ctx.Args) > 1 {
			needle = strings.ToLower(ctx.Args[1])
		}
		keys := make([]string, 0, len(doc.Fields))
		for _, field := range doc.Fields {
			k := str(field["key"])
			if k == "" {
				continue
			}
			if needle == "" || strings.Contains(strings.ToLower(k), needle) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return Result{Pretty: strings.Join(keys, "\n"), Data: map[string]any{"keys": keys}}, nil
	case "diff":
		var rows [][]string
		for k, v := range doc.Values {
			def := doc.Defaults[k]
			if fmt.Sprintf("%v", v) != fmt.Sprintf("%v", def) {
				rows = append(rows, []string{k, truncate(fmt.Sprintf("%v", def), 34), truncate(fmt.Sprintf("%v", v), 34)})
			}
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
		pretty := heading("config diff (file vs defaults)") + "\n"
		if len(rows) == 0 {
			pretty += dim("no changes from defaults")
		} else {
			pretty += table([]string{"key", "default", "effective"}, rows)
		}
		return Result{Pretty: pretty, Data: map[string]any{"changed": rows}}, nil
	case "reload":
		if r, proceed, err := guarded(ctx, "reload the live configuration"); !proceed {
			return r, err
		}
		out, err := ctx.C.Post(ctx.Ctx, "/admin/reload", nil, map[string]any{})
		if err != nil {
			return Result{Code: 1}, err
		}
		return Result{Pretty: green("config reloaded") + " " + toJSON(out), Data: out}, nil
	case "set":
		if r, proceed, err := guarded(ctx, "change the live configuration"); !proceed {
			return r, err
		}
		if len(ctx.Args) < 2 {
			return Result{Code: 2}, fmt.Errorf("usage: mvctl config set KEY=VALUE [KEY=VALUE ...]")
		}
		values := map[string]any{}
		for _, pair := range ctx.Args[1:] {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) != 2 {
				return Result{Code: 2}, fmt.Errorf("expected KEY=VALUE, got %q", pair)
			}
			values[parts[0]] = parseValue(parts[1])
		}
		out, err := ctx.C.SetConfig(ctx.Ctx, doc.Revision, values)
		if err != nil {
			return Result{Code: 1}, err
		}
		return Result{Pretty: green("config saved") + "\n" + toJSON(out), Data: out}, nil
	default:
		return Result{Code: 2}, fmt.Errorf("unknown config subcommand %q", sub)
	}
}

// guarded makes every state-changing command explicit: without --yes it
// returns a preview and performs no request. It is shared with the upstream
// contribution so mutating commands are safe in both distributions.
func guarded(ctx Context, action string) (Result, bool, error) {
	if wantYes(ctx) {
		return Result{}, true, nil
	}
	return Result{
		Pretty: yellow("Preview only. Re-run with --yes to " + action + "."),
		Data:   map[string]any{"applied": false, "action": action},
	}, false, nil
}

func parseValue(s string) any {
	if s == "true" || s == "false" {
		return s == "true"
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	return s
}

func cmdPause(ctx Context) (Result, error) {
	sub := ""
	if len(ctx.Args) > 0 {
		sub = ctx.Args[0]
	}
	if sub == "" || sub == "show" {
		out, err := ctx.C.Get(ctx.Ctx, "/admin/pause", nil)
		if err != nil {
			return Result{Code: 1}, err
		}
		return Result{Pretty: heading("pause") + "\n" + toJSON(out), Data: out}, nil
	}
	if sub != "on" && sub != "off" {
		return Result{Code: 2}, fmt.Errorf("usage: mvctl pause [on|off]")
	}
	if r, proceed, err := guarded(ctx, sub+" pause holds"); !proceed {
		return r, err
	}
	body := map[string]any{"paused": sub == "on"}
	if v := flag(ctx, "provider", ""); v != "" {
		body["providers"] = strings.Split(v, ",")
	}
	if v := flag(ctx, "client", ""); v != "" {
		body["clients"] = strings.Split(v, ",")
	}
	if v := flag(ctx, "duration", ""); v != "" {
		body["duration"] = v
	}
	out, err := ctx.C.Post(ctx.Ctx, "/admin/pause", nil, body)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: green("pause updated") + "\n" + toJSON(out), Data: out}, nil
}

func cmdThrottle(ctx Context) (Result, error) {
	sub := ""
	if len(ctx.Args) > 0 {
		sub = ctx.Args[0]
	}
	if sub == "" || sub == "show" {
		out, err := ctx.C.Get(ctx.Ctx, "/admin/throttle", nil)
		if err != nil {
			return Result{Code: 1}, err
		}
		return Result{Pretty: heading("throttle") + "\n" + toJSON(out), Data: out}, nil
	}
	provider := flag(ctx, "provider", "")
	if provider == "" {
		return Result{Code: 2}, fmt.Errorf("--provider is required")
	}
	if r, proceed, err := guarded(ctx, sub+" the throttle for "+provider); !proceed {
		return r, err
	}
	body := map[string]any{"provider": provider}
	if sub == "clear" {
		body["clear"] = true
	} else if sub == "set" {
		for _, key := range []string{"concurrency", "requests", "tokens"} {
			if v := flag(ctx, key, ""); v != "" {
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return Result{Code: 2}, fmt.Errorf("%s must be a number", key)
				}
				body[key] = n
			}
		}
		for _, key := range []string{"request_window", "token_window", "ramp_duration"} {
			if v := flag(ctx, key, ""); v != "" {
				body[key] = v
			}
		}
		if v := flag(ctx, "anti-burst", ""); v != "" {
			body["anti_burst"] = v == "true"
		}
	} else {
		return Result{Code: 2}, fmt.Errorf("usage: mvctl throttle [set|clear] --provider X")
	}
	out, err := ctx.C.Post(ctx.Ctx, "/admin/throttle", nil, body)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: green("throttle updated") + "\n" + toJSON(out), Data: out}, nil
}

func cmdQuota(ctx Context) (Result, error) {
	if len(ctx.Args) == 0 || ctx.Args[0] != "resume" {
		out, err := ctx.C.Get(ctx.Ctx, "/admin/quota", nil)
		if err != nil {
			return Result{Code: 1}, err
		}
		return Result{Pretty: heading("quota / storm") + "\n" + toJSON(out), Data: out}, nil
	}
	provider := flag(ctx, "provider", "")
	if provider == "" {
		return Result{Code: 2}, fmt.Errorf("--provider is required")
	}
	if r, proceed, err := guarded(ctx, "resume the quota gate for "+provider); !proceed {
		return r, err
	}
	out, err := ctx.C.Post(ctx.Ctx, "/admin/quota", nil, map[string]any{"provider": provider, "resume": true})
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: green("quota gate resumed") + "\n" + toJSON(out), Data: out}, nil
}

func cmdDrain(ctx Context) (Result, error) {
	body := map[string]any{}
	count := 0
	for _, key := range []string{"provider", "model", "client"} {
		if v := flag(ctx, key, ""); v != "" {
			body[key] = v
			count++
		}
	}
	// --yes is a confirmation flag only; it must never widen the drain scope.
	if flag(ctx, "all", "") == "true" {
		if count != 0 {
			return Result{Code: 2}, fmt.Errorf("--all cannot be combined with --provider, --model or --client")
		}
		body = map[string]any{"all": true}
		count = 1
	}
	if count != 1 {
		return Result{Code: 2}, fmt.Errorf("exactly one of --all, --provider, --model, --client is required")
	}
	if r, proceed, err := guarded(ctx, "drain the request queue"); !proceed {
		return r, err
	}
	out, err := ctx.C.Post(ctx.Ctx, "/admin/queue/drain", nil, body)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: green("queue drained") + "\n" + toJSON(out), Data: out}, nil
}

func cmdDebug(ctx Context) (Result, error) {
	sub := ""
	if len(ctx.Args) > 0 {
		sub = ctx.Args[0]
	}
	if sub == "" || sub == "show" {
		out, err := ctx.C.Get(ctx.Ctx, "/admin/debug", nil)
		if err != nil {
			return Result{Code: 1}, err
		}
		return Result{Pretty: heading("debug capture") + "\n" + toJSON(out), Data: out}, nil
	}
	if sub != "on" && sub != "off" {
		return Result{Code: 2}, fmt.Errorf("usage: mvctl debug [on|off]")
	}
	if r, proceed, err := guarded(ctx, sub+" debug capture"); !proceed {
		return r, err
	}
	body := map[string]any{"enabled": sub == "on"}
	for _, key := range []string{"clients", "providers", "models"} {
		if v := flag(ctx, strings.TrimSuffix(key, "s"), ""); v != "" {
			body[key] = strings.Split(v, ",")
		}
	}
	if v := flag(ctx, "duration", ""); v != "" {
		body["duration"] = v
	}
	out, err := ctx.C.Post(ctx.Ctx, "/admin/debug", nil, body)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: green("debug updated") + "\n" + toJSON(out), Data: out}, nil
}

// purgeFilter mirrors storage.PurgeFilter exactly. The admin decoder rejects
// unknown keys, so plural or invented names must never be sent.
func purgeFilter(ctx Context) map[string]any {
	f := map[string]any{}
	for _, key := range []string{"client", "provider", "model", "conversation_id", "error_type"} {
		if v := flag(ctx, key, ""); v != "" {
			f[key] = v
		}
	}
	if v := flag(ctx, "status", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f["status_code"] = n
		}
	}
	for _, key := range []string{"before_ms", "after_ms"} {
		if v := flag(ctx, key, ""); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				f[key] = n
			}
		}
	}
	for _, key := range []string{"has_error", "debug"} {
		if v := flag(ctx, key, ""); v != "" {
			f[key] = v == "true" || v == "1"
		}
	}
	return f
}

func cmdPurge(ctx Context) (Result, error) {
	f := purgeFilter(ctx)
	count, err := ctx.C.Post(ctx.Ctx, "/admin/purge/count", nil, f)
	if err != nil {
		return Result{Code: 1}, err
	}
	preview := heading("purge preview") + "\n" + toJSON(count)
	if !wantYes(ctx) {
		return Result{Pretty: preview + "\n\n" + yellow("Preview only. Re-run with --yes to delete."), Data: map[string]any{"preview": count, "applied": false}}, nil
	}
	if len(f) == 0 && flag(ctx, "all", "") != "true" {
		return Result{Code: 2, Pretty: preview + "\n\n" + yellow("Refusing an unfiltered purge. Add a filter or pass --all --yes.")},
			fmt.Errorf("refusing an unfiltered purge; add a filter or pass --all --yes")
	}
	var body any = f
	if len(f) == 0 {
		// No body means "delete everything" to millivolt; only an explicit
		// --all --yes reaches this branch.
		body = nil
	}
	out, err := ctx.C.Post(ctx.Ctx, "/admin/purge", nil, body)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: green("history purged") + "\n" + toJSON(out), Data: map[string]any{"preview": count, "applied": true, "result": out}}, nil
}

func cmdBackup(ctx Context) (Result, error) {
	out := flag(ctx, "out", "millivolt-backup.mvb")
	if _, err := os.Stat(out); err == nil && flag(ctx, "overwrite", "") != "true" {
		return Result{Code: 2}, fmt.Errorf("backup file %s already exists; pass --overwrite=true to replace it", out)
	}
	q := url.Values{}
	if flag(ctx, "config", "1") != "0" {
		q.Set("config", "1")
	}
	if flag(ctx, "database", "1") != "0" {
		q.Set("database", "1")
	}
	// Stream to a temp file: archives can exceed the 4 MiB metrics cap, and a
	// truncated archive must never be reported as a successful backup.
	n, err := ctx.C.Download(ctx.Ctx, "/admin/backup", q, out)
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{
		Pretty: fmt.Sprintf("%s  %s written (%s)", green("backup saved"), out, bytesHuman(float64(n))),
		Data:   map[string]any{"file": out, "bytes": n},
	}, nil
}

func cmdRestore(ctx Context) (Result, error) {
	file := flag(ctx, "file", "")
	if file == "" {
		return Result{Code: 2}, fmt.Errorf("usage: mvctl restore --file FILE [--yes]")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return Result{Code: 1}, err
	}
	if len(data) < 4 {
		return Result{Code: 2}, fmt.Errorf("%s is too small to be a millivolt archive", file)
	}
	q := url.Values{}
	apply := wantYes(ctx)
	if !apply {
		q.Set("inspect", "1")
	}
	out, err := ctx.C.PostRaw(ctx.Ctx, "/admin/restore", q, "application/octet-stream", data)
	if err != nil {
		return Result{Code: 1}, err
	}
	label := "inspect (no changes applied; the archive was still uploaded for validation)"
	if apply {
		label = "restore applied"
	}
	return Result{Pretty: green(label) + "\n" + toJSON(out), Data: map[string]any{"applied": apply, "result": out}}, nil
}

func cmdRestart(ctx Context) (Result, error) {
	if !wantYes(ctx) {
		return Result{Pretty: yellow("Preview only. Re-run with --yes to restart."), Data: map[string]any{"applied": false}}, nil
	}
	out, err := ctx.C.Post(ctx.Ctx, "/admin/restart", nil, map[string]any{})
	if err != nil {
		return Result{Code: 1}, err
	}
	return Result{Pretty: green("restart requested") + "\n" + toJSON(out), Data: out}, nil
}

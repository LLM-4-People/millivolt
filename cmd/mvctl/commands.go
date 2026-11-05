package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	backupparams "github.com/LLM-4-People/millivolt/internal/backup/params"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// Result is one command outcome. Data is the JSON document printed when
// --json is set; Pretty is the human rendering. Code is the process exit code
// on success, so a command can report a preview or a canceled stream without
// pretending it failed.
type Result struct {
	Pretty string
	Data   any
	Code   int
}

// Context is one command invocation. C is nil only for commands that do not
// talk to the proxy (url, a bare help or version).
type Context struct {
	Ctx     context.Context
	G       Global
	C       *mcp.Client
	Command *Command
	Args    []string
	Flags   flagValues
}

type Command struct {
	Name     string
	Group    string
	Summary  string
	Args     string
	Flags    []flagSpec
	NoClient bool
	Run      func(*Context) (Result, error)
}

func registry() []Command {
	defaults := defaultSettings()
	return []Command{
		{Name: "status", Group: "view", Summary: "Health, dashboard build and headline metrics", Run: cmdStatus},
		{Name: "overview", Group: "view", Summary: "KPI, storage and operator state", Run: cmdOverview},
		{Name: "requests", Group: "view", Summary: fmt.Sprintf("Recent proxied requests (default %d)", defaults.RequestsPage), Args: "[N]",
			Flags: []flagSpec{
				{name: "before-ms", kind: kindInt, usage: "page before this start time in Unix milliseconds"},
				{name: "before-id", kind: kindString, usage: "page before this record id"},
			}, Run: cmdRequests},
		{Name: "query", Group: "view", Summary: "Bounded SQL over durable history", Args: `"SELECT ..."`,
			Flags: []flagSpec{
				{name: "query-timeout", kind: kindDuration, usage: fmt.Sprintf("bound the SQL read (default %s)", defaults.QueryTimeout)},
			}, Run: cmdQuery},
		{Name: "prometheus", Group: "view", Summary: "Prometheus text exposition", Run: cmdPrometheus},
		{Name: "watch", Group: "view", Summary: "Sample live counters",
			Flags: []flagSpec{
				{name: "samples", kind: kindInt, usage: fmt.Sprintf("number of snapshots (%d..%d, default %d)", defaults.WatchSamplesMin, defaults.WatchSamplesMax, defaults.WatchSamples)},
				{name: "interval", kind: kindDuration, usage: fmt.Sprintf("time between snapshots (default %s)", defaults.WatchInterval)},
			}, Run: cmdWatch},
		{Name: "health", Group: "view", Summary: "Check /healthz", Run: cmdHealth},
		{Name: "url", Group: "view", Summary: "Print the normalized proxy origin", NoClient: true, Run: cmdURL},

		{Name: "config", Group: "control", Summary: "Read, validate and change live config", Args: "get|set|reload|keys|diff",
			Flags: []flagSpec{
				{name: "reveal", kind: kindBool, usage: "print credential-class provider header values (get, diff, set)"},
			}, Run: cmdConfig},
		{Name: "pause", Group: "control", Summary: "Show or change pause holds", Args: "show|on|off",
			Flags: []flagSpec{
				{name: "client", kind: kindString, usage: "comma-separated client names"},
				{name: "provider", kind: kindString, usage: "comma-separated provider names"},
				{name: "duration", kind: kindString, usage: mcp.CaptureDurationHelp},
				{name: "id", kind: kindString, usage: "hold id to edit or remove"},
				{name: "all", kind: kindBool, usage: "pause every client and provider, or remove every hold"},
				{name: "new", kind: kindBool, usage: "pause only clients first seen after the hold"},
				{name: "max-queued", kind: kindInt, usage: "queue cap for the hold"},
			}, Run: cmdPause},
		{Name: "throttle", Group: "control", Summary: "Show or change provider limits", Args: "show|set|clear",
			Flags: []flagSpec{
				{name: "provider", kind: kindString, usage: "provider label"},
				{name: "concurrency", kind: kindInt, usage: "concurrent request cap"},
				{name: "requests", kind: kindInt, usage: "requests per window"},
				{name: "request-window", kind: kindString, usage: "request window such as 1m"},
				{name: "tokens", kind: kindInt, usage: "tokens per window"},
				{name: "token-window", kind: kindString, usage: "token window such as 1m"},
				{name: "anti-burst", kind: kindBool, usage: "enable anti-burst pacing"},
				{name: "ramp-duration", kind: kindString, usage: "ramp duration such as 30s"},
			}, Run: cmdThrottle},
		{Name: "quota", Group: "control", Summary: "Show or resume a quota gate", Args: "show|resume",
			Flags: []flagSpec{
				{name: "provider", kind: kindString, usage: "provider label"},
			}, Run: cmdQuota},
		{Name: "drain", Group: "control", Summary: "Release parked requests",
			Flags: []flagSpec{
				{name: "all", kind: kindBool, usage: "drain every parked request"},
				{name: "provider", kind: kindString, usage: "drain one provider"},
				{name: "model", kind: kindString, usage: "drain one model"},
				{name: "client", kind: kindString, usage: "drain one client"},
			}, Run: cmdDrain},
		{Name: "debug", Group: "control", Summary: "Show or change capture sessions", Args: "show|on|off",
			Flags: []flagSpec{
				{name: "client", kind: kindString, usage: "comma-separated client scope"},
				{name: "provider", kind: kindString, usage: "comma-separated provider scope"},
				{name: "model", kind: kindString, usage: "comma-separated model scope"},
				{name: "duration", kind: kindString, usage: mcp.CaptureDurationHelp},
				{name: "id", kind: kindString, usage: "session id to stop"},
				{name: "all", kind: kindBool, usage: "stop every session (debug off)"},
			}, Run: cmdDebug},
		{Name: "backup", Group: "control", Summary: "Download a settings and history archive",
			Flags: []flagSpec{
				{name: "out", kind: kindString, usage: "output archive path (required)"},
				{name: "overwrite", kind: kindBool, usage: "replace an existing output file"},
				backupConfigFlag(defaults),
				backupDatabaseFlag(defaults),
			}, Run: cmdBackup},
		{Name: "restore", Group: "control", Summary: "Inspect or apply an archive",
			Flags: []flagSpec{
				{name: "file", kind: kindString, usage: "archive path (required)"},
				{name: "inspect", kind: kindBool, usage: "validate and print the archive without applying it"},
				{name: "reveal", kind: kindBool, usage: "print credential-class provider header values in the inspect document"},
				restoreConfigFlag(defaults),
				restoreDatabaseFlag(defaults),
				restoreConfigModeFlag(defaults),
				restoreDatabaseModeFlag(defaults),
			}, Run: cmdRestore},
		{Name: "purge", Group: "control", Summary: "Preview then delete history",
			Flags: []flagSpec{
				{name: "client", kind: kindString, usage: "exact client label"},
				{name: "provider", kind: kindString, usage: "exact provider label"},
				{name: "model", kind: kindString, usage: "exact model id"},
				{name: "conversation-id", kind: kindString, usage: "exact conversation id"},
				{name: "error-type", kind: kindString, usage: "exact final error type"},
				{name: "status", kind: kindInt, usage: "exact HTTP status code"},
				{name: "before-ms", kind: kindInt, usage: "started before this Unix millisecond"},
				{name: "after-ms", kind: kindInt, usage: "started at or after this Unix millisecond"},
				{name: "has-error", kind: kindBool, usage: "only rows with a final error"},
				{name: "debug", kind: kindBool, usage: "only operator-debug captures"},
				{name: "all", kind: kindBool, usage: "delete every row (requires --yes)"},
			}, Run: cmdPurge},
		{Name: "restart", Group: "control", Summary: "Request a graceful restart", Run: cmdRestart},
	}
}

// guarded makes every state-changing command explicit. Without --yes it
// returns a preview and performs no mutation. body is the exact request the
// apply would send, so a --json preview is as inspectable as the human one. A
// command may perform read-only requests while building the preview (a config
// revision or a purge count).
func guarded(ctx *Context, action string, body any) (Result, bool, error) {
	if ctx.G.Yes {
		return Result{}, true, nil
	}
	data := map[string]any{"applied": false, "action": action}
	if body != nil {
		data["body"] = body
	}
	return Result{
		Pretty: yellow("Preview only. Re-run with --yes to " + sanitizeCell(action) + "."),
		Data:   data,
	}, false, nil
}

func (c *Context) bounded() (context.Context, context.CancelFunc) {
	if c.G.Timeout <= 0 {
		return context.WithCancel(c.Ctx)
	}
	return context.WithTimeout(c.Ctx, c.G.Timeout)
}

func (c *Context) boundedFor(d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return c.bounded()
	}
	return context.WithTimeout(c.Ctx, d)
}

func (c *Context) requireArgs(max int, usage string) error {
	if len(c.Args) > max {
		return fmt.Errorf("usage: %s", usage)
	}
	return nil
}

func (c *Context) requireNoExtraArgs(usage string) error {
	return c.requireArgs(0, usage)
}

func exitUsage(err error) (Result, error) { return Result{Code: exitUsageCode}, err }

// rejectFlagsExcept rejects every command flag that allowed does not name. It
// fails closed: a flag added to the command is rejected by every subcommand
// until that subcommand deliberately accepts it, instead of being parsed and
// silently ignored.
func (c *Context) rejectFlagsExcept(allowed ...string) error {
	if c.Command == nil {
		return nil
	}
	permitted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		permitted[name] = true
	}
	for _, spec := range c.Command.Flags {
		if c.Flags.Present(spec.name) && !permitted[spec.name] {
			return fmt.Errorf("--%s is not valid for this subcommand", spec.name)
		}
	}
	return nil
}

func splitNames(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, errors.New("empty name in comma list")
		}
		out = append(out, value)
	}
	return out, nil
}

func boolField(document map[string]any, key string) bool {
	value, _ := document[key].(bool)
	return value
}

// The flag constructors below keep the help text and the runtime default on
// the same settings field. A default changes in settings.go only.
func backupConfigFlag(defaults settings) flagSpec {
	return flagSpec{name: "config", kind: kindBool, usage: fmt.Sprintf("include the saved config (default %t)", defaults.BackupConfig)}
}

func backupDatabaseFlag(defaults settings) flagSpec {
	return flagSpec{name: "database", kind: kindBool, usage: fmt.Sprintf("include the SQLite history (default %t)", defaults.BackupDatabase)}
}

func restoreConfigFlag(defaults settings) flagSpec {
	return flagSpec{name: "config", kind: kindBool, usage: fmt.Sprintf("restore the config member (default %t)", defaults.RestoreConfig)}
}

func restoreDatabaseFlag(defaults settings) flagSpec {
	return flagSpec{name: "database", kind: kindBool, usage: fmt.Sprintf("restore the database member (default %t)", defaults.RestoreDatabase)}
}

func restoreConfigModeFlag(defaults settings) flagSpec {
	return flagSpec{name: "config-mode", kind: kindString, usage: backupModeUsage(defaults.RestoreConfigMode)}
}

func restoreDatabaseModeFlag(defaults settings) flagSpec {
	return flagSpec{name: "database-mode", kind: kindString, usage: backupModeUsage(defaults.RestoreDatabaseMode)}
}

func backupModeUsage(fallback string) string {
	return fmt.Sprintf("%s or %s (default %s)", backupparams.ModeMerge, backupparams.ModeReplace, fallback)
}

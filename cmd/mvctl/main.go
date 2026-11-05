// Command mvctl is the human command-line client for millivolt's operator
// plane. It reads the documented /admin and /metrics routes through the same
// HTTP client the MCP server uses, so authentication, origin validation,
// redirect refusal, response bounds and credential redaction have one owner.
// It adds no server settings and no second listener.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/LLM-4-People/millivolt"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func main() {
	os.Exit(runWith(os.Args[1:], os.Stdout, os.Stderr))
}

func runWith(argv []string, stdout, stderr io.Writer) int {
	baseCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithContext(baseCtx, argv, stdout, stderr)
}

// runWithContext is the testable dispatch seam. The caller owns the base
// context, so a test can cancel an in-flight request without delivering a
// process-wide signal.
func runWithContext(baseCtx context.Context, argv []string, stdout, stderr io.Writer) int {
	cmds := registry()
	inv, err := parseInvocation(argv, cmds)
	if err != nil {
		fmt.Fprintln(stderr, "mvctl: "+sanitizeText(err.Error()))
		return exitUsageCode
	}
	setColor(resolveColor(inv.global.Color, stdout))
	if inv.version {
		fmt.Fprintln(stdout, versionLine())
		return exitOK
	}
	if inv.help {
		fmt.Fprintln(stdout, helpText(cmds, inv))
		return exitOK
	}
	if inv.command == nil {
		fmt.Fprintln(stdout, helpText(cmds, inv))
		return exitOK
	}

	cmdCtx := &Context{
		Ctx:     baseCtx,
		G:       inv.global,
		Command: inv.command,
		Args:    inv.args,
		Flags:   inv.flags,
	}
	if !inv.command.NoClient {
		client, err := newOperatorClient(inv.global)
		if err != nil {
			fmt.Fprintln(stderr, "mvctl: "+sanitizeText(err.Error()))
			return exitUsageCode
		}
		cmdCtx.C = client
	}

	result, runErr := inv.command.Run(cmdCtx)
	if runErr == nil {
		writeResult(stdout, inv.global.JSON, result)
		return result.Code
	}
	message := runErr.Error()
	if cmdCtx.C != nil {
		message = cmdCtx.C.Redact(message)
	}
	message = render(sanitizeText(message))
	if errors.Is(runErr, context.Canceled) || baseCtx.Err() != nil {
		writeResult(stdout, inv.global.JSON, result)
		return exitInterrupted
	}
	code := result.Code
	if code == 0 {
		code = exitFailure
	}
	if inv.global.JSON {
		payload := map[string]any{"error": message}
		if result.Data != nil {
			payload["details"] = result.Data
		}
		fmt.Fprintln(stdout, toJSON(payload))
		return code
	}
	fmt.Fprintln(stderr, "mvctl: "+message)
	if result.Pretty != "" {
		fmt.Fprintln(stdout, render(result.Pretty))
	}
	return code
}

func writeResult(stdout io.Writer, json bool, result Result) {
	if json && result.Data != nil {
		fmt.Fprintln(stdout, toJSON(result.Data))
		return
	}
	if result.Pretty != "" {
		fmt.Fprintln(stdout, render(result.Pretty))
	}
}

func newOperatorClient(g Global) (*mcp.Client, error) {
	// The per-request deadline lives on the command context: the shared client
	// is built without a blanket http.Client.Timeout so backup and restore can
	// stream for as long as the operator allows, and every bounded command
	// wraps its own context.
	return mcp.NewCommandClient(g.BaseURL, g.Token)
}

func versionLine() string {
	line := "mvctl " + millivolt.Version()
	if build, err := millivolt.CurrentBuild(); err == nil && build.Revision != "" && build.Revision != "unknown" {
		line += " (" + build.Revision + ")"
	}
	return line
}

// resolveColor applies the explicit flag first, then the environment, then
// the terminal. A redirected stdout is never painted, which keeps JSON, logs
// and pipelines free of escape sequences. The parser only records --color and
// --no-color; this function is the one owner of the NO_COLOR policy.
func resolveColor(mode colorMode, stdout io.Writer) bool {
	switch mode {
	case colorAlways:
		return true
	case colorNever:
		return false
	}
	if os.Getenv(noColorEnv) != "" {
		return false
	}
	file, ok := stdout.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

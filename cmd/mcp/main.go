// Command mcp serves millivolt's operator and observability API to an LLM over
// stdio. It talks to a running proxy with that proxy's operator credential; it
// never starts, reconfigures or stops a proxy itself.
//
// Setup comes from flags and environment variables, flags winning:
//
//	--proxy-url       / MILLIVOLT_MCP_PROXY_URL       (for example http://127.0.0.1:8081)
//	--operator-token  / MILLIVOLT_MCP_OPERATOR_TOKEN  (the proxy's MILLIVOLT_OPERATOR_TOKEN)
//	--query-max-rows  / MILLIVOLT_MCP_QUERY_MAX_ROWS
//	--page-size       / MILLIVOLT_MCP_PAGE_SIZE
//	--capture-max-bytes / MILLIVOLT_MCP_CAPTURE_MAX_BYTES
//	--query-timeout   / MILLIVOLT_MCP_QUERY_TIMEOUT
//	--timeout         / MILLIVOLT_MCP_TIMEOUT
//
// The credential is never logged, never placed in a URL, and never echoed into
// a tool result. stdout carries the MCP stream only: every diagnostic goes to
// stderr, because a stray stdout line would corrupt the protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/LLM-4-People/millivolt"
	"github.com/LLM-4-People/millivolt/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// options is the whole setup surface. It exists as one type so flag and
// environment parsing are a single testable step, and so no consumer anywhere
// else reads the environment.
type options struct {
	proxyURL      string
	operatorToken string
	limits        mcp.Limits
}

// parse resolves the setup parameters. Flags win over environment variables;
// an empty environment value is treated as unset so an empty container
// interpolation falls through to the default rather than to a broken value.
func parse(args []string, lookupEnv func(string) (string, bool)) (options, error) {
	defaults := mcp.DefaultLimits()
	var (
		queryMaxRows int
		pageSize     int
		captureBytes int
		queryTimeout time.Duration
		timeout      time.Duration
	)
	var err error
	if queryMaxRows, err = envInt(lookupEnv, "MILLIVOLT_MCP_QUERY_MAX_ROWS", defaults.QueryMaxRows); err != nil {
		return options{}, err
	}
	if pageSize, err = envInt(lookupEnv, "MILLIVOLT_MCP_PAGE_SIZE", defaults.PageSize); err != nil {
		return options{}, err
	}
	if captureBytes, err = envInt(lookupEnv, "MILLIVOLT_MCP_CAPTURE_MAX_BYTES", defaults.CaptureBytes); err != nil {
		return options{}, err
	}
	if queryTimeout, err = envDuration(lookupEnv, "MILLIVOLT_MCP_QUERY_TIMEOUT", defaults.QueryTimeout); err != nil {
		return options{}, err
	}
	if timeout, err = envDuration(lookupEnv, "MILLIVOLT_MCP_TIMEOUT", defaults.Timeout); err != nil {
		return options{}, err
	}
	fs := flag.NewFlagSet("millivolt-mcp", flag.ContinueOnError)
	var o options
	o.limits = mcp.Limits{QueryMaxRows: queryMaxRows, PageSize: pageSize, CaptureBytes: captureBytes, QueryTimeout: queryTimeout, Timeout: timeout}
	fs.StringVar(&o.proxyURL, "proxy-url", envString(lookupEnv, "MILLIVOLT_MCP_PROXY_URL", ""),
		"millivolt proxy origin, for example http://127.0.0.1:8081 (env MILLIVOLT_MCP_PROXY_URL)")
	fs.StringVar(&o.operatorToken, "operator-token", envString(lookupEnv, "MILLIVOLT_MCP_OPERATOR_TOKEN", ""),
		"the proxy's MILLIVOLT_OPERATOR_TOKEN (env MILLIVOLT_MCP_OPERATOR_TOKEN)")
	fs.IntVar(&o.limits.QueryMaxRows, "query-max-rows", o.limits.QueryMaxRows,
		"row cap applied to query results before the explicit truncation marker (env MILLIVOLT_MCP_QUERY_MAX_ROWS)")
	fs.IntVar(&o.limits.PageSize, "page-size", o.limits.PageSize,
		"default page size for the record and capture listings (env MILLIVOLT_MCP_PAGE_SIZE)")
	fs.IntVar(&o.limits.CaptureBytes, "capture-max-bytes", o.limits.CaptureBytes,
		"document size above which a capture is withheld whole, in bytes (env MILLIVOLT_MCP_CAPTURE_MAX_BYTES)")
	fs.DurationVar(&o.limits.QueryTimeout, "query-timeout", o.limits.QueryTimeout,
		"bound on a full-history chart or explorer read (env MILLIVOLT_MCP_QUERY_TIMEOUT)")
	fs.DurationVar(&o.limits.Timeout, "timeout", o.limits.Timeout,
		"bound on every call to the proxy (env MILLIVOLT_MCP_TIMEOUT)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stderr))
}

func run(args []string, lookupEnv func(string) (string, bool), stderr io.Writer) int {
	o, err := parse(args, lookupEnv)
	if err != nil {
		fmt.Fprintln(stderr, "millivolt-mcp: setup:", err)
		return 2
	}
	// Fail fast at startup: a missing credential or a malformed origin is a
	// setup error, not something to discover on the first tool call.
	service, err := mcp.NewService(o.proxyURL, o.operatorToken, o.limits)
	if err != nil {
		fmt.Fprintln(stderr, "millivolt-mcp: setup:", err)
		return 1
	}

	server := sdk.NewServer(&sdk.Implementation{
		Name:    "millivolt",
		Title:   "millivolt",
		Version: millivolt.Version(),
	}, nil)
	service.Register(server)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.SetPrefix("millivolt-mcp: ")
	log.SetOutput(stderr)
	// The credential is never part of this line, and neither is any request
	// body: only the origin the tools will call.
	log.Printf("serving millivolt operator tools over stdio against %s", service.Origin())
	if err := server.Run(ctx, &sdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "millivolt-mcp:", err)
		return 1
	}
	return 0
}

// envString reads an environment override. An empty value counts as unset, so
// an empty container interpolation falls through to the flag default instead of
// becoming a broken value.
func envString(lookupEnv func(string) (string, bool), name, fallback string) string {
	if value, ok := lookupEnv(name); ok && value != "" {
		return value
	}
	return fallback
}

// envInt parses an integer override. A malformed value fails setup: silently
// using the default would hide a broken deployment.
func envInt(lookupEnv func(string) (string, bool), name string, fallback int) (int, error) {
	value, ok := lookupEnv(name)
	if !ok || value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

// envDuration parses a Go duration override.
func envDuration(lookupEnv func(string) (string, bool), name string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookupEnv(name)
	if !ok || value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 30s or 2m", name)
	}
	return parsed, nil
}

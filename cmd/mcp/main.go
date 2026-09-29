// Command mcp serves millivolt's operator and observability API to an LLM over
// stdio. It talks to a running proxy with that proxy's operator credential; it
// never starts, reconfigures or stops a proxy itself.
//
// Setup comes from flags and environment variables, flags winning:
//
//	--proxy-url       / MILLIVOLT_MCP_PROXY_URL       (for example http://127.0.0.1:8081)
//	--operator-token  / MILLIVOLT_MCP_OPERATOR_TOKEN  (the proxy's MILLIVOLT_OPERATOR_TOKEN)
//	--query-max-rows  / MILLIVOLT_MCP_QUERY_MAX_ROWS
//	--query-max-bytes / MILLIVOLT_MCP_QUERY_MAX_BYTES
//	--page-size       / MILLIVOLT_MCP_PAGE_SIZE
//	--capture-max-bytes / MILLIVOLT_MCP_CAPTURE_MAX_BYTES
//	--query-timeout   / MILLIVOLT_MCP_QUERY_TIMEOUT
//	--timeout         / MILLIVOLT_MCP_TIMEOUT
//
// The proxy binary carries the same entrypoint as `millivolt mcp`; see
// docs/mcp.md for setup and client configuration.
package main

import (
	"os"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func main() {
	os.Exit(mcp.Run("millivolt-mcp", os.Args[1:], os.LookupEnv, os.Stderr))
}

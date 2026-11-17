package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

type internalMCPKey struct{}

// markInternalMCP is used only by the in-process MCP transport. A context
// value cannot be forged by an HTTP client, unlike a header.
func markInternalMCP(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalMCPKey{}, true)
}

func isInternalMCP(ctx context.Context) bool {
	marked, _ := ctx.Value(internalMCPKey{}).(bool)
	return marked
}

// internalRouteScope maps an MCP tool's internal operator call to the
// capability required to make it. Unknown routes are unmapped so a scoped
// credential fails closed rather than inheriting a broader surface.
func internalRouteScope(r *http.Request) (mcp.Capability, bool) {
	path := r.URL.Path
	switch {
	case path == "/metrics/query", path == "/metrics/agg/log", path == "/metrics/export":
		return mcp.CapabilityRecords, true
	case path == "/admin/debug", path == "/admin/debug/capture":
		if r.Method == http.MethodGet {
			return mcp.CapabilityAudit, true
		}
		return mcp.CapabilityAuditControl, true
	case path == "/admin/config":
		if r.Method == http.MethodGet {
			return mcp.CapabilityConfig, true
		}
		return mcp.CapabilityAdminConfig, true
	case path == "/admin/reload", path == "/admin/restart":
		return mcp.CapabilityAdminConfig, true
	case path == "/admin/pause", path == "/admin/throttle", path == "/admin/quota":
		return mcp.CapabilityControl, true
	case path == "/admin/purge", path == "/admin/purge/count":
		return mcp.CapabilityAdminHistory, true
	case strings.HasPrefix(path, "/metrics/"):
		return mcp.CapabilityMetrics, true
	case path == "/healthz":
		return mcp.CapabilityMetrics, true
	default:
		return "", false
	}
}

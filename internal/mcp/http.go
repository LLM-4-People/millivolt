package mcp

import (
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHTTPHandler returns the streamable HTTP transport for the tool server.
// build produces the Service for one admitted request; the request's own
// credential is bound into it, so one caller's request can never be answered
// with another caller's authorization. The handler is stateless: every POST
// gets a temporary server session built for that request and closed when it
// completes, so no session store exists and no session-scoped credential
// survives the request. Stateless mode supports plain request/response tool
// use, which is all this server does; server-to-client requests (sampling,
// elicitation) are unsupported there and no tool here needs them. Session
// timeout and session IDs are therefore meaningless on this endpoint, and the
// SDK's default request-body bound (4 MiB) stays in force: the largest request
// this server accepts is a tool argument document far below it.
//
// The SDK's localhost DNS-rebinding check is disabled on purpose. It compares
// the connection's local address with the request Host, and the documented
// reverse-proxy placement preserves the external Host while dialing the proxy
// over loopback, so leaving it on would reject that placement. The endpoint is
// Bearer-only and re-validated by the operator gate before the SDK sees the
// request, which is the stronger check for an authenticated endpoint.
func NewHTTPHandler(build func(*http.Request) *Service) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		service := build(r)
		if service == nil {
			// The SDK answers 400 for a nil server; it never panics on one.
			return nil
		}
		return NewServer(service)
	}, &sdk.StreamableHTTPOptions{
		Stateless:                  true,
		DisableLocalhostProtection: true,
	})
}

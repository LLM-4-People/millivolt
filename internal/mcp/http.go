package mcp

import (
	"net/http"
	"sync"

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
// The sdk.Server built from an admitted Service is memoized per credential.
// The SDK explicitly permits getServer to return the same server for repeated
// requests, and rebuilding it per POST re-registered all 22 tools and their
// JSON schemas, which measured about 6.8 MiB and 10 ms per call. The cache
// holds exactly one credential and one server: a changed credential discards
// the previous entry, so no server is ever shared across credentials. The
// credential is used only as the in-memory cache key; it is never logged or
// persisted.
//
// The SDK's localhost DNS-rebinding check is disabled on purpose. It compares
// the connection's local address with the request Host, and the documented
// reverse-proxy placement preserves the external Host while dialing the proxy
// over loopback, so leaving it on would reject that placement. The endpoint is
// Bearer-only and re-validated by the operator gate before the SDK sees the
// request, which is the stronger check for an authenticated endpoint.
func NewHTTPHandler(build func(*http.Request) *Service) http.Handler {
	var cache credentialServerCache
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		service := build(r)
		if service == nil {
			// The SDK answers 400 for a nil server; it never panics on one.
			return nil
		}
		return cache.serverFor(service.credentialKey(), func() *sdk.Server {
			return NewServer(service)
		})
	}, &sdk.StreamableHTTPOptions{
		Stateless:                  true,
		DisableLocalhostProtection: true,
	})
}

// credentialServerCache memoizes the SDK server built for one admitted
// credential. One entry is all the process can have in production (the gate
// admits a single configured token), and replacing the entry on a changed
// credential is the invalidation: a different credential can never receive the
// previous server.
type credentialServerCache struct {
	mu         sync.Mutex
	credential string
	server     *sdk.Server
}

// serverFor returns the cached server for credential, building one with make
// on a miss. A nil make result is not cached, so a failed build cannot
// poison the entry for a later request.
func (c *credentialServerCache) serverFor(credential string, make func() *sdk.Server) *sdk.Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server != nil && c.credential == credential {
		return c.server
	}
	server := make()
	if server == nil {
		return nil
	}
	c.credential, c.server = credential, server
	return server
}

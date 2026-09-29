package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// newMCPHandler builds the streamable HTTP MCP endpoint served at /mcp. It is
// Bearer-only on top of the operator gate: the gate admits the dashboard
// session cookie on gated paths, but an MCP request must present the operator
// credential as a Bearer header, because the request's own credential is what
// travels on the internal calls. A cookie-only request is denied here and can
// therefore never mint an internal call.
//
// dispatch is the proxy's own guarded handler. Each tool call becomes an
// in-process request to that handler with the caller's Authorization header
// attached, so the gate re-validates the credential on the internal call
// exactly as it did on the transport request. No loopback origin is dialed and
// nothing is derived from the request Host: the client's nominal origin is the
// named non-routable constant internal/mcp.InProcessOrigin.
func newMCPHandler(gate *operatorGate, dispatch http.Handler) http.Handler {
	// The defaults are the only limits policy on this endpoint: there is no
	// config knob for it, exactly as there is none for the stdio entrypoint's
	// defaults. The same policy bounds the Service and the in-process
	// transport's deadline.
	limits := mcp.DefaultLimits()
	streamable := mcp.NewHTTPHandler(func(r *http.Request) *mcp.Service {
		token, ok := bearerToken(r)
		if !ok || !gate.valid(token) {
			return nil
		}
		service, err := mcp.NewServiceWithTransport(mcp.InProcessOrigin, token, limits, inProcessTransport{
			dispatch: dispatch,
			timeout:  limits.Timeout,
		})
		if err != nil {
			// The wrapper validated the same credential this constructor
			// validates, so a failure here is an internal inconsistency; the
			// SDK answers 400 for the nil server rather than panicking.
			log.Printf("mcp: build request service: %v", err)
			return nil
		}
		return service
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r); !ok || !gate.valid(token) {
			denyOperator(w, http.StatusForbidden, "MCP requires the operator token as a Bearer credential; the session cookie is not accepted here")
			return
		}
		streamable.ServeHTTP(w, r)
	})
}

// inProcessTransport dispatches one synthesized operator request straight to
// the proxy's own handler. It performs no dialing and holds no address, so no
// routed path can escape to a loopback port or to another host.
type inProcessTransport struct {
	dispatch http.Handler
	// timeout is the deadline enforced when the request context carries none.
	// In production net/http wraps the context with the client's own Timeout
	// before calling a transport, so this is the explicit fallback for a
	// direct RoundTrip.
	timeout time.Duration
}

// RoundTrip implements http.RoundTripper. The dispatch runs on a bounded
// deadline: the request context's own deadline when it has one (the client's
// Timeout and the full-history reads' query timeout both arrive that way),
// otherwise the configured bound. A handler that ignores its context can no
// longer hold the request open indefinitely; the proxy's own handlers poll the
// context, so for them the observable behavior is unchanged.
func (t inProcessTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
		request = request.WithContext(ctx)
	}
	recorder := &inProcessRecorder{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.dispatch.ServeHTTP(recorder, request)
	}()
	select {
	case <-done:
		return recorder.response(request), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// inProcessRecorder is the minimal ResponseWriter the in-process dispatch
// needs. It deliberately does not implement Flusher or Hijacker: the closed
// tool route set contains no streaming or hijacking endpoint, and pretending
// to support them would hide a route-set regression instead of failing it.
type inProcessRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *inProcessRecorder) Header() http.Header { return r.header }

func (r *inProcessRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *inProcessRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

// response renders the recorded answer as the *http.Response the client
// expects. The Request field is set so relative redirect resolution and cookie
// storage see the synthesized request, even though this client refuses
// redirects.
func (r *inProcessRecorder) response(request *http.Request) *http.Response {
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     r.header,
		Body:       io.NopCloser(bytes.NewReader(r.body.Bytes())),
		Request:    request,
	}
}

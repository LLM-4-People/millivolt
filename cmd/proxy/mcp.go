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
//
// limits is the boot config's mcp category (mcp.LimitsFromConfig): the same
// policy bounds the Service and the in-process transport's deadline, and the
// values are validated at the config boundary before the boot reaches here.
func newMCPHandler(gate *operatorGate, dispatch http.Handler, limits mcp.Limits) http.Handler {
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
// otherwise the configured bound.
//
// Residual, stated plainly: the transport returns at its deadline while the
// dispatched handler may keep running. The pause, throttle, debug and quota
// handlers never read r.Context(), but their persistence runs through
// storeQueryCtx(), so each is bounded by the store timeout and the residual is
// reachable only while that timeout exceeds this transport bound. The config
// write and reload paths read no context and carry no deadline at all
// (internal/config admin WriteFile and the reload's
// RenameProviders(context.Background(), ...)), so their residual has no
// timeout bound. Every abandoned call holds one goroutine and a bounded
// recorder until the handler returns: inProcessRecorder discards every byte
// past inProcessRecorderBufferMax, so the retained body stays bounded even
// after the call is gone.
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
//
// The retained body is capped at inProcessRecorderBufferMax. The MCP client
// reads at most MaxResponseBytes and treats one byte more as over budget, so
// retaining that extra byte preserves the client's exact over-budget error for
// an oversized body while a runaway downstream handler can allocate at most
// this much; bytes past the cap are discarded, and Write still reports them
// accepted because the downstream handler owns the answer, not the recorder.
//
// The recorder owns its byte slice instead of a bytes.Buffer because Buffer's
// grown slice doubles whenever the target is under twice the current capacity:
// a full buffer needed a second budget (two times 64 MiB) to fit the last
// byte, so three abandoned calls retained about 402 MB. This growth doubles
// too, but clamps the new capacity at the budget, so live retention is about
// one budget; a small body grows the ordinary way and never reaches the clamp.
type inProcessRecorder struct {
	header http.Header
	body   []byte
	status int
}

// inProcessRecorderBufferMax is the byte budget the in-process recorder
// retains: the client's read budget plus the one byte that makes the client's
// over-budget check fire. Internal safety constant, not a tunable.
const inProcessRecorderBufferMax = mcp.MaxResponseBytes + 1

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
	written := len(data)
	remaining := inProcessRecorderBufferMax - len(r.body)
	if remaining <= 0 {
		return written, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
	}
	if need := len(r.body) + len(data); need > cap(r.body) {
		grown := cap(r.body) * 2
		if grown < need {
			grown = need
		}
		if grown > inProcessRecorderBufferMax {
			grown = inProcessRecorderBufferMax
		}
		next := make([]byte, len(r.body), grown)
		copy(next, r.body)
		r.body = next
	}
	r.body = append(r.body, data...)
	return written, nil
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
		Body:       io.NopCloser(bytes.NewReader(r.body)),
		Request:    request,
	}
}

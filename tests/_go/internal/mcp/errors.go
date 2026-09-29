package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestErrorPathsProduceReadableToolErrors pins every documented failure shape
// the proxy can answer, and that each becomes a message the model can act on
// rather than a panic or a raw Go error.
func TestErrorPathsProduceReadableToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
		ctype   string
		headers map[string]string
		want    []string
	}{
		{
			name: "unauthorized", status: http.StatusUnauthorized,
			message: "operator token required", ctype: "application/json",
			headers: map[string]string{"WWW-Authenticate": `Bearer realm="millivolt-operator"`},
			want:    []string{"401", "operator token required", "MILLIVOLT_OPERATOR_TOKEN"},
		},
		{
			name: "operator plane unarmed", status: http.StatusForbidden,
			message: "operator plane is disabled: set MILLIVOLT_OPERATOR_TOKEN to enable it",
			ctype:   "application/json",
			want:    []string{"403", "operator plane is disabled", "unarmed"},
		},
		{
			name: "not found", status: http.StatusNotFound,
			message: "debug capture not found", ctype: "text/plain; charset=utf-8",
			want: []string{"404", "debug capture not found"},
		},
		{
			name: "conflict", status: http.StatusConflict,
			message: "pause overlaps existing hold", ctype: "application/json",
			want: []string{"409", "pause overlaps existing hold"},
		},
		{
			name: "limit breach", status: http.StatusRequestEntityTooLarge,
			message: "query limit exceeded", ctype: "application/json",
			want: []string{"413", "query limit exceeded"},
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests,
			message: "too many rejected credentials; try again later", ctype: "application/json",
			headers: map[string]string{"Retry-After": "42"},
			want:    []string{"429", "retry after 42"},
		},
		{
			// The operator routes are inconsistent: some answer text/plain with
			// a JSON body. The .error field must still be read.
			name: "text/plain error body", status: http.StatusBadRequest,
			message: "clients, providers, or models required",
			ctype:   "text/plain; charset=utf-8",
			want:    []string{"400", "clients, providers, or models required"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newFakeProxy(t)
			proxy.fail(http.MethodGet, schemaPath, tc.status, tc.message, tc.ctype)
			for key, value := range tc.headers {
				proxy.respond(http.MethodGet, schemaPath, cannedResponse{
					Status: tc.status, ContentType: tc.ctype,
					Body:    `{"error":` + quote(tc.message) + `}`,
					Headers: map[string]string{key: value},
				})
			}
			service := newTestService(t, proxy, Limits{})

			_, err := service.query(context.Background(), QueryInput{SQL: "SELECT 1"})
			if err == nil {
				t.Fatal("expected a tool error")
			}
			for _, needle := range tc.want {
				if !strings.Contains(err.Error(), needle) {
					t.Fatalf("error %q must mention %q", err, needle)
				}
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error must be an *APIError, got %T", err)
			}
			if apiErr.Status != tc.status {
				t.Fatalf("status = %d, want %d", apiErr.Status, tc.status)
			}
			if tc.name == "unauthorized" && apiErr.Challenge == "" {
				t.Fatal("the WWW-Authenticate challenge must be preserved")
			}
			if tc.name == "rate limited" && apiErr.RetryAfter != "42" {
				t.Fatalf("Retry-After = %q, want 42", apiErr.RetryAfter)
			}
			assertNoToken(t, "error message", err.Error())
		})
	}
}

// TestMalformedSuccessBodyIsReadable pins that a 200 whose body is not the
// expected document becomes a readable error, not a decode panic.
func TestMalformedSuccessBodyIsReadable(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodGet, explorerPath, cannedResponse{
		Status: http.StatusOK, ContentType: "application/json", Body: `{"dim": "provider", `,
	})
	service := newTestService(t, proxy, Limits{})
	_, err := service.explore(context.Background(), ExploreInput{Dim: "provider"})
	if err == nil {
		t.Fatal("a truncated body must be an error")
	}
	for _, needle := range []string{"200", "could not decode"} {
		if !strings.Contains(err.Error(), needle) {
			t.Fatalf("error %q must mention %q", err, needle)
		}
	}
}

// TestNonJSONSuccessBodyIsReadable pins the same for a body of the wrong shape
// entirely (an HTML page from something that is not the proxy).
func TestNonJSONSuccessBodyIsReadable(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodGet, chartPath, cannedResponse{
		Status: http.StatusOK, ContentType: "text/html", Body: "<html>gateway</html>",
	})
	service := newTestService(t, proxy, Limits{})
	_, err := service.chart(context.Background(), ChartInput{Window: "5"})
	if err == nil || !strings.Contains(err.Error(), "could not decode") {
		t.Fatalf("error = %v, want a decode failure", err)
	}
}

// TestConnectionFailureIsReadable pins that an unreachable proxy is a message,
// not a panic, and that the message names the origin so the operator can see
// which one was tried.
func TestConnectionFailureIsReadable(t *testing.T) {
	// A server that is closed immediately: the address is valid, the
	// connection is refused.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	origin := dead.URL
	dead.Close()

	service, err := NewService(origin, testToken, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.query(context.Background(), QueryInput{SQL: "SELECT 1"})
	if err == nil {
		t.Fatal("an unreachable proxy must be an error")
	}
	if !strings.Contains(err.Error(), "reach millivolt") {
		t.Fatalf("error = %v, want a reachability message", err)
	}
	assertNoToken(t, "connection error", err.Error())
}

// TestRequestTimeoutIsReported pins that a slow proxy becomes a message. The
// deadline is tiny and the handler blocks on the request context, so the
// outcome does not depend on wall-clock luck.
func TestRequestTimeoutIsReported(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	limits := DefaultLimits()
	limits.Timeout = 20 * time.Millisecond
	service, err := NewService(server.URL, testToken, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.query(context.Background(), QueryInput{SQL: "SELECT 1"}); err == nil {
		t.Fatal("a request that outlives the timeout must be an error")
	}
}

// TestExplorerReadIsBoundedByQueryTimeout pins that the explorer fold, like the
// chart, is bounded by the configured QueryTimeout rather than only by the
// generic client timeout. Both are full-history reads, and the --query-timeout
// help and docs/mcp.md both state the bound covers the explorer.
func TestExplorerReadIsBoundedByQueryTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	limits := DefaultLimits()
	limits.QueryTimeout = 25 * time.Millisecond
	limits.Timeout = 5 * time.Second
	service, err := NewService(server.URL, testToken, limits)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = service.explore(context.Background(), ExploreInput{Dim: "provider"})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("an explorer read that outlives the query timeout must be an error")
	}
	if elapsed > limits.Timeout/2 {
		t.Fatalf("the explorer read took %v; QueryTimeout (%v) must bound it, not the generic Timeout (%v)",
			elapsed, limits.QueryTimeout, limits.Timeout)
	}
}

// TestFullHistoryReadIsNotCappedByTheGenericTimeout is the regression for the
// silent cap: chart and explorer carry a QueryTimeout context, but the shared
// http.Client{Timeout: limits.Timeout} used to cut every call at the shorter
// generic bound, so the documented 2m query timeout was really 30s. A read
// slower than Timeout but inside QueryTimeout must succeed, and an ordinary
// call must still be cut at Timeout.
func TestFullHistoryReadIsNotCappedByTheGenericTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dim":"provider","groups":[],"rail":{},"scope":{}}`))
	}))
	defer slow.Close()

	limits := DefaultLimits()
	limits.Timeout = 50 * time.Millisecond
	limits.QueryTimeout = 5 * time.Second
	service, err := NewService(slow.URL, testToken, limits)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := service.explore(context.Background(), ExploreInput{Dim: "provider"}); err != nil {
		t.Fatalf("a read inside QueryTimeout (%v) must not be cut by Timeout (%v): %v",
			limits.QueryTimeout, limits.Timeout, err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("the read returned after %v; the handler was still working, so it cannot be the measured bound", elapsed)
	}

	// The same blocking handler bounds an ordinary call at the generic Timeout.
	limits.Timeout = 25 * time.Millisecond
	ordinary, err := NewService(slow.URL, testToken, limits)
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if _, err := ordinary.query(context.Background(), QueryInput{SQL: "SELECT 1"}); err == nil {
		t.Fatal("an ordinary call that outlives Timeout must fail")
	}
	if elapsed := time.Since(started); elapsed >= limits.Timeout+100*time.Millisecond {
		t.Fatalf("the ordinary call took %v; Timeout (%v) must bound it, not the handler", elapsed, limits.Timeout)
	}
}

// TestRetryAfterIsSanitized pins that the Retry-After header is never copied
// into a model-visible message verbatim. It is endpoint-controlled text, and
// `retry after <value>` used to print it unbounded.
func TestRetryAfterIsSanitized(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"seconds", "42", "42"},
		{"padded seconds", "  42  ", "42"},
		{"negative seconds", "-42", ""},
		{"signed seconds", "+42", ""},
		{"http date", "Wed, 21 Oct 2015 07:28:00 GMT", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"arbitrary text", "call the operator at once", ""},
		{"credential", testToken, ""},
		{"unbounded digits", strings.Repeat("9", 40), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newFakeProxy(t)
			proxy.respond(http.MethodGet, schemaPath, cannedResponse{
				Status: http.StatusTooManyRequests, ContentType: "application/json",
				Body:    `{"error":"too many rejected credentials"}`,
				Headers: map[string]string{"Retry-After": tc.raw},
			})
			service := newTestService(t, proxy, Limits{})
			_, err := service.query(context.Background(), QueryInput{SQL: "SELECT 1"})
			if err == nil {
				t.Fatal("expected the 429")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error must be an *APIError, got %T", err)
			}
			if apiErr.RetryAfter != tc.want {
				t.Fatalf("Retry-After = %q, want %q", apiErr.RetryAfter, tc.want)
			}
			assertNoToken(t, "429 error", err.Error())
		})
	}
}

// TestErrorBodyExcerptIsBounded pins that an unexpected large non-JSON body is
// excerpted rather than pasted into a model context in full.
func TestErrorBodyExcerptIsBounded(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodGet, schemaPath, cannedResponse{
		Status: http.StatusBadGateway, ContentType: "text/html",
		Body: "<html>" + strings.Repeat("upstream failure ", 4096) + "</html>",
	})
	service := newTestService(t, proxy, Limits{})
	_, err := service.query(context.Background(), QueryInput{SQL: "SELECT 1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > maxErrorBodyBytes+256 {
		t.Fatalf("error message is %d bytes; it must stay near the %d byte excerpt budget", len(err.Error()), maxErrorBodyBytes)
	}
	if !strings.Contains(err.Error(), "[truncated]") {
		t.Fatalf("a bounded excerpt must say it was truncated: %v", err)
	}

	// The flat {"error": "..."} shape is bounded too: it used to bypass the
	// excerpt entirely and could carry a multi-megabyte field straight into a
	// model context.
	proxy.respond(http.MethodGet, explorerPath, cannedResponse{
		Status: http.StatusBadGateway, ContentType: "application/json",
		Body: `{"error":"` + strings.Repeat("upstream failure ", 4096) + `"}`,
	})
	_, err = service.explore(context.Background(), ExploreInput{Dim: "provider"})
	if err == nil {
		t.Fatal("expected the flat error body to fail")
	}
	if len(err.Error()) > maxErrorBodyBytes+256 {
		t.Fatalf("flat error message is %d bytes; it must stay near the %d byte excerpt budget", len(err.Error()), maxErrorBodyBytes)
	}
	if !strings.Contains(err.Error(), "[truncated]") {
		t.Fatalf("a bounded flat error must say it was truncated: %v", err)
	}
}

// TestOversizedFailureBodyDecodeIsBounded is the regression for the unbounded
// failure-body decode: a 64 MiB nested JSON body was decoded in full before
// excerpting, about 128 MiB allocated and 200 ms spent to produce the same
// 4 KiB message. failureExcerpt now bounds both inputs: the stream decode sees
// at most maxExcerptScanBytes and stops at the excerpt target, while redaction
// and truncation keep working on the result.
func TestOversizedFailureBodyDecodeIsBounded(t *testing.T) {
	body := []byte(`{"detail":"upstream refused ` + testToken + ` "` + strings.Repeat("x", 64<<20) + `"}`)
	resp := &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	apiErr := newAPIError(resp, body, testToken)
	runtime.ReadMemStats(&after)

	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("the 64 MiB failure body allocated %d bytes; the decode must be bounded to the excerpt region", allocated)
	}
	if len(apiErr.Message) > maxErrorBodyBytes+len(" [truncated]") {
		t.Fatalf("the excerpt is %d bytes, above the %d byte budget", len(apiErr.Message), maxErrorBodyBytes)
	}
	if !strings.Contains(apiErr.Message, "upstream refused") || !strings.Contains(apiErr.Message, "[truncated]") {
		t.Fatalf("the excerpt must keep the leading diagnostics and state the truncation: %q", apiErr.Message)
	}
	if strings.Contains(apiErr.Message, testToken) {
		t.Fatalf("the credential inside the oversized body survived: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, redactionMarker) {
		t.Fatalf("the redaction must stay visible in the excerpt: %q", apiErr.Message)
	}
}

// TestValidOversizedFailureBodyKeepsTheDecodedMessage is the regression for the
// region-only decode: a valid JSON body larger than the excerpt region used to
// be decoded in full, and bounding that decode to the raw region cut the
// document mid-string, so the flat {"error": ...} message degraded to raw JSON
// text. The bounded stream decode must keep the message for any body size, and
// still must not allocate for the body it does not need.
func TestValidOversizedFailureBodyKeepsTheDecodedMessage(t *testing.T) {
	const message = "the model did not exist"
	body := []byte(`{"error":"` + message + `","junk":"` + strings.Repeat("x", 64<<20) + `"}`)
	resp := &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	apiErr := newAPIError(resp, body, testToken)
	runtime.ReadMemStats(&after)

	if apiErr.Message != message {
		t.Fatalf("message = %q, want the decoded %q", apiErr.Message, message)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("the 64 MiB valid failure body allocated %d bytes; the decode must stop at the excerpt budget", allocated)
	}
}

// TestFlatErrorValueWinsBeforeLaterStringValues pins the flat {"error": ...}
// early return in streamedFailureText: once the error key's value is read the
// walk returns it and never joins later string values. The junk value is
// inside the scan cap, so without the early return the same body would produce
// "m xxxxxxxx" instead of the proxy's own message.
func TestFlatErrorValueWinsBeforeLaterStringValues(t *testing.T) {
	const body = `{"error":"m","junk":"xxxxxxxx"}`
	if message := failureMessage(t, testToken, body); message != "m" {
		t.Fatalf("message = %q, want exactly %q", message, "m")
	}
}

// TestValidOversizedNonFlatValueDecodeIsBounded pins the scan-cap input bound
// when no flat error key short-circuits the walk: the first value is one 64 MiB
// string, which json.Decoder materializes in full before the budget check.
// failureExcerpt must pass the capped prefix to the decoder, not the whole
// body, so the allocation stays near the cap rather than the body size and the
// cut value degrades to the raw region instead of losing the leading text.
func TestValidOversizedNonFlatValueDecodeIsBounded(t *testing.T) {
	const prefix = `{"detail":"`
	const diagnostic = "upstream refused"
	body := []byte(prefix + diagnostic + strings.Repeat("x", 64<<20) + `"}`)
	resp := &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	apiErr := newAPIError(resp, body, testToken)
	runtime.ReadMemStats(&after)

	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("the 64 MiB non-flat failure body allocated %d bytes; the decode must be bounded to the %d byte scan cap",
			allocated, maxExcerptScanBytes)
	}
	if len(apiErr.Message) > maxErrorBodyBytes+len(" [truncated]") {
		t.Fatalf("the excerpt is %d bytes, above the %d byte budget", len(apiErr.Message), maxErrorBodyBytes)
	}
	if !strings.HasPrefix(apiErr.Message, prefix) || !strings.Contains(apiErr.Message, diagnostic) {
		t.Fatalf("the excerpt must keep the leading body text: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "[truncated]") {
		t.Fatalf("the bounded excerpt must state the truncation: %q", apiErr.Message)
	}
}

// TestRefusedRedirectReplaysNothing is the credential-replay guard. net/http
// strips Authorization only when the HOSTNAME changes, and the port is not part
// of that comparison, so a same-host different-port or subdomain 307/308 replays
// both the credential and the full POST body to the redirect target. The proxy
// redirects no operator route, so refusing every redirect removes the question.
func TestRefusedRedirectReplaysNothing(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// The sink is a second listener: anything it receives was replayed.
			sink := newSink(t)
			upstream := newFakeProxy(t)
			// A body-carrying POST, so a replay would carry the request body as
			// well as the credential.
			upstream.respond(http.MethodPost, reloadPath, cannedResponse{
				Status:      status,
				ContentType: "text/plain",
				Headers:     map[string]string{"Location": sink.url() + "/admin/reload"},
				Body:        "moved\n",
			})
			// The proxy this client trusts is on the sink's own host, so a
			// hostname-only redirect check would have replayed the credential.
			service := newTestService(t, upstream, Limits{})
			_, err := service.reloadConfig(context.Background(), ReloadConfigInput{})
			if err == nil {
				t.Fatal("a redirect must be a clean tool error, never a success")
			}
			if !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("error = %v, want the redirect refusal", err)
			}
			if !strings.Contains(err.Error(), "credential") {
				t.Fatalf("the refusal must say why following it is unsafe: %v", err)
			}
			assertNoToken(t, "redirect error", err.Error())
			if got := sink.requests(); len(got) != 0 {
				t.Fatalf("the redirect target received %d requests, want none: %+v", len(got), got[0])
			}
		})
	}
	// A redirect on a read is refused the same way.
	sink := newSink(t)
	upstream := newFakeProxy(t)
	upstream.respond(http.MethodGet, explorerPath, cannedResponse{
		Status: http.StatusTemporaryRedirect, ContentType: "text/plain",
		Headers: map[string]string{"Location": sink.url() + "/metrics/agg/explorer"}, Body: "moved\n",
	})
	service := newTestService(t, upstream, Limits{})
	if _, err := service.explore(context.Background(), ExploreInput{Dim: "provider"}); err == nil ||
		!strings.Contains(err.Error(), "redirect") {
		t.Fatalf("a read redirect must be refused too, got %v", err)
	}
	if len(sink.requests()) != 0 {
		t.Fatal("the redirect target received a request")
	}
}

// TestErrorBodyIsRedactedOfTheCredential pins that the redaction covers the
// failure BODY, not only the transport error. An endpoint that reflects the
// Authorization header into its own error would otherwise hand the credential to
// the model verbatim, and everything this server returns is logged and pasted
// elsewhere.
func TestErrorBodyIsRedactedOfTheCredential(t *testing.T) {
	// The fake echoes the credential into its own body, which is the worst case
	// a transport can face.
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodGet, explorerPath, cannedResponse{
		Status: http.StatusInternalServerError, ContentType: "application/json",
		Body: `{"error":"upstream repeated ` + testToken + ` in a header"}`,
	})
	proxy.respond(http.MethodGet, schemaPath, cannedResponse{
		Status: http.StatusBadGateway, ContentType: "text/html",
		Body: "<html><body>Authorization: Bearer " + testToken + "</body></html>",
	})
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	_, err := service.explore(ctx, ExploreInput{Dim: "provider"})
	if err == nil {
		t.Fatal("expected the echoed failure")
	}
	assertNoToken(t, "flat .error body", err.Error())
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("the redaction must be visible, not silent removal: %q", err)
	}
	// The non-JSON excerpt path too: the proxy's own .error field is absent
	// there, so a different branch produces the message.
	_, err = service.query(ctx, QueryInput{SQL: "SELECT 1"})
	if err == nil {
		t.Fatal("expected the reflected failure")
	}
	assertNoToken(t, "excerpted body", err.Error())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error must be an *APIError, got %T", err)
	}
	assertNoToken(t, "APIError.Message", apiErr.Message)
}

// TestErrorBodyCredentialStraddlingTheExcerptBoundaryIsRedacted is the
// regression for redaction running AFTER the excerpt bound: a credential that
// straddles the 4 KiB boundary was cut in half before the full token was
// matched, so a recognizable fragment of the credential survived in the tool
// message. The full message must be redacted first and bounded second, for
// both the structured {"error": ...} shape and the non-JSON excerpt.
func TestErrorBodyCredentialStraddlingTheExcerptBoundaryIsRedacted(t *testing.T) {
	// A distinctive credential, so a surviving fragment is unambiguous: none
	// of the padding characters occurs in it, and the service is built with
	// this token rather than the shared fixture.
	const token = "QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ"
	for _, shape := range []struct {
		name string
		body func(string) string
	}{
		{"structured", func(message string) string { return `{"error":` + quote(message) + `}` }},
		{"excerpt", func(message string) string { return message }},
	} {
		for offset := 4094; offset <= 4098; offset++ {
			t.Run(shape.name+"/"+strconv.Itoa(offset), func(t *testing.T) {
				proxy := newFakeProxy(t)
				proxy.respond(http.MethodGet, explorerPath, cannedResponse{
					Status: http.StatusBadGateway, ContentType: "application/json",
					Body: shape.body(strings.Repeat("X", offset) + token + strings.Repeat("Y", 5000)),
				})
				service, err := NewService(proxy.origin(), token, DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				_, err = service.explore(context.Background(), ExploreInput{Dim: "provider"})
				if err == nil {
					t.Fatal("expected the upstream failure")
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("error must be an *APIError, got %T", err)
				}
				if strings.Contains(apiErr.Message, "Q") || strings.Contains(err.Error(), token) {
					t.Fatalf("a credential fragment survived the excerpt boundary: %q", apiErr.Message)
				}
			})
		}
	}
}

// failureMessage drives one failure body through the client and returns the
// model-visible message. It fails the test when the call does not fail as an
// *APIError, so every redaction case shares one error path.
func failureMessage(t *testing.T, token, body string) string {
	t.Helper()
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodGet, explorerPath, cannedResponse{
		Status: http.StatusBadGateway, ContentType: "application/json", Body: body,
	})
	service, err := NewService(proxy.origin(), token, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.explore(context.Background(), ExploreInput{Dim: "provider"})
	if err == nil {
		t.Fatal("expected the upstream failure")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error must be an *APIError, got %T", err)
	}
	return apiErr.Message
}

// assertNoCredentialFragment fails when any contiguous run of
// min(minRedactionRun, len(token)) bytes of the credential survives anywhere in
// the message. The window comes from the redaction owner, so raising or
// lowering the fragment floor changes both what redaction removes and what
// this helper guards; a hardcoded floor would drift from it silently.
func assertNoCredentialFragment(t *testing.T, token, where, message string) {
	t.Helper()
	window := min(minRedactionRun, len(token))
	for start := 0; start+window <= len(token); start++ {
		if fragment := token[start : start+window]; strings.Contains(message, fragment) {
			t.Fatalf("%s leaked the credential fragment %q: %q", where, fragment, message)
		}
	}
}

// assertNoStrippedCredentialFragment fails when the whitespace-free view of the
// message still contains a floor-length run of the whitespace-free credential.
// A contiguous byte check cannot see an echo that interleaves whitespace
// between every credential character, and such an echo is trivially
// reconstructible by anyone reading the excerpt.
func assertNoStrippedCredentialFragment(t *testing.T, token, where, message string) {
	t.Helper()
	strippedToken := strings.Join(strings.Fields(token), "")
	strippedMessage := strings.Join(strings.Fields(message), "")
	window := min(minRedactionRun, len(strippedToken))
	for start := 0; start+window <= len(strippedToken); start++ {
		if fragment := strippedToken[start : start+window]; strings.Contains(strippedMessage, fragment) {
			t.Fatalf("%s leaked the interleaved credential fragment %q: %q", where, fragment, message)
		}
	}
}

// assertRedacted fails when the credential, any contiguous 8-byte fragment of
// it, or any named encoded form survives, and when the visible marker is
// absent. It is the one assertion every redaction case shares.
func assertRedacted(t *testing.T, token, where, message string, forms ...string) {
	t.Helper()
	if strings.Contains(message, token) {
		t.Fatalf("%s leaked the credential: %q", where, message)
	}
	assertNoCredentialFragment(t, token, where, message)
	for _, form := range forms {
		if strings.Contains(message, form) {
			t.Fatalf("%s leaked the credential form %q: %q", where, form, message)
		}
	}
	if !strings.Contains(message, "[redacted]") {
		t.Fatalf("%s must show the redaction marker: %q", where, message)
	}
}

// TestErrorBodyRedactionCoversCredentialForms is the regression for three
// reflection shapes the whole-token replacement missed: a credential echoed
// with different whitespace (the whitespace collapse turned the echo back into
// the credential AFTER redaction had already run), the percent-encoded
// credential, and a credential split across two JSON string fields whose
// fragments the whole-token match can never see.
func TestErrorBodyRedactionCoversCredentialForms(t *testing.T) {
	// A multi-word credential, so every whitespace-separated piece is shorter
	// than the fragment floor: only the post-collapse redaction can catch the
	// tabbed echo. Deliberately not the shared fixture.
	const token = "alpha beta gamma delta"

	t.Run("whitespace variant", func(t *testing.T) {
		tabbed := strings.ReplaceAll(token, " ", "\t")
		message := failureMessage(t, token, `{"error":`+quote("upstream echoed "+tabbed)+`}`)
		assertRedacted(t, token, "whitespace echo", message)
	})

	t.Run("percent encoded", func(t *testing.T) {
		pathForm := url.PathEscape(token)
		queryForm := url.QueryEscape(token)
		message := failureMessage(t, token, "upstream echoed "+pathForm+" and "+queryForm)
		assertRedacted(t, token, "percent-encoded echo", message, pathForm, queryForm)
	})

	t.Run("split across two JSON fields", func(t *testing.T) {
		first, second := token[:12], token[12:]
		// The nested shape is not the flat {"error": "..."} document, so the
		// whole body is excerpted and BOTH fragments reach the message.
		message := failureMessage(t, token, `{"first":`+quote(first)+`,"second":`+quote(second)+`}`)
		assertRedacted(t, token, "split echo", message)
	})

	t.Run("control body unchanged", func(t *testing.T) {
		const control = "the upstream refused the request"
		message := failureMessage(t, token, `{"error":`+quote(control)+`}`)
		if message != control {
			t.Fatalf("a body with no credential must pass through unchanged, got %q", message)
		}
	})
}

// TestErrorBodyRedactionCoversInterleavedWhitespace is the regression for a
// reflection that interleaves whitespace between every credential character:
// the exact-token and encoded-form matchers never see such an echo, and the
// collapse only turns it into single spaces between every character, which is
// still not the credential. A contiguous fragment check does not catch it
// either, yet the echo is trivially reconstructible. The scan must match the
// credential against a whitespace-stripped view of the text and redact the raw
// span the match came from, whitespace included.
func TestErrorBodyRedactionCoversInterleavedWhitespace(t *testing.T) {
	const token = "k9+Qf/2 bZ=x7?Lm3+qA"
	interleave := func(form, separator string) string {
		var out strings.Builder
		for index, r := range form {
			if index > 0 {
				out.WriteString(separator)
			}
			out.WriteRune(r)
		}
		return out.String()
	}
	percent := url.PathEscape(token)
	jsonForm := jsonEscapeForm(token, hexMixed)
	split := len(token) / 2
	mixed := interleave(url.PathEscape(token[:split]), " ") + interleave(jsonEscapeForm(token[split:], hexUpper), "\n")

	for _, tc := range []struct {
		name string
		form string
	}{
		{"spaces", interleave(token, " ")},
		{"tabs", interleave(token, "\t")},
		{"newlines", interleave(token, "\n")},
		{"mixed whitespace", interleave(token, " \t\n")},
		{"percent encoded with spaces", interleave(percent, " ")},
		{"json escapes with tabs", interleave(jsonForm, "\t")},
		{"mixed encoding with interleaving", mixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := failureMessage(t, token, `{"detail":`+quote("upstream echoed "+tc.form)+`}`)
			assertRedacted(t, token, tc.name, message, tc.form)
			assertNoStrippedCredentialFragment(t, token, tc.name, message)
		})
	}

	// The config patch path decodes its own response body and shares the same
	// scan, so an interleaved echo in a committed reload failure must be
	// redacted there too.
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodPost, configPath, cannedResponse{
		Status: http.StatusInternalServerError, ContentType: "application/json",
		Body: `{"saved":true,"revision":"z","values":{},"restart_required":[],"error":` +
			quote("reload failed for "+interleave(token, " ")) + `}`,
	})
	service, err := NewService(proxy.origin(), token, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out, err := service.setConfig(context.Background(), SetConfigInput{
		Values: map[string]any{"history_size": 5}, Revision: "mine",
	})
	if err != nil {
		t.Fatalf("a committed mutation must not be reported as an error: %v", err)
	}
	assertRedacted(t, token, "set_config interleaved error", out.Error)
	assertNoStrippedCredentialFragment(t, token, "set_config interleaved error", out.Error)
}

// hexCase selects the case an encoded form's hex digits are written in. A
// reflection can lower, upper or mix the hex of percent and \u escapes, and
// the matcher must treat the case as insignificant.
type hexCase int

const (
	hexUpper hexCase = iota
	hexLower
	hexMixed
)

// foldHex rewrites the case of the two hex digits in every percent escape of a
// form, leaving the escaped byte and every literal character alone.
func foldHex(form string, casing hexCase) string {
	var out strings.Builder
	out.Grow(len(form))
	escape := 0
	for index := 0; index < len(form); {
		if form[index] == '%' && index+2 < len(form) {
			pair := form[index+1 : index+3]
			switch casing {
			case hexUpper:
				pair = strings.ToUpper(pair)
			case hexLower:
				pair = strings.ToLower(pair)
			case hexMixed:
				if escape%2 == 0 {
					pair = strings.ToLower(pair)
				} else {
					pair = strings.ToUpper(pair)
				}
			}
			out.WriteByte('%')
			out.WriteString(pair)
			index += 3
			escape++
			continue
		}
		out.WriteByte(form[index])
		index++
	}
	return out.String()
}

// jsonEscapeForm renders every rune of the credential as a JSON \u escape.
func jsonEscapeForm(token string, casing hexCase) string {
	var out strings.Builder
	escape := 0
	for _, r := range token {
		format := "\\u%04X"
		switch {
		case casing == hexLower:
			format = "\\u%04x"
		case casing == hexMixed && escape%2 == 1:
			format = "\\u%04x"
		}
		fmt.Fprintf(&out, format, r)
		escape++
	}
	return out.String()
}

// TestErrorBodyRedactionCoversEncodedCredentialForms is the regression for the
// encodings an exact-form replacement cannot see: percent escapes in lower or
// mixed case hex, the query + form, double encoding, and JSON \u escapes in a
// structured body whose string fields were never decoded (only the flat
// {"error": ...} document was). Each surviving form is asserted absent by its
// own text, because an encoded echo carries no plain 8-byte credential run.
func TestErrorBodyRedactionCoversEncodedCredentialForms(t *testing.T) {
	// Every character class an encoding rewrites: a plus and a slash (the
	// base64 alphabet), a space (the query + form), and an equals sign.
	const token = "k9+Qf/2 bZ=x7?Lm3+qA"

	structured := func(value string) string { return `{"detail":` + quote(value) + `}` }
	percent := url.PathEscape(token)
	query := url.QueryEscape(token)
	doubled := strings.ReplaceAll(percent, "%", "%25")

	splitFirst, splitSecond := token[:10], token[10:]
	splitFirstForm := foldHex(url.PathEscape(splitFirst), hexLower)
	splitSecondForm := jsonEscapeForm(splitSecond, hexUpper)

	for _, tc := range []struct {
		name  string
		body  string
		forms []string
	}{
		{
			name:  "percent lower hex",
			body:  structured("upstream echoed " + foldHex(percent, hexLower)),
			forms: []string{foldHex(percent, hexLower)},
		},
		{
			name:  "percent mixed hex",
			body:  structured("upstream echoed " + foldHex(percent, hexMixed)),
			forms: []string{foldHex(percent, hexMixed)},
		},
		{
			name:  "query plus form with lower hex",
			body:  structured("upstream echoed " + foldHex(query, hexLower)),
			forms: []string{foldHex(query, hexLower)},
		},
		{
			name:  "double encoded",
			body:  structured("upstream echoed " + doubled),
			forms: []string{doubled},
		},
		{
			name:  "double encoded mixed hex",
			body:  structured("upstream echoed " + foldHex(doubled, hexMixed)),
			forms: []string{foldHex(doubled, hexMixed)},
		},
		{
			// The structured string field carries the escapes in the JSON
			// document itself; only decoding that field before matching sees
			// the credential.
			name:  "json escapes upper hex",
			body:  `{"detail":"upstream echoed ` + jsonEscapeForm(token, hexUpper) + `"}`,
			forms: []string{jsonEscapeForm(token, hexUpper)},
		},
		{
			name:  "json escapes mixed hex",
			body:  `{"detail":"upstream echoed ` + jsonEscapeForm(token, hexMixed) + `"}`,
			forms: []string{jsonEscapeForm(token, hexMixed)},
		},
		{
			// The endpoint JSON-encodes text that already carries \u escape
			// text, so the decoded field holds a literal backslash-u form that
			// the matcher must decode itself.
			name:  "literal json escape text",
			body:  structured("upstream echoed " + jsonEscapeForm(token, hexUpper)),
			forms: []string{jsonEscapeForm(token, hexUpper)},
		},
		{
			name:  "flat body with lower hex percent",
			body:  `{"error":` + quote("upstream echoed "+foldHex(percent, hexLower)) + `}`,
			forms: []string{foldHex(percent, hexLower)},
		},
		{
			name:  "plain text body with double encoding",
			body:  "upstream echoed " + foldHex(doubled, hexLower),
			forms: []string{foldHex(doubled, hexLower)},
		},
		{
			name: "field pair split after decoding",
			// Each field decodes to one half of the credential: only
			// unescaping the fields and decoding each view can see either
			// half, and each half is long enough to matter on its own.
			body:  `{"first":` + quote(splitFirstForm) + `,"second":` + quote(splitSecondForm) + `}`,
			forms: []string{splitFirstForm, splitSecondForm},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := failureMessage(t, token, tc.body)
			assertRedacted(t, token, tc.name, message, tc.forms...)
		})
	}
}

// percentNest renders every byte of s as a percent escape, depth times over:
// the full-byte nested form a gateway produces by escaping an already escaped
// value. Each pass escapes every byte of the previous pass, including the `%`
// itself, so depth 2 of `k` is `%25%36%42` rather than the contiguous
// `%256b` a one-escape peel also handles.
func percentNest(s string, depth int) string {
	for range depth {
		var out strings.Builder
		for _, b := range []byte(s) {
			fmt.Fprintf(&out, "%%%02X", b)
		}
		s = out.String()
	}
	return s
}

// doubleUnescape applies the query unescape an operator (or a model) would
// apply twice, so a published excerpt that only decodes back to the credential
// after a second pass is still a leak.
func doubleUnescape(s string) string {
	once, err := url.QueryUnescape(s)
	if err != nil {
		return s
	}
	twice, err := url.QueryUnescape(once)
	if err != nil {
		return once
	}
	return twice
}

// TestErrorBodyRedactionCoversFullByteNestedPercent is the regression for the
// one-layer-short peel: full-byte nested percent encoding at depths 2 and 3
// used to pass through untouched, because peeling a single escape can only
// follow `%2520`-style contiguity and never the `%25%36%42` form. The first
// maxPercentLayers depths must be removed entirely; anything deeper must fail
// closed with the excerpt replaced, because publishing the undecoded remainder
// would be exactly the leak the bound exists to stop.
func TestErrorBodyRedactionCoversFullByteNestedPercent(t *testing.T) {
	const token = "k9+Qf/2 bZ=x7?Lm3+qA"
	for _, tc := range []struct {
		depth      int
		failClosed bool
	}{
		{depth: 1},
		{depth: 2},
		{depth: 3},
		{depth: 4, failClosed: true},
		{depth: 5, failClosed: true},
	} {
		t.Run(strconv.Itoa(tc.depth)+" layers", func(t *testing.T) {
			encoded := percentNest(token, tc.depth)
			message := failureMessage(t, token, "upstream echoed "+encoded)
			if tc.failClosed {
				if message != redactionMarker {
					t.Fatalf("nesting past the %d layer bound must replace the excerpt with %q, got %q",
						maxPercentLayers, redactionMarker, message)
				}
				return
			}
			assertRedacted(t, token, "full-byte nested echo", message, encoded)
			if decoded := doubleUnescape(message); strings.Contains(decoded, token) {
				t.Fatalf("two decodes of the published message recovered the credential: %q", decoded)
			}
		})
	}

	// The fail-closed rule must also hold when the literal credential is
	// present beside the too-deep form: redacting the literal and publishing
	// the rest would still leak.
	encoded := percentNest(token, maxPercentLayers+1)
	message := failureMessage(t, token, `{"error":`+quote("upstream echoed "+encoded+" and "+token)+`}`)
	if message != redactionMarker {
		t.Fatalf("a deeper form beside the literal credential must fail closed, got %q", message)
	}
}

// TestErrorBodyRedactionCoversDoubleQueryEscape is the regression for the
// plus-space pass that only mapped raw `+` characters. A credential escaped
// twice by url.QueryEscape carries its space as the literal `%2B` of the inner
// escape, so the first 8 bytes (a usable secret at the project's fragment
// floor) survived as text that two query-unescapes recovered.
func TestErrorBodyRedactionCoversDoubleQueryEscape(t *testing.T) {
	const token = "k9+Qf/2 bZ=x7?Lm3+qA"
	encoded := url.QueryEscape(url.QueryEscape(token))
	message := failureMessage(t, token, `{"error":`+quote("upstream echoed "+encoded)+`}`)
	assertRedacted(t, token, "double QueryEscape echo", message, encoded)
	decoded := doubleUnescape(message)
	if !strings.Contains(message, redactionMarker) {
		t.Fatalf("the double-escaped echo must be redacted: %q", message)
	}
	if strings.Contains(decoded, token[:minRedactionRun]) {
		t.Fatalf("two query-unescapes of the published message expose the credential's first %d bytes: %q",
			minRedactionRun, decoded)
	}
}

// TestErrorBodyRedactionCoversRePercentedPathEscape covers a mixed depth-2
// form: url.PathEscape output (literal `+`, escaped space, slash, equals and
// question mark) with every byte then percent-escaped again. Only part of the
// credential is contiguous under a one-layer peel, so this is the shape that
// needs the decoding pipeline rather than a deeper single-escape peel.
func TestErrorBodyRedactionCoversRePercentedPathEscape(t *testing.T) {
	const token = "k9+Qf/2 bZ=x7?Lm3+qA"
	encoded := percentNest(url.PathEscape(token), 1)
	message := failureMessage(t, token, "upstream echoed "+encoded)
	assertRedacted(t, token, "re-percented PathEscape echo", message, encoded)
}

// TestErrorBodyRedactionCoversDoubleJSONEscape covers a plain-text body whose
// credential was JSON-escaped once and then escaped again, so the backslashes
// are doubled and one pass of \u decoding only yields the inner escape text.
// The decoding pipeline runs the next pass over that text and removes it.
func TestErrorBodyRedactionCoversDoubleJSONEscape(t *testing.T) {
	const token = "k9+Qf/2 bZ=x7?Lm3+qA"
	twice := strings.ReplaceAll(jsonEscapeForm(token, hexUpper), `\`, `\\`)
	message := failureMessage(t, token, "upstream echoed "+twice)
	assertRedacted(t, token, "double JSON-escaped echo", message, twice)
}

// TestNestedFailureExcerptIsDeterministic pins that a nested-JSON failure body
// is excerpted in a stable field order. The walk once iterated the decoded Go
// map, whose order is randomized per run, so the same response produced
// different model-visible messages; repeating one response must give
// byte-identical output.
func TestNestedFailureExcerptIsDeterministic(t *testing.T) {
	body := `{"zeta":"upstream said ","alpha":"the request was refused because ",` +
		`"middle":"the key was stale","beta":"and the window had closed"}`
	proxy := newFakeProxy(t)
	proxy.respond(http.MethodGet, explorerPath, cannedResponse{
		Status: http.StatusBadGateway, ContentType: "application/json", Body: body,
	})
	service := newTestService(t, proxy, Limits{})

	var first string
	for attempt := 0; attempt < 32; attempt++ {
		_, err := service.explore(context.Background(), ExploreInput{Dim: "provider"})
		if err == nil {
			t.Fatal("expected the nested failure body")
		}
		if attempt == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("the same response produced a different excerpt on attempt %d:\nfirst: %q\n now:  %q",
				attempt, first, err.Error())
		}
	}
	if !strings.Contains(first, "refused") {
		t.Fatalf("the excerpt lost the proxy's text: %q", first)
	}
}

// assertExcerptScanBounded pins the raw work bound on the excerpt walk: the
// scan stops at maxExcerptScanBytes and may only finish the rune that starts
// there, so the bytes it reports examining never exceed the cap by more than
// one UTF-8 rune. The counter is the guard; a wall-clock budget alone cannot
// prove the bound because the fail-closed output is identical either way.
func assertExcerptScanBounded(t *testing.T, examined int) {
	t.Helper()
	if limit := maxExcerptScanBytes + utf8.UTFMax - 1; examined > limit {
		t.Fatalf("the excerpt scan examined %d raw bytes, above the %d byte cap plus one rune (%d)",
			examined, maxExcerptScanBytes, limit)
	}
}

// TestRedactionScansOnlyTheExcerptRegion pins the work bound on the redaction
// that runs before the excerpt truncation: the scan covers only the region
// that can reach the published excerpt, not the whole body. A fixed raw byte
// cut would be both too wide (scanning unreachable text) and too narrow (a
// long whitespace run can pull a later credential into the excerpt), so the
// region is found by walking the collapse itself.
func TestRedactionScansOnlyTheExcerptRegion(t *testing.T) {
	const token = testToken
	target := maxErrorBodyBytes + credentialFormByteMax*len(token)

	// A multi-megabyte body with no credential: the region stops near the
	// excerpt budget instead of covering the message.
	message := strings.Repeat("word ", 1<<20)
	region, complete, examined := excerptRegion(message, target)
	if !complete {
		t.Fatal("a body whose collapse reaches the target inside the cap must not fail closed")
	}
	assertExcerptScanBounded(t, examined)
	if examined != len(region) {
		t.Fatalf("the scan reported %d examined bytes for a %d byte region", examined, len(region))
	}
	if len(region) >= len(message) {
		t.Fatalf("the region covers the whole %d byte message", len(message))
	}
	if len(region) > 1<<16 {
		t.Fatalf("the region is %d bytes for a %d byte message", len(region), len(message))
	}
	if collapsed := strings.Join(strings.Fields(region), " "); len(collapsed) < maxErrorBodyBytes {
		t.Fatalf("the region collapses to %d bytes, below the excerpt budget", len(collapsed))
	}

	// Whitespace collapse can pull a credential into the excerpt from far
	// beyond any fixed raw cut, so the region has to cross the run.
	hidden := strings.Repeat(" ", 1<<20) + token
	region, complete, examined = excerptRegion(hidden, target)
	if !complete {
		t.Fatal("a whitespace-hidden credential inside the scan cap must not fail closed")
	}
	assertExcerptScanBounded(t, examined)
	if examined != len(region) {
		t.Fatalf("the scan reported %d examined bytes for a %d byte region", examined, len(region))
	}
	if !strings.Contains(region, token) {
		t.Fatal("the region must include a credential that whitespace collapse pulls into the excerpt")
	}
	redacted := redactForExcerpt(hidden, token)
	if strings.Contains(redacted, token) || !strings.Contains(redacted, "[redacted]") {
		t.Fatalf("the whitespace-hidden credential survived: %q", redacted)
	}

	// The bound itself still holds on a body far larger than the excerpt.
	bounded := redactForExcerpt(message, token)
	if len(bounded) > maxErrorBodyBytes+len(" [truncated]") {
		t.Fatalf("the excerpt is %d bytes, above the %d byte budget", len(bounded), maxErrorBodyBytes)
	}
}

// TestWhitespaceFloodRedactionIsBoundedAndFailsClosed is the regression for the
// unbounded scan: a failure body that never reaches the collapsed excerpt
// target used to be scanned in full (up to MaxResponseBytes, 64 MiB, through
// every decoding view). The scan must stop at maxExcerptScanBytes and fail
// closed, because a credential could sit past the cap.
func TestWhitespaceFloodRedactionIsBoundedAndFailsClosed(t *testing.T) {
	const token = testToken
	message := "x" + strings.Repeat(" ", (64<<20)-1-len(token)) + token
	if len(message) != 64<<20 {
		t.Fatalf("the fixture is %d bytes, want %d", len(message), 64<<20)
	}
	target := maxErrorBodyBytes + credentialFormByteMax*len(token)

	region, complete, examined := excerptRegion(message, target)
	if complete {
		t.Fatal("a collapse that never reaches the target inside the cap must fail closed")
	}
	if region != "" {
		t.Fatalf("a failed scan must return no region, got %d bytes", len(region))
	}
	assertExcerptScanBounded(t, examined)
	if examined < maxExcerptScanBytes {
		t.Fatalf("the flood walk examined %d raw bytes; it must walk to the %d byte cap before failing closed",
			examined, maxExcerptScanBytes)
	}

	started := time.Now()
	got := redactForExcerpt(message, token)
	elapsed := time.Since(started)
	if got != redactionMarker {
		t.Fatalf("the whitespace flood must fail closed to %q, got %q", redactionMarker, got)
	}
	// The examined-bytes assertion above is the work bound; this time budget is
	// only a secondary sanity bound on the same path.
	if elapsed > 2*time.Second {
		t.Fatalf("the 64 MiB flood took %v; the scan must stop at maxExcerptScanBytes (%d bytes)",
			elapsed, maxExcerptScanBytes)
	}
	assertNoCredentialFragment(t, token, "whitespace flood", got)
}

// TestTokenBeyondTheScanCapNeverSurvives pins the fail-closed guarantee for a
// credential placed past the raw scan cap: the excerpt is the redaction marker
// and no fragment of the token is published.
func TestTokenBeyondTheScanCapNeverSurvives(t *testing.T) {
	const token = testToken
	message := strings.Repeat(" ", maxExcerptScanBytes+64) + token + strings.Repeat(" ", 64)
	got := redactForExcerpt(message, token)
	if got != redactionMarker {
		t.Fatalf("a token beyond the cap must fail closed to %q, got %q", redactionMarker, got)
	}
	assertRedacted(t, token, "token beyond the cap", got)
}

// TestPlainWhitespaceFloodFailsClosed pins the documented criterion: the cap
// applies regardless of credential presence, so a whitespace-heavy body with no
// credential in it still fails closed rather than publish an excerpt that
// cannot be proven clean.
func TestPlainWhitespaceFloodFailsClosed(t *testing.T) {
	message := strings.Repeat(" ", maxExcerptScanBytes+1)
	if got := redactForExcerpt(message, testToken); got != redactionMarker {
		t.Fatalf("a whitespace body whose target is not reached must fail closed to %q, got %q",
			redactionMarker, got)
	}
}

// TestTokenInsideTheScanCapIsCaughtAtTheBoundaryOffsets pins that the cap only
// fails the bodies whose collapse cannot reach the target inside it: a token
// that ends inside the cap is redacted and the surrounding diagnostics survive,
// while a token that crosses the cap turns the whole excerpt into the marker.
func TestTokenInsideTheScanCapIsCaughtAtTheBoundaryOffsets(t *testing.T) {
	const token = testToken
	prefix := "start "
	spaces := func(tail int) int { return maxExcerptScanBytes - len(prefix) - len(token) - tail }
	for _, tail := range []int{minRedactionRun - 1, 1, 0} {
		message := prefix + strings.Repeat(" ", spaces(tail)) + token + strings.Repeat("x", tail)
		if len(message) > maxExcerptScanBytes {
			t.Fatalf("fixture ends at %d bytes, past the cap", len(message))
		}
		got := redactForExcerpt(message, token)
		if strings.Contains(got, token) {
			t.Fatalf("a token inside the cap survived: %q", got)
		}
		if !strings.Contains(got, "start") || !strings.Contains(got, redactionMarker) {
			t.Fatalf("a token inside the cap must be redacted without losing the diagnostics: %q", got)
		}
		assertNoCredentialFragment(t, token, "token inside the cap", got)
	}
	// One byte past the cap: the token crosses the boundary the scan refuses to
	// read past, so the excerpt fails closed.
	crossing := prefix + strings.Repeat(" ", spaces(0)+1) + token
	if len(crossing) <= maxExcerptScanBytes {
		t.Fatal("the crossing fixture must exceed the cap")
	}
	if got := redactForExcerpt(crossing, token); got != redactionMarker {
		t.Fatalf("a token crossing the cap must fail closed to %q, got %q", redactionMarker, got)
	}
}

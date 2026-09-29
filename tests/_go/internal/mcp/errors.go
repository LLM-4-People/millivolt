package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
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

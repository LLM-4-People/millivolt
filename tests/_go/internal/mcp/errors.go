package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
}

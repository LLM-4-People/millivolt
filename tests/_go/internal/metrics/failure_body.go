package metrics

// Regression: a failed response whose body carries no OpenAI "error" envelope
// must still yield its human-readable reason. Observed symptom: a 503 whose
// body was `{"detail":"..."}`, `{"message":"..."}` or Cloudflare's
// `{"success":false,"errors":[{"message":"..."}]}` was recorded as
// provider_error with an EMPTY message, so the dashboard showed a bare status.
//
// The failure-only decoder is the narrow fix. ParseErrorEnvelope - which every
// success path uses as a "is this a failure" discriminator - must keep
// answering empty for these bodies, or a healthy 200 carrying a top-level
// "message" would be reclassified as a provider error. Both halves are pinned
// here; the end-to-end record path is pinned in tests/_go/internal/proxy.

import (
	"strings"
	"testing"
)

func TestParseFailureBodyShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTyp string
		wantCde string
		wantMsg string
	}{
		// The OpenAI envelope always wins, in every spelling.
		{
			name:    "openai envelope",
			body:    `{"error":{"type":"invalid_request_error","code":"bad","message":"boom"}}`,
			wantTyp: "invalid_request_error",
			wantCde: "bad",
			wantMsg: "boom",
		},
		{
			name:    "anthropic envelope",
			body:    `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantTyp: "overloaded_error",
			wantMsg: "Overloaded",
		},
		{
			name:    "plain string error",
			body:    `{"error":"service unavailable"}`,
			wantTyp: "provider_error",
			wantMsg: "service unavailable",
		},
		// Non-envelope failure shapes recover the provider's own reason.
		{
			name:    "fastapi detail string",
			body:    `{"detail":"model is not serving"}`,
			wantTyp: "provider_error",
			wantMsg: "model is not serving",
		},
		{
			name:    "bare top-level message",
			body:    `{"message":"model is not serving"}`,
			wantTyp: "provider_error",
			wantMsg: "model is not serving",
		},
		{
			name:    "cloudflare errors array",
			body:    `{"success":false,"errors":[{"code":1000,"message":"DNS points to prohibited IP"}]}`,
			wantTyp: "provider_error",
			wantCde: "1000",
			wantMsg: "DNS points to prohibited IP",
		},
		{
			name:    "cloudflare errors single object",
			body:    `{"success":false,"errors":{"code":1001,"message":"origin unreachable"}}`,
			wantTyp: "provider_error",
			wantCde: "1001",
			wantMsg: "origin unreachable",
		},
		{
			name:    "fastapi validation detail array",
			body:    `{"detail":[{"loc":["body"],"msg":"field required","type":"value_error"}]}`,
			wantTyp: "provider_error",
			wantMsg: "field required",
		},
		{
			name:    "short msg member",
			body:    `{"msg":"bad request"}`,
			wantTyp: "provider_error",
			wantMsg: "bad request",
		},
		{
			name:    "reason member",
			body:    `{"status":"error","reason":"capacity exhausted"}`,
			wantTyp: "provider_error",
			wantMsg: "capacity exhausted",
		},
		{
			name:    "top-level string document",
			body:    `"upstream is down"`,
			wantTyp: "provider_error",
			wantMsg: "upstream is down",
		},
		{
			name:    "nested detail object",
			body:    `{"detail":{"message":"inner reason"}}`,
			wantTyp: "provider_error",
			wantMsg: "inner reason",
		},
		// Decoys and shapes with no reason stay classifiable but silent.
		{
			name: "decoy members are not stringified",
			body: `{"detail":{"nested":true},"message":42}`,
		},
		{
			name:    "boolean reason ignored",
			body:    `{"message":true}`,
			wantTyp: "",
		},
		{
			name: "null decoy error stays empty",
			body: `{"error":null}`,
		},
		{
			name: "empty body stays unclassified",
			body: "",
		},
		{
			name: "json without a reason member",
			body: `{"success":false}`,
		},
		{
			name: "deep nesting stays bounded",
			body: `{"detail":{"detail":{"detail":{"detail":{"detail":{"detail":"too deep"}}}}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, code, msg := ParseFailureBody([]byte(tc.body))
			if typ != tc.wantTyp || code != tc.wantCde || msg != tc.wantMsg {
				t.Fatalf("ParseFailureBody(%q) = %q/%q/%q, want %q/%q/%q",
					tc.body, typ, code, msg, tc.wantTyp, tc.wantCde, tc.wantMsg)
			}
		})
	}
}

// TestParseFailureBodyBounds proves the recovered reason cannot smuggle
// unbounded provider bytes into a retained record field: it is truncated to
// the same bound the envelope path uses.
func TestParseFailureBodyBounds(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	for _, body := range []string{
		`{"detail":"` + huge + `"}`,
		`{"message":"` + huge + `"}`,
		`{"errors":[{"message":"` + huge + `"}]}`,
		`"` + huge + `"`,
	} {
		_, _, msg := ParseFailureBody([]byte(body))
		if msg == "" {
			t.Fatalf("body %.20q produced no message", body)
		}
		if len(msg) > RetainedErrorMsgMax {
			t.Fatalf("message length %d exceeds the record bound %d", len(msg), RetainedErrorMsgMax)
		}
	}
}

// TestSuccessShapesStayUnclassified is the negative half of the contract: the
// success-capable decoder must never treat a reason-bearing body as a failure,
// because every HTTP 200 / in-band-error detector relies on that answer.
func TestSuccessShapesStayUnclassified(t *testing.T) {
	for _, body := range []string{
		`{"detail":"model is not serving"}`,
		`{"message":"model is not serving"}`,
		`{"errors":[{"message":"boom"}]}`,
		`{"error":null}`,
	} {
		typ, code, msg := ParseErrorEnvelope([]byte(body))
		if typ != "" || code != "" || msg != "" {
			t.Fatalf("ParseErrorEnvelope(%q) = %q/%q/%q: a success-path body must stay unclassified",
				body, typ, code, msg)
		}
	}
}

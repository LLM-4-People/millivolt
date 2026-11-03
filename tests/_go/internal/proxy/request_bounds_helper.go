package proxy

// Test helpers for the request preflight bounds. Production reads the live
// config snapshot through Server.cfg(); these helpers reuse the shared
// testConfig() (config.Default() with millisecond retry ladders) so every
// test path carries the documented request defaults. Cap tests mutate a copy
// instead of passing a test-only bound type.

import (
	"net/http"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// testRequestConfig is one read-only Default snapshot shared by the parsing
// helpers. config.Default() allocates a full Config, and allocation-sensitive
// tests measure the preflight itself, so the helpers must not construct a new
// snapshot per call.
var testRequestConfig = config.Default()

func parseTestRequest(body []byte, rec *metrics.Record, preview bool) error {
	return parseLLMRequest(body, rec, preview, testRequestConfig)
}

func readTestRequest(r *http.Request, maxBytes int64, preview bool) (body []byte, rec *metrics.Record, err error) {
	return readRequest(r, maxBytes, preview, testRequestConfig)
}

func readTestHint(r *http.Request, maxBytes int64, preview bool, hintMax int64) (body []byte, rec *metrics.Record, err error) {
	return readRequestHint(r, maxBytes, preview, hintMax, testRequestConfig)
}

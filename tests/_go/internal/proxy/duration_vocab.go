package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The dashboard's duration dropdowns offer fixed token sets (chrome.js
// PAUSE_DURS/DEBUG_DURS, LIMIT_WINDOWS, FILTER_AGES). These tables pin that
// every JS-offered token is accepted by the Go owner of its surface, so a
// menu can never offer a duration the server would reject:
//   - pause/debug hold durations go through the pauseDurations allowlist
//     (shared by HandlePause and HandleDebug);
//   - throttle request/token windows go through parseLimitWindow;
//   - the Clear/Logs age filter's tokens must be canonical Go duration
//     spellings (config.FormatDuration round-trips them) because the client
//     parses them with the Go duration grammar before sending before_ms.
var (
	pauseDebugDurations = []string{"", "15m", "1h", "6h", "12h", "24h"}
	limitWindows        = []string{"1s", "10s", "30s", "1m", "5m", "15m", "1h", "6h", "24h"}
	filterAges          = []string{"1h", "24h", "168h"}
)

func TestPauseDebugDurationsAcceptJSMenu(t *testing.T) {
	for _, tok := range pauseDebugDurations {
		p := New(config.Default(), metrics.Noop{})
		body := `{"paused":true,"clients":["client-a"],"duration":` + quoteJSON(tok) + `}`
		rec := httptest.NewRecorder()
		p.HandlePause(rec, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Errorf("pause duration %q = %d %s, want 200", tok, rec.Code, rec.Body.Bytes())
			continue
		}
		p.removePause(pauseJSON(t, rec)["holds"].([]any)[0].(map[string]any)["id"].(string), time.Time{})

		body = `{"enabled":true,"clients":["client-a"],"duration":` + quoteJSON(tok) + `}`
		rec = httptest.NewRecorder()
		p.HandleDebug(rec, httptest.NewRequest(http.MethodPost, "/admin/debug", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Errorf("debug duration %q = %d %s, want 200", tok, rec.Code, rec.Body.Bytes())
		}
	}
	// A token outside the vocabulary is denied (deny by default): the shared
	// allowlist, not ParseDuration, decides.
	p := New(config.Default(), metrics.Noop{})
	rec := httptest.NewRecorder()
	p.HandlePause(rec, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"],"duration":"30s"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("pause duration 30s = %d, want 400 (not on the menu, not accepted)", rec.Code)
	}
}

// quoteJSON renders a duration token (or the empty string) as a JSON string.
func quoteJSON(tok string) string {
	if tok == "" {
		return `""`
	}
	return `"` + tok + `"`
}

func TestLimitWindowsAcceptJSMenu(t *testing.T) {
	for _, tok := range limitWindows {
		d, err := parseLimitWindow(tok)
		if err != nil {
			t.Errorf("parseLimitWindow(%q): %v, want the Limits menu token accepted", tok, err)
			continue
		}
		if d < minLimitWindow || d > maxLimitWindow {
			t.Errorf("parseLimitWindow(%q) = %v, inside the accepted range", tok, d)
		}
	}
	// Out-of-range windows stay denied: the menu's largest token is 24h, the
	// server's upper bound is the same 24h, and anything beyond is rejected.
	for _, tok := range []string{"25h", "0s", "500ms"} {
		if _, err := parseLimitWindow(tok); err == nil {
			t.Errorf("parseLimitWindow(%q) accepted, want rejected (outside the window range)", tok)
		}
	}
}

func TestFilterAgesAreCanonicalGoDurations(t *testing.T) {
	for _, tok := range filterAges {
		d, err := time.ParseDuration(tok)
		if err != nil {
			t.Errorf("time.ParseDuration(%q): %v, want a Go duration literal", tok, err)
			continue
		}
		if got := config.FormatDuration(d); got != tok {
			t.Errorf("FormatDuration(%q) = %q, want the same canonical spelling", tok, got)
		}
	}
}

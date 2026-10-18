package metrics

import (
	"strings"
	"testing"
)

// TestPrometheusLabelValuesEscapeOnlyRequired pins the exposition text-format
// escaping contract for every label value: only backslash, double quote, and
// line feed get a Prometheus escape; valid UTF-8 (including format characters,
// line separators, and DEL) rides raw; invalid UTF-8 bytes become U+FFFD so a
// client-minted provider label can never poison the whole scrape. Ordinary
// labels stay byte-identical.
func TestPrometheusLabelValuesEscapeOnlyRequired(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		want     string
	}{
		{"ordinary", "alpha", `provider="alpha"`},
		{"zero width space", "a\u200b.com", "provider=\"a\u200b.com\""},
		{"line separator", "a\u2028.com", "provider=\"a\u2028.com\""},
		{"delete character", "a\x7f.com", "provider=\"a\x7f.com\""},
		{"backslash", "a\\b", `provider="a\\b"`},
		{"double quote", `a"b`, `provider="a\"b"`},
		{"line feed", "a\nb", `provider="a\nb"`},
		{"invalid utf8", "a\xffb", "provider=\"a\uFFFDb\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := renderPrometheusExposition([]*Record{{Provider: tc.provider, StatusCode: 200}})
			if err != nil {
				t.Fatal(err)
			}
			wantLine := "llm_proxy_requests_by_provider{" + tc.want + "} 1"
			for line := range strings.Lines(body) {
				line = strings.TrimSuffix(line, "\n")
				if strings.HasPrefix(line, "llm_proxy_requests_by_provider{") {
					if line != wantLine {
						t.Fatalf("label line = %q, want %q", line, wantLine)
					}
					return
				}
			}
			t.Fatalf("provider label line missing from exposition:\n%s", body)
		})
	}
}

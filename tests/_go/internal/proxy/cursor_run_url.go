package proxy

import (
	"testing"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
)

// TestCursorRunURLJoinsLikePassthrough pins the Cursor send URL to the shared
// target owner. The Connect transport builds its own request body, but path and
// query normalization must match every relayed request: a trailing-slash base,
// an override without a leading slash, a duplicated base segment and the
// X-Proxy-Query override all resolve here.
func TestCursorRunURLJoinsLikePassthrough(t *testing.T) {
	const base = "http://upstream.example"
	cases := []struct {
		name, base, path, query, want string
	}{
		{"default", base, "", "", base + providerformat.CursorConnectPath()},
		{"trailing slash base", base + "/", "", "", base + providerformat.CursorConnectPath()},
		{"override without slash", base, "custom/run", "", base + "/custom/run"},
		{"duplicate base segment", base + "/api", "/api/run", "", base + "/api/run"},
		{"query override", base, "/run", "connect=v1", base + "/run?connect=v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cursorRunURL(&target{baseURL: tc.base, path: tc.path, query: tc.query})
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.want {
				t.Fatalf("cursorRunURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCursorRunURLRejectsAnUnescapablePath pins the validation the old string
// concatenation skipped: a percent escape the URL package cannot decode is an
// error instead of a malformed send.
func TestCursorRunURLRejectsAnUnescapablePath(t *testing.T) {
	if _, err := cursorRunURL(&target{baseURL: "http://upstream.example", path: "/run%zz"}); err == nil {
		t.Fatal("cursorRunURL accepted a malformed percent escape")
	}
}

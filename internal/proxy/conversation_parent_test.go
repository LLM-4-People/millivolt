package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestRequestConversationDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sessions []string
		parents  []string
		wantErr  bool
	}{
		{name: "ordinary"},
		{name: "explicit", sessions: []string{"task"}},
		{name: "child", sessions: []string{" child "}, parents: []string{" parent "}},
		{name: "parent without child", parents: []string{"parent"}, wantErr: true},
		{name: "empty child", sessions: []string{" "}, parents: []string{"parent"}, wantErr: true},
		{name: "empty parent", sessions: []string{"child"}, parents: []string{" "}, wantErr: true},
		{name: "duplicate child", sessions: []string{"child", "child"}, parents: []string{"parent"}, wantErr: true},
		{name: "duplicate parent", sessions: []string{"child"}, parents: []string{"parent", "parent"}, wantErr: true},
		{name: "self", sessions: []string{" child "}, parents: []string{"child"}, wantErr: true},
		{name: "long declaration", sessions: []string{"child"}, parents: []string{strings.Repeat("p", maxDeclaredSessionBytes+1)}, wantErr: true},
		{name: "long child", sessions: []string{strings.Repeat("c", maxDeclaredSessionBytes+1)}, parents: []string{"parent"}, wantErr: true},
		{name: "control", sessions: []string{"child"}, parents: []string{"par\x00ent"}, wantErr: true},
		{name: "invalid utf8", sessions: []string{"child"}, parents: []string{"\xff"}, wantErr: true},
		{name: "legacy session unchanged", sessions: []string{strings.Repeat("c", maxDeclaredSessionBytes+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			for _, v := range tc.sessions {
				h.Add(hdrSession, v)
			}
			for _, v := range tc.parents {
				h.Add(hdrParentSession, v)
			}
			child, parent, err := requestConversation(h)
			if (err != nil) != tc.wantErr {
				t.Fatalf("child=%q parent=%q err=%v", child, parent, err)
			}
			if err == nil && len(tc.parents) == 1 && (child != strings.TrimSpace(tc.sessions[0]) || parent != strings.TrimSpace(tc.parents[0])) {
				t.Fatalf("declaration changed: child=%q parent=%q", child, parent)
			}
		})
	}
}

func TestParentConversationCaptureAndTransparency(t *testing.T) {
	const body = `{"model":"fixture-model","messages":[{"role":"user","content":"fixture"}],"previous_response_id":"prior-response","metadata":{"parent_id":"untrusted-body-label"}}`
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if string(data) != body {
			t.Errorf("request changed: %q", data)
		}
		if r.Header.Get(hdrSession) != "" || r.Header.Get(hdrParentSession) != "" {
			t.Error("conversation control header leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"fixture answer"}}]}`)
	}))
	defer upstream.Close()
	buf := metrics.NewBuffer(8)
	s := New(config.Default(), buf)
	for _, declared := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set(hdrBaseURL, upstream.URL)
		r.Header.Set(hdrSession, "child")
		if declared {
			r.Header.Set(hdrParentSession, "parent")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d: %s", w.Code, w.Body.String())
		}
	}
	rows := buf.Snapshot()
	if len(rows) != 2 {
		t.Fatalf("records=%d", len(rows))
	}
	parents := 0
	for _, r := range rows {
		if r.ConversationID != "s:child" {
			t.Fatalf("legacy ID changed: %q", r.ConversationID)
		}
		if r.ParentConversationID == "s:parent" {
			parents++
		} else if r.ParentConversationID != "" {
			t.Fatalf("invented parent: %q", r.ParentConversationID)
		}
	}
	if parents != 1 {
		t.Fatalf("explicit declarations=%d, want one", parents)
	}
	bad := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	bad.Header.Set(hdrBaseURL, upstream.URL)
	bad.Header.Set(hdrParentSession, "parent")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, bad)
	if w.Code != http.StatusBadRequest || calls != 2 || len(buf.Snapshot()) != 2 {
		t.Fatalf("invalid declaration dispatched: status=%d calls=%d", w.Code, calls)
	}
}

func TestParentConversationHeaderCannotBeInjected(t *testing.T) {
	if !isProxyControlHeader("x-PrOxY-PaReNt-SeSsIoN") {
		t.Fatal("parent header is not stripped at shared owner")
	}
	s := New(config.Default(), metrics.Noop{})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(hdrParentSession, "forwarded")
	target := &target{baseURL: "https://fixture.example", authHeader: "Authorization", extraHeader: http.Header{hdrParentSession: []string{"injected"}}}
	req, err := s.buildUpstreamRequest(t.Context(), r, target, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get(hdrParentSession) != "" {
		t.Fatal("injected parent header leaked upstream")
	}
}

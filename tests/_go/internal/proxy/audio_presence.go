package proxy

// F1 regression: a valid audio-only turn is real answer output on every
// surface. The Chat stream/non-stream transcript is answer content, audio data
// is answer-shaped output, and a Responses terminal message whose content part
// is output_audio is not replaced by response.failed. A truncated audio
// stream is never re-sent after the transcript was delivered.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestChatAudioTranscriptIsAnswerContent(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, chunk := range []string{
				"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
				"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"audio\":{\"id\":\"audio_1\",\"expires_at\":1,\"data\":\"QUJD\",\"transcript\":\"spoken answer\"}}}]}\n\n",
				"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
				"data: [DONE]\n\n",
			} {
				w.Write([]byte(chunk))
				w.(http.Flusher).Flush()
			}
		}))
		defer upstream.Close()

		cfg := config.Default()
		cfg.CaptureBodyPreview = true
		buf := metrics.NewBuffer(8)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()

		status, got := streamRequest(t, srv, upstream)
		if status != http.StatusOK {
			t.Fatalf("status = %d (body %s)", status, got)
		}
		if strings.Contains(got, metrics.CodeEmptyCompletion) {
			t.Fatalf("audio transcript was classified empty_completion: %s", got)
		}
		if !strings.Contains(got, `"transcript":"spoken answer"`) {
			t.Fatalf("audio transcript was not relayed: %s", got)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("upstream calls = %d, want 1", n)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.IsError() || rec.Retries != 0 || !rec.HadAnswerContent {
			t.Fatalf("record = %+v, want a healthy first-attempt answer", rec)
		}
		if rec.ResponsePreview != "spoken answer" {
			t.Fatalf("preview = %q, want the audio transcript", rec.ResponsePreview)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		var calls atomic.Int32
		body := `{"id":"chat-audio","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":null,"audio":{"id":"audio_1","expires_at":1,"data":"QUJD","transcript":"spoken answer"}}}]}`
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, body)
		}))
		defer upstream.Close()

		cfg := config.Default()
		cfg.CaptureBodyPreview = true
		buf := metrics.NewBuffer(8)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-k")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, got)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("upstream calls = %d, want 1 (audio transcript is answer content)", n)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.IsError() || rec.Retries != 0 || !rec.HadAnswerContent {
			t.Fatalf("record = %+v, want a healthy first-attempt answer", rec)
		}
		if rec.ResponsePreview != "spoken answer" {
			t.Fatalf("preview = %q, want the audio transcript", rec.ResponsePreview)
		}
	})

	t.Run("audio data only is not content", func(t *testing.T) {
		var calls atomic.Int32
		body := `{"id":"chat-audio","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":null,"audio":{"id":"audio_1","expires_at":1,"data":"QUJD"}}}]}`
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, body)
		}))
		defer upstream.Close()

		cfg := config.Default()
		cfg.CaptureBodyPreview = true
		buf := metrics.NewBuffer(8)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-k")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, got)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("upstream calls = %d, want 1 (audio data is answer-shaped output)", n)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.IsError() || rec.Retries != 0 || rec.HadAnswerContent {
			t.Fatalf("record = %+v, want a healthy non-content audio output", rec)
		}
	})
}

func TestResponsesAudioTranscriptIsAnswerContent(t *testing.T) {
	t.Run("terminal message content", func(t *testing.T) {
		upstream := streamUpstream(t,
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_audio\",\"transcript\":\"spoken answer\",\"data\":\"QUJD\"}]}]}}\n\n",
		)
		defer upstream.Close()

		cfg := config.Default()
		cfg.CaptureBodyPreview = true
		buf := metrics.NewBuffer(8)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()

		status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
			`{"model":"m","stream":true,"input":"hi"}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d (body %s)", status, got)
		}
		if !strings.Contains(got, "event: response.completed") {
			t.Fatalf("terminal audio completion was not relayed: %s", got)
		}
		if strings.Contains(got, "response.failed") || strings.Contains(got, "empty_completion") {
			t.Fatalf("terminal audio content was replaced with a failure: %s", got)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.IsError() || !rec.HadAnswerContent {
			t.Fatalf("record = %+v, want answer content and no error", rec)
		}
		if rec.ResponsePreview != "spoken answer" {
			t.Fatalf("preview = %q, want the terminal transcript", rec.ResponsePreview)
		}
	})

	t.Run("stream transcript delta", func(t *testing.T) {
		upstream := streamUpstream(t,
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n",
			"event: response.audio.transcript.delta\ndata: {\"type\":\"response.audio.transcript.delta\",\"delta\":\"spoken answer\",\"output_index\":0,\"content_index\":0}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\"}}\n\n",
		)
		defer upstream.Close()

		cfg := config.Default()
		cfg.CaptureBodyPreview = true
		buf := metrics.NewBuffer(8)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()

		status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
			`{"model":"m","stream":true,"input":"hi"}`)
		if status != http.StatusOK {
			t.Fatalf("status = %d (body %s)", status, got)
		}
		if strings.Contains(got, "response.failed") || strings.Contains(got, "empty_completion") {
			t.Fatalf("transcript delta was replaced with a failure: %s", got)
		}
		rec := waitForRecord(t, buf, 1)[0]
		if rec.IsError() || !rec.HadAnswerContent {
			t.Fatalf("record = %+v, want answer content and no error", rec)
		}
		if rec.ResponsePreview != "spoken answer" {
			t.Fatalf("preview = %q, want the transcript delta", rec.ResponsePreview)
		}
	})
}

// TestTruncatedAudioStreamIsNotRescued pins the rescue control: a transcript
// or audio data already reached the client, so the truncation must stay
// client-side instead of re-sending and duplicating audio.
func TestTruncatedAudioStreamIsNotRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"audio\":{\"transcript\":\"spoken answer\"}}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("truncated audio error missing: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (delivered audio is never re-sent)", n)
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.ErrorCode != metrics.CodeTruncated || !rec.HadAnswerContent || rec.Retries != 0 {
		t.Fatalf("record = %+v, want a truncated delivered audio turn", rec)
	}
}

package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type stormCursorBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *stormCursorBody) Close() error {
	b.closed.Store(true)
	return nil
}

func stormCursorReply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: &stormCursorBody{Reader: strings.NewReader(body)}}
}

func stormCursorAnswer() string {
	return string(cframe(cmsg(1, cmsg(1, cstr(1, "recovered"))))) + string(cframe(cmsg(1, cmsg(8, cvint(1, 3))))) + string(cframe(cmsg(1, cmsg(14, nil)))) + string(cend("{}"))
}

func TestStormCursorDoesNotReplayStartedRunsOrQuota(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		status  int
		body    string
	}{
		{"disabled", false, 503, `{"error":{"type":"unavailable"}}`},
		{"quota", true, 429, `{"error":{"type":"insufficient_quota"}}`},
		{"started_run", true, 200, string(cend(`{"error":{"code":"unavailable","message":"fixture"}}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := stormTestConfig()
			cfg.StormEnabled = tc.enabled
			buf := metrics.NewBuffer(10)
			s := New(cfg, buf)
			var calls atomic.Int32
			s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				readFrame(t, r.Body)
				return stormCursorReply(tc.status, tc.body), nil
			})}
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"neutral-model","messages":[{"role":"user","content":"fixture"}]}`))
			r.Header.Set(hdrBaseURL, "http://neutral.invalid")
			r.Header.Set(hdrFormat, "cursor")
			s.ServeHTTP(httptest.NewRecorder(), r)
			rec := waitForRecord(t, buf, 1)[0]
			if calls.Load() != 1 || rec.Retries != 0 || len(rec.Attempts) != 0 {
				t.Fatalf("native work replayed: calls=%d record=%+v", calls.Load(), rec)
			}
			if tc.name != "started_run" && len(s.scheduler.StormSnapshot()) != 0 {
				t.Fatal("disabled/quota rejection opened a storm")
			}
		})
	}
}

func TestStormCursorFreshOpenersShareRetryBudgetAndCleanup(t *testing.T) {
	for _, reask := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "reask"}[reask], func(t *testing.T) {
			cfg := stormTestConfig()
			cfg.StormMaxRetries = 2
			s := New(cfg, metrics.NewBuffer(10))
			rec := &metrics.Record{Model: "neutral-model", Provider: "neutral.invalid"}
			state := &stormRequestState{rec: rec}
			ctx := context.WithValue(context.Background(), stormContextKey{}, state)
			var requests []*http.Request
			var abandoned *stormCursorBody
			s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r)
				readFrame(t, r.Body)
				switch len(requests) {
				case 1:
					return nil, io.ErrUnexpectedEOF
				case 2:
					resp := stormCursorReply(503, `{"error":{"type":"unavailable"}}`)
					abandoned = resp.Body.(*stormCursorBody)
					return resp, nil
				default:
					return stormCursorReply(200, stormCursorAnswer()), nil
				}
			})}
			target := &target{baseURL: "http://neutral.invalid", timeout: time.Second}
			hooks := scheduler.WaiterHooks{Provider: rec.Provider}
			if reask {
				run, err := s.openCursorRun(ctx, nil, target, "", []byte(`{"model":"neutral-model","messages":[{"role":"user","content":"fixture"}]}`), true, "native-retry", hooks)
				if err != nil {
					t.Fatal(err)
				}
				run.Close()
			} else {
				resp, pw, cancel, err := s.openCursorHTTP(ctx, target, "", []byte("native-frame"), "native-retry", hooks, false)
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				pw.Close()
				resp.Body.Close()
			}
			s.finishStormResponse(ctx, false)
			if len(requests) != 3 || state.extra != 2 || rec.Retries != 2 || len(rec.Attempts) != 2 || rec.FinalAttemptAt.IsZero() {
				t.Fatalf("native shared retry budget: calls=%d state=%+v rec=%+v", len(requests), state, rec)
			}
			if rec.Attempts[0].ErrorType != "transport" || rec.Attempts[1].StatusCode != 503 || !abandoned.closed.Load() {
				t.Fatalf("abandoned native outcomes were not finalized: %+v", rec.Attempts)
			}
			for _, r := range requests {
				if r.Context().Err() == nil {
					t.Fatal("native request context leaked")
				}
				if _, err := r.Body.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("native pipe remains open: %v", err)
				}
			}
			if s.scheduler.RequestBackoff("native-retry") != 0 || len(s.scheduler.StormSnapshot()) != 0 {
				t.Fatal("successful native recovery retained pacing")
			}
		})
	}
}

func TestStormCursorWaitDoesNotSpendSendDeadline(t *testing.T) {
	cfg := stormTestConfig()
	cfg.StormInitialBackoff = 150 * time.Millisecond
	cfg.StormMaxBackoff = cfg.StormInitialBackoff
	s := New(cfg, metrics.NewBuffer(10))
	rec := &metrics.Record{Model: "neutral-model", Provider: "neutral.invalid"}
	ctx := context.WithValue(context.Background(), stormContextKey{}, &stormRequestState{rec: rec})
	permit, err := s.scheduler.WaitStorm(ctx, rec.Provider, rec.Model, nil)
	if err != nil {
		t.Fatal(err)
	}
	permit.Observe(true, "HTTP 503", 0)
	s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		readFrame(t, r.Body)
		return stormCursorReply(200, stormCursorAnswer()), nil
	})}
	start := time.Now()
	resp, pw, cancel, err := s.openCursorHTTP(ctx, &target{baseURL: "http://neutral.invalid", timeout: 50 * time.Millisecond}, "", []byte("native-frame"), "native-wait", scheduler.WaiterHooks{Provider: rec.Provider}, false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	pw.Close()
	resp.Body.Close()
	s.finishStormResponse(ctx, false)
	if time.Since(start) < 100*time.Millisecond || rec.QueueWaitMs < 100 {
		t.Fatalf("native send bypassed storm wait: elapsed=%s record=%+v", time.Since(start), rec)
	}
}

func TestStormCursorRecoversBeforeNativeRunStarts(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readFrame(t, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"type":"unavailable","message":"retry fixture"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "recovered")))))
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		w.(http.Flusher).Flush()
	}), &http2.Server{}))
	defer up.Close()
	buf := metrics.NewBuffer(10)
	s := New(stormTestConfig(), buf)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"neutral-model","messages":[{"role":"user","content":"fixture"}]}`))
	r.Header.Set(hdrBaseURL, up.URL)
	r.Header.Set(hdrFormat, "cursor")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "recovered") || calls.Load() != 2 {
		t.Fatalf("native retry status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].StatusCode != 503 || rec.FinalAttemptAt.IsZero() || rec.ErrorType != "" {
		t.Fatalf("native retry accounting: %+v", rec)
	}
	if storms := s.scheduler.StormSnapshot(); len(storms) != 0 {
		t.Fatalf("successful native response did not settle recovery: %+v", storms)
	}
}

func TestStormCursorExhaustionAndQueueWait(t *testing.T) {
	for _, queueWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry_budget", true: "queue_wait"}[queueWait], func(t *testing.T) {
			cfg := stormTestConfig()
			cfg.StormMaxRetries = 1
			wantStatus, wantCalls := 503, int32(2)
			if queueWait {
				cfg.StormInitialBackoff = time.Second
				cfg.StormMaxBackoff = time.Second
				cfg.StormMaxWait = 10 * time.Millisecond
				wantStatus, wantCalls = 429, 1
			}
			buf := metrics.NewBuffer(10)
			s := New(cfg, buf)
			var calls atomic.Int32
			s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				readFrame(t, r.Body)
				return stormCursorReply(503, `{"error":{"type":"unavailable"}}`), nil
			})}
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"neutral-model","messages":[{"role":"user","content":"fixture"}]}`))
			r.Header.Set(hdrBaseURL, "http://neutral.invalid")
			r.Header.Set(hdrFormat, "cursor")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != wantStatus || calls.Load() != wantCalls {
				t.Fatalf("native bounded retry: status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
			if queueWait && w.Header().Get("Retry-After") == "" {
				t.Fatal("native storm queue rejection omitted retry hint")
			}
			if rec := waitForRecord(t, buf, 1)[0]; rec.Retries != 1 || len(rec.Attempts) != 1 {
				t.Fatalf("native exhausted request accounting: %+v", rec)
			}
		})
	}
}

func TestStormCursorQualityFailureSettlesBeforeReask(t *testing.T) {
	cfg := stormTestConfig()
	cfg.QualityRetries = 1
	cfg.StormInitialBackoff = 150 * time.Millisecond
	cfg.StormMaxBackoff = cfg.StormInitialBackoff
	buf := metrics.NewBuffer(10)
	s := New(cfg, buf)
	var calls atomic.Int32
	s.cursorClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		readFrame(t, r.Body)
		if calls.Add(1) == 1 {
			return stormCursorReply(200, string(cframe(cmsg(1, cmsg(14, nil))))+string(cend("{}"))), nil
		}
		return stormCursorReply(200, stormCursorAnswer()), nil
	})}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"neutral-model","messages":[
		{"role":"user","content":"fixture"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"fixture-call","type":"function","function":{"name":"fixture","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"fixture-call","content":"fixture"}]}`))
	r.Header.Set(hdrBaseURL, "http://neutral.invalid")
	r.Header.Set(hdrFormat, "cursor")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	rec := waitForRecord(t, buf, 1)[0]
	if w.Code != 200 || calls.Load() != 2 || rec.QueueWaitMs < 100 || rec.Retries != 1 || rec.Attempts[0].ErrorType != "empty_turn" {
		t.Fatalf("quality re-ask bypassed settled response failure: status=%d calls=%d record=%+v", w.Code, calls.Load(), rec)
	}
	if len(s.scheduler.StormSnapshot()) != 0 {
		t.Fatal("quality re-ask success did not settle recovery")
	}
}

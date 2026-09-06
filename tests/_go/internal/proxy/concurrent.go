package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

func storedProxyForTest(t *testing.T) (*Server, *metrics.Buffer, *storage.Store) {
	t.Helper()
	cfg := config.Default()
	store, err := storage.Open(filepath.Join(t.TempDir(), "requests.db"), storage.Options{
		WriteChanCap: cfg.StorageWriteChanCap, BatchCap: cfg.StorageBatchCap,
		FlushInterval: cfg.StorageFlushInterval, QueryTimeout: cfg.StorageQueryTimeout,
		WriteTrackCap: 2 * cfg.HistorySize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	buf := metrics.NewBuffer(cfg.HistorySize)
	p := New(cfg, store.Recorder(buf))
	t.Cleanup(func() { p.httpClient().CloseIdleConnections() })
	return p, buf, store
}

// Exercise the real pooled transport and asynchronous recorder together. The
// upstream is local and synthetic; every body must survive streaming unchanged
// and every accepted request must leave exactly one durable finalized record.
func TestConcurrentStreamingAccounting(t *testing.T) {
	const workers, requests = 16, 256
	const response = "data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range strings.SplitAfter(response, "\n\n") {
			if frame == "" {
				continue
			}
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	p, buf, store := storedProxyForTest(t)
	server := httptest.NewServer(p)
	defer server.Close()
	client := server.Client()
	defer client.CloseIdleConnections()
	jobs := make(chan int, requests)
	for i := range requests {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/v1/chat/completions",
					strings.NewReader(`{"model":"fixture","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
				if err != nil {
					t.Error(err)
					continue
				}
				req.Header.Set("Authorization", "Bearer fixture-key")
				req.Header.Set("X-Proxy-Base-URL", upstream.URL)
				req.Header.Set("X-Request-ID", fmt.Sprintf("concurrent-%d", i))
				resp, err := client.Do(req)
				if err != nil {
					t.Error(err)
					continue
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK || string(body) != response {
					t.Errorf("request %d: status=%d bytes=%d error=%v", i, resp.StatusCode, len(body), err)
				}
			}
		})
	}
	wg.Wait()
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	if counters := buf.Counters(); counters.TotalReq != requests || counters.TotalErr != 0 || counters.InFlight != 0 {
		t.Fatalf("concurrent lifecycle accounting: %+v", counters)
	}
	if totals := store.Totals(); totals.Requests != requests || totals.Errors != 0 || totals.InputTok != requests*10 || totals.OutputTok != requests*4 {
		t.Fatalf("durable totals did not match completed streams: %+v", totals)
	}
	if dropped := store.Dropped(); dropped != 0 {
		t.Fatalf("local bounded workload dropped %d records", dropped)
	}
	if pending := buf.PendingRecords(); len(pending) != 0 {
		t.Fatalf("completed workload retained %d pending records", len(pending))
	}
}

func TestDuplicateClientRequestIDsRemainDistinct(t *testing.T) {
	const clientID = "client-reused-request-id"
	const response = `{"choices":[{"message":{"content":"done"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
	arrived, release := make(chan string, 2), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- r.Header.Get("X-Request-Id")
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer upstream.Close()
	p, buf, store := storedProxyForTest(t)
	server := httptest.NewServer(p)
	defer server.Close()
	defer unblock() // also release blocked upstream requests on a test failure
	client := server.Client()
	defer client.CloseIdleConnections()
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"fixture","messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Authorization", "Bearer fixture-key")
			req.Header.Set("X-Proxy-Base-URL", upstream.URL)
			req.Header.Set("X-Request-Id", clientID)
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || string(body) != response {
				t.Errorf("proxy response status=%d bytes=%q err=%v", resp.StatusCode, body, err)
			}
		})
	}
	for range 2 {
		select {
		case got := <-arrived:
			if got != clientID {
				t.Errorf("upstream client header changed: %q", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not overlap upstream")
		}
	}
	if got := buf.Counters().InFlight; got != 2 {
		t.Errorf("overlapping in-flight=%d, want 2", got)
	}
	pending := buf.PendingRecords()
	if len(pending) != 2 {
		t.Errorf("overlapping requests collapsed into %d pending rows", len(pending))
	} else if pending[0].ID == pending[1].ID || pending[0].ID == clientID || pending[1].ID == clientID {
		t.Error("audit identity is controlled by the client header")
	}
	unblock()
	wg.Wait()
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := buf.Counters(); got.InFlight != 0 || got.TotalReq != 2 {
		t.Errorf("completion counters=%+v", got)
	}
	if pending := buf.PendingRecords(); len(pending) != 0 {
		t.Errorf("completed requests retained %d pending rows", len(pending))
	}
	rows := buf.Snapshot()
	if len(rows) != 2 || rows[0].ID == rows[1].ID {
		t.Errorf("ring requests not distinct: %+v", rows)
	}
	durable, err := store.LoadRecent(t.Context(), 10)
	if err != nil || len(durable) != 2 {
		t.Errorf("durable requests=%d err=%v, want 2", len(durable), err)
	}
	if totals := store.Totals(); totals.Requests != 2 || totals.InputTok != 4 || totals.OutputTok != 2 {
		t.Errorf("durable totals=%+v", totals)
	}
	if n, err := store.CountWhere(t.Context(), storage.PurgeFilter{}); err != nil || n != store.Totals().Requests {
		t.Errorf("durable count=%d totals=%d err=%v", n, store.Totals().Requests, err)
	}
}

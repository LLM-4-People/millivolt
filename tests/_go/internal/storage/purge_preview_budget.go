package storage

// NV-2: the preview barrier was detached from the caller deadline but the
// count still ran on the caller context, so a caller deadline shorter than the
// query budget made the preview fail instead of answering from the committed
// snapshot the operations doc promises.

import (
	"context"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestPurgePreviewAnswersBehindAnExpiredCaller(t *testing.T) {
	variants := []struct {
		name string
		call func(t *testing.T) context.Context
	}{
		{"deadline", func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			t.Cleanup(cancel)
			return ctx
		}},
		{"cancel", func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			time.AfterFunc(50*time.Millisecond, cancel)
			return ctx
		}},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			opts := testOpts
			opts.QueryTimeout = 2 * time.Second
			s := openPurgeTestStore(t, opts)
			s.Record(&metrics.Record{ID: "committed", Start: time.Now(), StatusCode: 200})
			if err := s.Flush(); err != nil {
				t.Fatal(err)
			}
			// The held connection wedges the writer, so the barrier cannot
			// commit the queued record inside the caller's 50ms.
			conn, err := s.db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			s.Record(&metrics.Record{ID: "queued", Start: time.Now(), StatusCode: 200})

			type answer struct {
				n   int64
				err error
			}
			done := make(chan answer, 1)
			go func() {
				n, err := s.CountWhere(variant.call(t), PurgeFilter{})
				done <- answer{n, err}
			}()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("preview behind an expired caller: %v", got.err)
				}
				if got.n != 1 {
					t.Fatalf("preview count = %d, want the one committed row", got.n)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("preview never answered")
			}
		})
	}
}

package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestCompletedReaderRetainsFreshTurnUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			frames := cframe(cmsg(3, cmsg(5, cvint(1, 42)))) // checkpoint context occupancy
			frames = append(frames, cframe(cmsg(1, cmsg(8, cvint(1, 17))))...)
			frames = append(frames, cframe(cmsg(1, cmsg(1, cstr(1, "answer"))))...)
			frames = append(frames, cend(`{}`)...)
			closed := make(chan struct{})
			run := providerformat.NewCursorRun(io.Discard, bytes.NewReader(frames), nil, func() { close(closed) }, time.Hour)
			t.Cleanup(run.Close)
			run.Start()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("reader did not finish the bounded fixture")
			}
			// Usage and data are already decoded when the HTTP response driver
			// begins. A caller-side reset here erased fast fresh-run usage.
			p := New(config.Default(), metrics.Noop{})
			rec := &metrics.Record{ID: "fixture", StatusCode: 200, Start: time.Now()}
			w := httptest.NewRecorder()
			p.driveRun(t.Context(), w, run, stream, rec, cursorTurnRender{model: "m", est: 3, includeUsage: true}, false, nil)
			if rec.Usage.InputTokens != 42 || rec.Usage.OutputTokens != 17 || !strings.Contains(w.Body.String(), `"content":"answer"`) {
				t.Fatalf("fresh driver lost decoded data: usage=%+v body=%s", rec.Usage, w.Body.String())
			}
		})
	}
}

package proxy

// Round 5 E4 regression: a pre-commit, proxy-generated status-line error
// envelope is not relayed body bytes. The pacer and non-pacer writers must leave
// ResponseBytes unchanged; only an upstream body that is relayed counts.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestErrorEnvelopeBytesNotCounted(t *testing.T) {
	pacerRec := &metrics.Record{}
	p := newSSEPacer(httptest.NewRecorder(), time.Hour, 0)
	alreadySSE := p.commitError(nil, pacerRec, ir.Chat, http.StatusBadGateway, "api_error", "boom")
	if alreadySSE {
		t.Fatal("commitError reported an SSE commit")
	}
	if pacerRec.ResponseBytes != 0 {
		t.Fatalf("pacer error bytes = %d, want 0", pacerRec.ResponseBytes)
	}

	plainRec := &metrics.Record{}
	recorder := httptest.NewRecorder()
	writeClientErrorHdr(recorder, plainRec, ir.Chat, "api_error", "boom", http.StatusBadGateway, nil)
	if plainRec.ResponseBytes != 0 {
		t.Fatalf("non-pacer error bytes = %d, want 0", plainRec.ResponseBytes)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("non-pacer writer produced no envelope")
	}
}

package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// TestValuesModelFailsClosedAtRawSpellingBound pins the first model vocabulary
// bound. Every raw spelling collapses through the default rules into the same
// canonical name, so only the raw-spelling counter can stop the walk. The tool
// must name that bound and return no partial vocabulary.
func TestValuesModelFailsClosedAtRawSpellingBound(t *testing.T) {
	total := modelValuesMaxRawSpellings + 1
	spellings := make([]string, total)
	for i := range spellings {
		spellings[i] = fmt.Sprintf("vendor%06d/glm-5.3", i)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, config.DefaultModelRules()))
	proxy.dynamic(http.MethodGet, schemaPath, modelPageResponder(spellings, 1, 0))
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
	if err == nil {
		t.Fatalf("raw model vocabulary past the bound must fail closed, got %d values", len(out.Values))
	}
	if out != nil {
		t.Fatalf("a failed enumeration must not return a partial vocabulary: %+v", out)
	}
	if !strings.Contains(err.Error(), "distinct raw spellings") ||
		!strings.Contains(err.Error(), strconv.Itoa(modelValuesMaxRawSpellings)) {
		t.Fatalf("error = %v, want the explicit raw-spelling bound %d", err, modelValuesMaxRawSpellings)
	}
}

// TestValuesModelFailsClosedAtCanonicalByteBound pins the byte bound that the
// canonical-name count cannot cover: a handful of very long distinct names
// stays well under modelValuesMaxCanonical but exceeds the retained-byte cap.
func TestValuesModelFailsClosedAtCanonicalByteBound(t *testing.T) {
	const nameBytes = 1024
	total := modelValuesMaxCanonicalBytes/nameBytes + 1
	spellings := make([]string, total)
	for i := range spellings {
		// Identity rules keep each long spelling as its own canonical name.
		spellings[i] = fmt.Sprintf("%04d", i) + strings.Repeat("a", nameBytes-4)
	}
	if total >= modelValuesMaxCanonical {
		t.Fatalf("fixture names = %d, must stay below the count bound %d", total, modelValuesMaxCanonical)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, nil))
	proxy.dynamic(http.MethodGet, schemaPath, modelPageResponder(spellings, 1, 0))
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
	if err == nil {
		t.Fatalf("long canonical names past the byte bound must fail closed, got %d values", len(out.Values))
	}
	if out != nil {
		t.Fatalf("a failed enumeration must not return a partial vocabulary: %+v", out)
	}
	if !strings.Contains(err.Error(), "canonical model names exceed") ||
		!strings.Contains(err.Error(), strconv.Itoa(modelValuesMaxCanonicalBytes)) {
		t.Fatalf("error = %v, want the explicit canonical-byte bound %d", err, modelValuesMaxCanonicalBytes)
	}
}

// TestValuesModelPageFloorFailsExplicitly pins the 413 floor: a proxy that
// refuses every positive LIMIT makes the tool halve the page down to one row,
// then report the failure. It must never ask for LIMIT 0 (the empty result that
// would be mistaken for a complete vocabulary) and must not panic on the empty
// page such a request returns.
func TestValuesModelPageFloorFailsExplicitly(t *testing.T) {
	var limits []int
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, modelBootstrap(t, nil))
	proxy.dynamic(http.MethodGet, schemaPath, func(r *http.Request) cannedResponse {
		statement := r.URL.Query().Get("q")
		const marker = " LIMIT "
		at := strings.LastIndex(statement, marker)
		if at < 0 {
			return cannedResponse{Status: http.StatusInternalServerError, ContentType: "application/json", Body: `{"error":"missing LIMIT"}`}
		}
		limit, err := strconv.Atoi(statement[at+len(marker):])
		if err != nil {
			return cannedResponse{Status: http.StatusInternalServerError, ContentType: "application/json", Body: `{"error":"invalid LIMIT"}`}
		}
		limits = append(limits, limit)
		if limit <= 0 {
			return cannedResponse{Status: http.StatusOK, ContentType: "application/json", Body: `[]`}
		}
		return cannedResponse{Status: http.StatusRequestEntityTooLarge, ContentType: "application/json",
			Body: `{"error":"query result exceeds storage_query_max_rows"}`}
	})
	service := newTestService(t, proxy, Limits{})

	out, err := service.values(context.Background(), ValuesInput{Dim: "model"})
	if err == nil {
		t.Fatalf("a page that cannot shrink below one row must fail explicitly, got %d values", len(out.Values))
	}
	if out != nil {
		t.Fatalf("a failed enumeration must not return a partial vocabulary: %+v", out)
	}
	if !strings.Contains(err.Error(), "enumerate model spellings") {
		t.Fatalf("error = %v, want the explicit page-floor refusal", err)
	}
	want := []int{1000, 500, 250, 125, 62, 31, 15, 7, 3, 1}
	if len(limits) != len(want) {
		t.Fatalf("page limits = %v, want the halving floor %v", limits, want)
	}
	for i := range want {
		if limits[i] != want[i] {
			t.Fatalf("page limits = %v, want the halving floor %v", limits, want)
		}
	}
}

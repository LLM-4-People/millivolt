package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestMetaRoundTripAndDistinctClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	if v, err := s.LoadMeta(ctx, "pause"); err != nil || v != "" {
		t.Fatalf("empty load = %q %v", v, err)
	}
	if err := s.SavePause(ctx, []byte(`{"all":true}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := s.LoadPause(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"all":true}` {
		t.Fatalf("load pause = %s", raw)
	}

	if err := s.SaveThrottle(ctx, []byte(`{"throttles":[{"provider":"alpha.example","concurrency":2}]}`)); err != nil {
		t.Fatal(err)
	}
	raw, err = s.LoadThrottle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "alpha.example") {
		t.Fatalf("load throttle = %s", raw)
	}

	s.Record(&metrics.Record{ID: "a", Client: "client-a", Provider: "alpha.example", Start: time.Now(), StatusCode: 200})
	s.Record(&metrics.Record{ID: "b", Client: "client-b", Provider: "beta.example", Start: time.Now(), StatusCode: 200})
	s.Record(&metrics.Record{ID: "c", Client: "client-a", Provider: "alpha.example", Start: time.Now(), StatusCode: 200})
	waitRows(t, s, 3)

	cs, err := s.ListClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0] != "client-a" || cs[1] != "client-b" {
		t.Fatalf("clients = %v", cs)
	}
	ps, err := s.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0] != "alpha.example" || ps[1] != "beta.example" {
		t.Fatalf("providers = %v", ps)
	}
}

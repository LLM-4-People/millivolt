package web

// The explorer payload reads the projection-owned conversation topology. That
// topology is mutated in place by append catch-up under the projection write
// lock, so the payload must be built while streamWindow still holds the read
// lease. This regression drives the real durable append + explorer paths
// together and validates every response against one coherent lineage snapshot.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func lineageTurn(id, conv, parent string, at time.Time) *metrics.Record {
	r := cacheRecord(id, at)
	r.Provider = "lineage.example"
	r.ConversationID, r.ParentConversationID = conv, parent
	return r
}

// lineageConcurrentScopes are four distinct aggregate keys that all match the
// whole fixture. Different keys fold independently instead of sharing one
// in-flight caller, so readers and appenders keep exercising the payload path.
func lineageConcurrentScopes() [][]scopeFilter {
	return [][]scopeFilter{
		nil,
		{{dim: "provider", id: "lineage.example"}},
		{{dim: "client", id: "cli"}},
		{{dim: "key", id: "k"}},
	}
}

// checkExplorerLineageSnapshot validates an unfiltered conversation payload
// against the fixture's invariants. Every fixture conversation has exactly one
// request, the root is the only main conversation, every other conversation
// declares an observed parent, and every filter in lineageConcurrentScopes
// matches the whole fixture, so the rail, footer and card counts must agree
// even when the returned payload is from an older append revision.
func checkExplorerLineageSnapshot(p explorerPayload, base, final int) error {
	if p.Dim != "conversation" {
		return fmt.Errorf("dim = %q", p.Dim)
	}
	if len(p.Groups) > xpNodeCap {
		return fmt.Errorf("groups = %d, cap %d", len(p.Groups), xpNodeCap)
	}
	if p.Total < int64(base) || p.Total > int64(final) {
		return fmt.Errorf("total = %d outside [%d,%d]", p.Total, base, final)
	}
	if p.Scope.Matches != p.Total || p.Scope.Errors != 0 || p.ErrorTot != 0 {
		return fmt.Errorf("scope = %+v, total=%d errors=%d", p.Scope, p.Total, p.ErrorTot)
	}
	rail := p.Rail["conversation"]
	if rail != int(p.Total) {
		return fmt.Errorf("rail conversation = %d, total = %d", rail, p.Total)
	}
	got := p.Conversations
	if got.Main != 1 || got.Unresolved != 0 || got.Sub != rail-1 {
		return fmt.Errorf("summary = %+v, rail = %d", got, rail)
	}
	if len(p.Groups) > rail {
		return fmt.Errorf("groups = %d, rail = %d", len(p.Groups), rail)
	}
	names := make(map[string]struct{}, len(p.Groups))
	for _, g := range p.Groups {
		if g.Name == "" {
			return fmt.Errorf("empty conversation name: %+v", g)
		}
		if _, dup := names[g.Name]; dup {
			return fmt.Errorf("duplicate conversation %q", g.Name)
		}
		names[g.Name] = struct{}{}
		if g.N != 1 {
			return fmt.Errorf("%s: n = %d, want one request", g.Name, g.N)
		}
		info := g.Conversation
		if info == nil {
			return fmt.Errorf("%s: missing conversation metadata", g.Name)
		}
		switch info.Role {
		case conversationMain:
			if info.ParentID != "" || info.ParentObserved != nil || info.ParentInScope != nil || len(info.ParentScope) != 0 || info.Issue != "" {
				return fmt.Errorf("%s: invalid main metadata %+v", g.Name, info)
			}
		case conversationSub:
			if !strings.HasPrefix(info.ParentID, "s:") {
				return fmt.Errorf("%s: sub parent %q", g.Name, info.ParentID)
			}
			if info.ParentObserved == nil || !*info.ParentObserved ||
				info.ParentInScope == nil || !*info.ParentInScope {
				return fmt.Errorf("%s: sub parent not observed in scope: %+v", g.Name, info)
			}
			if len(info.ParentScope) != 3 || info.Issue != "" {
				return fmt.Errorf("%s: sub parent scope %+v issue %q", g.Name, info.ParentScope, info.Issue)
			}
		default:
			return fmt.Errorf("%s: unexpected role %q", g.Name, info.Role)
		}
	}
	return nil
}

// TestExplorerLineagePayloadBuildsInsideReadLease pins the lease boundary
// itself instead of relying on appends and payload reads overlapping by luck.
// The hook runs at the payload's first lineage read. While the fixed explorer
// still holds streamWindow's projection read lease, the append can only become
// durable; its projection mutation waits for that lease. If the payload moves
// outside the lease, the hook observes the released lease, lets the real
// durable append reach the projection first, and the payload then mixes the
// old fold with the appended topology deterministically.
func TestExplorerLineagePayloadBuildsInsideReadLease(t *testing.T) {
	api, _ := projectionAPI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	now := time.Now().Add(-time.Hour)

	// The root starts as the only main conversation and the child observes it.
	for i, turn := range []struct{ id, conv, parent string }{
		{"root", "s:root", ""},
		{"child", "s:child", "s:root"},
	} {
		api.store.Record(lineageTurn(turn.id, turn.conv, turn.parent, now.Add(time.Duration(i)*time.Millisecond)))
	}
	if err := api.store.Flush(); err != nil {
		t.Fatal(err)
	}
	seed, err := api.explorer(ctx, "conversation", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkExplorerLineageSnapshot(seed, 2, 3); err != nil {
		t.Fatalf("seed payload: %v", err)
	}

	appendStart := make(chan struct{})
	appendDurable := make(chan struct{})
	appendDone := make(chan struct{})
	appendErr := make(chan error, 1)
	go func() {
		defer close(appendDone)
		<-appendStart
		// Declaring the child as the root's parent makes the appended topology
		// a cycle, so the payload summary changes shape without needing a new
		// parent dictionary slot that the old fold never interned.
		api.store.Record(lineageTurn("late", "s:root", "s:child", now.Add(2*time.Millisecond)))
		err := api.store.Flush()
		close(appendDurable)
		if err != nil {
			appendErr <- fmt.Errorf("flush append: %w", err)
			return
		}
		// The appender's own explorer drives the real projection catch-up. Its
		// result is irrelevant here; completion proves the append reached the
		// shared topology.
		if _, err := api.explorer(ctx, "conversation", nil, ""); err != nil {
			appendErr <- fmt.Errorf("explore appended topology: %w", err)
			return
		}
	}()

	var armed atomic.Bool
	armed.Store(true)
	api.testHooks = &aggregateTestHooks{lineage: func() {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		// The payload's first lineage read is inside streamWindow's read lease
		// on the fixed path and after it when the payload moved outside.
		leaseHeld := !api.projection.mu.TryLock()
		if !leaseHeld {
			api.projection.mu.Unlock()
		}
		close(appendStart)
		<-appendDurable
		if !leaseHeld {
			<-appendDone
		}
	}}
	t.Cleanup(func() { api.testHooks = nil })

	// A different scope from the seed request misses the last-result memo and
	// folds the fixture again over the same projection.
	scope := []scopeFilter{{dim: "provider", id: "lineage.example"}}
	p, err := api.explorer(ctx, "conversation", scope, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-appendErr:
		t.Fatal(err)
	default:
	}
	if err := checkExplorerLineageSnapshot(p, 2, 3); err != nil {
		t.Fatalf("reader payload built against an appended topology: %v", err)
	}
	<-appendDone

	final, err := api.explorer(ctx, "conversation", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if final.Total != 3 || final.Conversations != (conversationSummary{Unresolved: 2}) {
		t.Fatalf("final snapshot = %+v", final)
	}
	root := final.Groups[0]
	if root.Name != "s:root" || root.N != 2 || root.Conversation == nil ||
		root.Conversation.Role != conversationUnknown || root.Conversation.Issue != "cycle" {
		t.Fatalf("final root = %+v", root)
	}
}

func TestExplorerLineagePayloadStaysCoherentDuringAppend(t *testing.T) {
	api, _ := projectionAPI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	now := time.Now().Add(-time.Hour)

	const (
		baseRows    = 512
		appenders   = 2
		appendsEach = 24
		readers     = 2
		readsEach   = 48
	)
	finalRows := baseRows + appenders*appendsEach

	for i := range baseRows {
		conv := fmt.Sprintf("s:base-%03d", i)
		parent := ""
		if i > 0 {
			parent = "s:base-000"
		}
		api.store.Record(lineageTurn(fmt.Sprintf("base-%03d", i), conv, parent, now.Add(time.Duration(i)*time.Millisecond)))
	}
	if err := api.store.Flush(); err != nil {
		t.Fatal(err)
	}
	initial, err := api.explorer(ctx, "conversation", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkExplorerLineageSnapshot(initial, baseRows, finalRows); err != nil {
		t.Fatalf("initial payload: %v", err)
	}
	if initial.Total != baseRows || initial.Conversations != (conversationSummary{Main: 1, Sub: baseRows - 1}) {
		t.Fatalf("initial snapshot = %+v", initial)
	}

	scopes := lineageConcurrentScopes()
	errCh := make(chan error, 1)
	fail := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
		cancel()
	}
	explore := func(scope int) error {
		payload, err := api.explorer(ctx, "conversation", scopes[scope%len(scopes)], "")
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("explorer: %w", err)
		}
		if err := checkExplorerLineageSnapshot(payload, baseRows, finalRows); err != nil {
			return fmt.Errorf("explorer payload: %w", err)
		}
		return nil
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := range appenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			parent := "s:base-000"
			for turn := range appendsEach {
				conv := fmt.Sprintf("s:app-%d-%02d", worker, turn)
				api.store.Record(lineageTurn(
					fmt.Sprintf("app-%d-%02d", worker, turn),
					conv, parent, now.Add(time.Duration(worker*appendsEach+turn+1)*time.Millisecond),
				))
				if err := api.store.Flush(); err != nil {
					fail(fmt.Errorf("flush append %d/%d: %w", worker, turn, err))
					return
				}
				if err := explore(worker + turn); err != nil {
					fail(err)
					return
				}
				parent = conv
			}
		}()
	}
	for worker := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for turn := range readsEach {
				if err := explore(worker + turn); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	final, err := api.explorer(ctx, "conversation", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkExplorerLineageSnapshot(final, baseRows, finalRows); err != nil {
		t.Fatalf("final payload: %v", err)
	}
	if final.Total != int64(finalRows) || final.Conversations != (conversationSummary{Main: 1, Sub: finalRows - 1}) {
		t.Fatalf("final snapshot = %+v", final)
	}
}

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

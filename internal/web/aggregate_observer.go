package web

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sync"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// One immutable compiled rule set per current configuration, shared by query
// folds and observer metadata. Per-query fold memoization remains local to
// modelCanonizer; the observer's cross-call name memo below is keyed by this
// cache's revision.
type modelRuleCache struct {
	mu      sync.Mutex
	current *compiledModelRules
}

type compiledModelRules struct {
	identity, revision string
	rules              []config.ModelRule
	exec               []config.ModelRuleExec
}

// get returns the compiled rules for the current snapshot. The rules content
// is compared directly, so the identity marshal and hash run only when the
// operator actually changes them; the former per-event path serialized the
// whole rule set and hashed it again for every SSE event.
func (c *modelRuleCache) get(rules config.ModelCanon) *compiledModelRules {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || !slices.Equal(c.current.rules, rules.Rules) {
		identity := modelCanonIdentity(rules)
		hash := sha256.Sum256([]byte(identity))
		c.current = &compiledModelRules{
			identity: identity,
			revision: hex.EncodeToString(hash[:]),
			rules:    slices.Clone(rules.Rules),
			exec:     config.CompileModelRules(rules.Rules),
		}
	}
	return c.current
}

// modelNameMemoMax bounds the shared raw-to-canonical name memo. Model names
// are client-controlled, so an unbounded memo is a process-lifetime leak.
// Overflow names are still canonicalized per call; only retention stops.
const modelNameMemoMax = 8192

// modelNameMemo memoizes canonical spellings across observer calls. Entries
// are keyed by the compiled rule revision: a revision change discards the old
// map, so values produced by superseded rules are unreachable. The map is
// bounded by modelNameMemoMax.
type modelNameMemo struct {
	mu       sync.Mutex
	revision string
	names    map[string]string
}

// begin selects revision for one observation, discarding every entry from a
// superseded rule revision. The per-entry get/put revision checks keep
// interleaved observations correct if another goroutine switches revision
// concurrently.
func (m *modelNameMemo) begin(revision string) {
	m.mu.Lock()
	if m.revision != revision || m.names == nil {
		m.revision = revision
		m.names = make(map[string]string)
	}
	m.mu.Unlock()
}

// get returns the memoized canonical spelling for name under revision. A
// different current revision is a miss, never a stale hit.
func (m *modelNameMemo) get(revision, name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revision != revision {
		return "", false
	}
	canon, ok := m.names[name]
	return canon, ok
}

// put records one canonical spelling under revision. A changed revision
// replaces the whole map first; once the map is full, a new name is not
// retained (the caller still returns the computed value).
func (m *modelNameMemo) put(revision, name, canon string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revision != revision || m.names == nil {
		m.revision = revision
		m.names = make(map[string]string)
	}
	if len(m.names) >= modelNameMemoMax {
		if _, exists := m.names[name]; !exists {
			return
		}
	}
	m.names[name] = canon
}

type modelObserverState struct {
	Rules    *[]config.ModelRule `json:"rules,omitempty"`
	Revision string              `json:"revision"`
	Names    map[string]string   `json:"names"`
}

// ObserveModelNames serves only names carried by this observer response. The
// content identity pins semantics; bootstrap authorizes revision changes while
// late events with another identity require recapture, never a rule rollback.
func (a *AggAPI) ObserveModelNames(names []string) any {
	return a.observeModelNames(false, names)
}

func (a *AggAPI) observeModelNames(full bool, names []string) any {
	rules := a.modelCanon()
	compiled := a.modelRules.get(rules)
	a.modelNames.begin(compiled.revision)
	out := modelObserverState{Revision: compiled.revision, Names: make(map[string]string)}
	if full {
		if rules.Rules == nil {
			rules.Rules = []config.ModelRule{}
		}
		out.Rules = &rules.Rules
	}
	for _, name := range names {
		if _, exists := out.Names[name]; !exists {
			canon, ok := a.modelNames.get(compiled.revision, name)
			if !ok {
				canon = config.ApplyModelRules(compiled.exec, name)
				a.modelNames.put(compiled.revision, name, canon)
			}
			out.Names[name] = canon
		}
	}
	return out
}

// ObserveModels is the one provider wired to bootstrap and SSE. No record is
// changed, no history is queried, and regex work happens only on observers.
func (a *AggAPI) ObserveModels(full bool, groups ...[]*metrics.Record) any {
	// The SSE record/end path passes one record: skip the dedupe map and the
	// names slice it would build.
	if len(groups) == 1 && len(groups[0]) == 1 {
		if r := groups[0][0]; r != nil {
			names := [1]string{r.Model}
			return a.observeModelNames(full, names[:])
		}
		return a.observeModelNames(full, nil)
	}
	seen := make(map[string]struct{})
	var names []string
	for _, records := range groups {
		for _, r := range records {
			if r == nil {
				continue
			}
			if _, exists := seen[r.Model]; !exists {
				seen[r.Model] = struct{}{}
				names = append(names, r.Model)
			}
		}
	}
	return a.observeModelNames(full, names)
}

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
	// lastRules and lastCompiled are the slice and result of the last get. A
	// caller that hands back the same backing array (a cached snapshot, which
	// every observer path does within one config) skips the element compare the
	// 64-rule maximum would otherwise pay per event.
	lastRules    []config.ModelRule
	lastCompiled *compiledModelRules
}

type compiledModelRules struct {
	identity, revision string
	rules              []config.ModelRule
	exec               []config.ModelRuleExec
}

// get returns the compiled rules for the current snapshot. The rules content is
// compared directly only when the caller's slice is not the one the last get
// saw, so the identity marshal and hash run only when the operator actually
// changes them; the former per-event path serialized the whole rule set and
// hashed it again for every SSE event.
func (c *modelRuleCache) get(rules config.ModelCanon) *compiledModelRules {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastCompiled != nil && len(c.lastRules) == len(rules.Rules) &&
		(len(rules.Rules) == 0 || &rules.Rules[0] == &c.lastRules[0]) {
		return c.lastCompiled
	}
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
	c.lastRules, c.lastCompiled = rules.Rules, c.current
	return c.current
}

// modelNameMemoMax bounds the shared raw-to-canonical name memo. Model names
// are client-controlled, so an unbounded memo is a process-lifetime leak.
// Overflow names are still canonicalized per call; only retention stops.
const modelNameMemoMax = 8192

// modelNameMemo memoizes the whole single-name observer payload, not only the
// canonical spelling, so an SSE event that repeats a model name answers with one
// mutex and one map lookup: no rule application, no payload map and no boxing
// allocation. Entries are keyed by the compiled rule revision (a revision change
// discards the old map, so values produced by superseded rules are unreachable)
// and bounded by modelNameMemoMax. The cached payload's map is read-only, so the
// same pointer may be marshaled by concurrent observers.
type modelNameMemo struct {
	mu       sync.Mutex
	revision string
	entries  map[string]*modelObserverState
}

// state returns the cached single-name payload under revision, computing and
// recording it on a miss. A different current revision discards every entry
// first, so a stale payload is never returned.
func (m *modelNameMemo) state(compiled *compiledModelRules, name string) *modelObserverState {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reset(compiled.revision)
	if state, ok := m.entries[name]; ok {
		return state
	}
	state := &modelObserverState{
		Revision: compiled.revision,
		Names:    map[string]string{name: config.ApplyModelRules(compiled.exec, name)},
	}
	// Once the map is full, a new name is returned but not retained; the caller
	// still gets the computed payload.
	if len(m.entries) < modelNameMemoMax {
		m.entries[name] = state
	}
	return state
}

// begin selects revision for one bulk observation, discarding every entry from a
// superseded rule revision so a name list that later turns out empty still
// retires the old payloads. The per-entry revision check in state keeps
// interleaved observations correct if another goroutine switches revision.
func (m *modelNameMemo) begin(revision string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reset(revision)
}

func (m *modelNameMemo) reset(revision string) {
	if m.revision != revision || m.entries == nil {
		m.revision = revision
		m.entries = make(map[string]*modelObserverState)
	}
}

// canonical returns one cached canonical spelling for the bulk observer paths.
func (m *modelNameMemo) canonical(compiled *compiledModelRules, name string) string {
	return m.state(compiled, name).Names[name]
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
	// The SSE event path carries one name and no static rules: answer it from
	// the payload memo so the common event costs one lock and one lookup.
	if !full && len(names) == 1 {
		return a.modelNames.state(compiled, names[0])
	}
	a.modelNames.begin(compiled.revision)
	out := modelObserverState{Revision: compiled.revision, Names: make(map[string]string, len(names))}
	if full {
		if rules.Rules == nil {
			rules.Rules = []config.ModelRule{}
		}
		out.Rules = &rules.Rules
	}
	for _, name := range names {
		if _, exists := out.Names[name]; !exists {
			out.Names[name] = a.modelNames.canonical(compiled, name)
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

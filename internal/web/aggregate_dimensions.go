package web

import (
	"slices"
	"sync"
)

// Dimension positions are internal dictionary slots, never wire values.
// The names own the API dimension allowlist as well as index iteration.
const (
	dimClient = iota
	dimProvider
	dimModel
	dimConversation
	dimKey
	dimStatus
	dimTime
	dimTool
	dimError
	dimCount
)

var dimensionNames = [dimCount]string{"client", "provider", "model", "conversation", "key", "status", "time", "tool", "error"}

type dimensionDictionary struct {
	names  []string
	ids    map[string]uint32
	counts []int // projected memberships, including repeated tool/error occurrences
}

func (d *dimensionDictionary) intern(name string) uint32 {
	if d.ids == nil {
		d.ids = make(map[string]uint32)
		d.names = []string{""}
		d.counts = []int{0}
	}
	if name == "" {
		return 0
	}
	if id := d.ids[name]; id != 0 {
		return id
	}
	id := uint32(len(d.names))
	d.names = append(d.names, name)
	d.counts = append(d.counts, 0)
	d.ids[name] = id
	return id
}

type contribDimensions struct {
	dict [dimCount]dimensionDictionary
	// errorText dedupes repeated error type/code/message bytes across rows.
	// The joined filter key remains the error dictionary's name; this map only
	// shares the individual fields the cards and lineage expose.
	errorText map[string]string
}

// internErrorText returns the shared copy of one error field.
func (d *contribDimensions) internErrorText(s string) string {
	if s == "" {
		return ""
	}
	if shared, ok := d.errorText[s]; ok {
		return shared
	}
	if d.errorText == nil {
		d.errorText = make(map[string]string)
	}
	d.errorText[s] = s
	return s
}

// dimField owns the stored single-valued dimension mapping. Both the value
// reader and projection interning use it, so rows can share dictionary-owned
// string bytes without a second copy of dimension semantics.
func dimField(dim string, c *contrib) *string {
	switch dim {
	case "client":
		return &c.client
	case "provider":
		return &c.prov
	case "model":
		return &c.model
	case "conversation":
		return &c.conv
	case "key":
		return &c.key
	}
	return nil
}

// Intern derived membership once per materialized record. IDs preserve tool
// and error occurrence order/multiplicity; only query membership is a set.
func (d *contribDimensions) intern(c *contrib) {
	for dim := range dimTool {
		name, ok := dimValue(dimensionNames[dim], c)
		if ok || dim == dimModel {
			id := d.dict[dim].intern(name)
			c.dimensionIDs[dim] = id
			d.dict[dim].counts[id]++
			if field := dimField(dimensionNames[dim], c); field != nil {
				// SQL decodes equal labels into distinct allocations. The
				// dictionary already owns their canonical immutable bytes.
				*field = d.dict[dim].names[id]
			}
		}
	}
	if c.parentConv != "" {
		// A declaration can name a parent before its first observed request.
		// Reuse the conversation dictionary's bytes, but do not increment its
		// membership count: a parent reference is not an observed conversation.
		id := d.dict[dimConversation].intern(c.parentConv)
		c.parentConv = d.dict[dimConversation].names[id]
		c.parentConvID = id
	}
	c.toolIDs = make([]uint32, len(c.toolsL))
	for i, name := range c.toolsL {
		id := d.dict[dimTool].intern(name)
		c.toolIDs[i] = id
		// Share the dictionary's canonical bytes, like the single-valued
		// label fields above: per-row decode copies become garbage instead
		// of staying live for the history's lifetime.
		c.toolsL[i] = d.dict[dimTool].names[id]
		d.dict[dimTool].counts[id]++
	}
	c.errorIDs = make([]uint32, len(c.ent))
	for i := range c.ent {
		e := &c.ent[i]
		e.typ = d.internErrorText(e.typ)
		e.code = d.internErrorText(e.code)
		e.msg = d.internErrorText(e.msg)
		c.errorIDs[i] = d.dict[dimError].intern(errorKey(*e))
		d.dict[dimError].counts[c.errorIDs[i]]++
	}
}

func errorKey(e errEnt) string { return e.typ + "|" + e.code + "|" + e.msg }

// Bind model filters to the raw dictionary once, keeping projection rows
// immutable so readers can fold pointers without copying every contribution.
// Ring extras still carry fromRecord's canonical spelling. Never mutate the
// caller's filter slice: chart and cross-filter gallery may share that input.
func bindScope(fs []scopeFilter, p *projectionData, mcz *modelCanonizer) []scopeFilter {
	if p == nil {
		return fs
	}
	var out []scopeFilter
	var ids []uint32
	var models dimensionDictionary
	for i, f := range fs {
		if f.dim != "model" {
			continue
		}
		if out == nil {
			out = append([]scopeFilter(nil), fs...)
		}
		names := p.dimensions.dict[dimModel].names
		if ids == nil {
			ids, models = p.canonicalModels(mcz, names)
		}
		out[i].modelIDs = make([]bool, len(names))
		for id, name := range names {
			canonical := name
			if id < len(ids) {
				canonical = models.names[ids[id]]
			} else {
				canonical = mcz.model(name)
			}
			out[i].modelIDs[id] = canonical == f.id
		}
	}
	if out == nil {
		return fs
	}
	return out
}

// modelProjectionCache owns the canonical model IDs and canonical dictionary
// for one projection build. Appends extend the immutable prefix without
// re-running the rules over history; a destructive replacement gets a fresh
// cache from the new projection. A compiled-rules identity change rebuilds.
// The cache is shared by chart/explorer/scope binding, never by the bounded
// observer memo (which serves arbitrary observer names instead).
type modelProjectionCache struct {
	mu       sync.Mutex
	ready    bool
	version  int64
	revision string
	modelIDs []uint32
	models   dimensionDictionary
}

func newModelProjectionCache() *modelProjectionCache { return &modelProjectionCache{} }

func (c *modelProjectionCache) canonical(version int64, revision string, raw []string, mcz *modelCanonizer) ([]uint32, dimensionDictionary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready && c.revision == revision && c.version <= version && len(c.modelIDs) == len(raw) {
		c.version = version
		return c.modelIDs, c.models
	}
	if c.ready && c.revision == revision && c.version <= version && len(c.modelIDs) < len(raw) {
		// The projection's raw-model dictionary only appends within one build:
		// extend the canonical prefix instead of re-canonicalizing history.
		ids := make([]uint32, len(raw))
		copy(ids, c.modelIDs)
		models := dimensionDictionary{
			names: slices.Clone(c.models.names),
			ids:   make(map[string]uint32, len(c.models.ids)+len(raw)-len(c.modelIDs)),
		}
		for name, id := range c.models.ids {
			models.ids[name] = id
		}
		for i := len(c.modelIDs); i < len(raw); i++ {
			ids[i] = models.intern(mcz.model(raw[i]))
		}
		models.counts = nil
		c.modelIDs, c.models, c.version = ids, models, version
		return ids, models
	}
	models := dimensionDictionary{names: []string{""}, ids: make(map[string]uint32)}
	ids := make([]uint32, len(raw))
	for i, name := range raw {
		ids[i] = models.intern(mcz.model(name))
	}
	models.counts = nil
	c.ready, c.revision, c.version = true, revision, version
	c.modelIDs, c.models = ids, models
	return ids, models
}

// canonicalModels returns the canonical ID map and dictionary for this
// projection. Ad-hoc test/benchmark projections without a cache still build a
// transient dictionary, preserving the uncached semantics.
func (p *projectionData) canonicalModels(mcz *modelCanonizer, raw []string) ([]uint32, dimensionDictionary) {
	revision := ""
	if mcz != nil {
		revision = mcz.identity
	}
	if p.modelCache != nil {
		return p.modelCache.canonical(p.version, revision, raw, mcz)
	}
	models := dimensionDictionary{names: []string{""}, ids: make(map[string]uint32)}
	ids := make([]uint32, len(raw))
	for i, name := range raw {
		ids[i] = models.intern(mcz.model(name))
	}
	models.counts = nil
	return ids, models
}

// queryDimensions borrows immutable dictionary tables for one read-locked
// projection. The canonical model dictionary is projection-owned and keyed by
// the projection version plus the compiled rules revision; only ring/pending
// extras are interned locally. No per-row regex or string-map grouping runs.
type queryDimensions struct {
	base     [dimCount]dimensionDictionary
	extra    [dimCount]dimensionDictionary
	modelIDs []uint32
}

func (q *queryDimensions) prepare(p *projectionData, mcz *modelCanonizer) {
	if p == nil {
		return
	}
	q.base = p.dimensions.dict
	q.modelIDs, q.base[dimModel] = p.canonicalModels(mcz, q.base[dimModel].names)
}

func (q *queryDimensions) intern(dim int, name string) uint32 {
	if name == "" {
		return 0
	}
	if id := q.base[dim].ids[name]; id != 0 {
		return id
	}
	id := q.extra[dim].intern(name)
	base := len(q.base[dim].names)
	if base > 0 {
		id += uint32(base - 1)
	}
	return id
}

// Resolve the single-valued tuple once, rather than repeating projection and
// model checks for every dimension on every row. Only ring/pending extras
// need string lookups; the dictionaries remain the sole membership owner.
func (q *queryDimensions) singles(c *contrib) [dimTool]uint32 {
	ids := c.dimensionIDs
	if ids[dimStatus] != 0 {
		ids[dimModel] = q.modelIDs[ids[dimModel]]
		return ids
	}
	for dim := range dimTool {
		if name, ok := dimValue(dimensionNames[dim], c); ok {
			ids[dim] = q.intern(dim, name)
		}
	}
	return ids
}

func (q *queryDimensions) id(dim int, c *contrib, offset int) uint32 {
	if dim == dimTool {
		if offset < len(c.toolIDs) {
			return c.toolIDs[offset]
		}
		return q.intern(dim, c.toolsL[offset])
	}
	if offset < len(c.errorIDs) {
		return c.errorIDs[offset]
	}
	return q.intern(dim, errorKey(c.ent[offset]))
}

func (q *queryDimensions) name(dim int, id uint32) string {
	base := q.base[dim].names
	if int(id) < len(base) {
		return base[id]
	}
	if len(base) > 0 {
		id -= uint32(len(base) - 1)
	}
	return q.extra[dim].names[id]
}

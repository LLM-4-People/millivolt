package web

import "strings"

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

type contribDimensions struct{ dict [dimCount]dimensionDictionary }

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
	}
	c.toolIDs = make([]uint32, len(c.toolsL))
	for i, name := range c.toolsL {
		c.toolIDs[i] = d.dict[dimTool].intern(name)
		d.dict[dimTool].counts[c.toolIDs[i]]++
	}
	c.errorIDs = make([]uint32, len(c.ent))
	for i, ent := range c.ent {
		c.errorIDs[i] = d.dict[dimError].intern(errorKey(ent))
		d.dict[dimError].counts[c.errorIDs[i]]++
	}
}

func errorKey(e errEnt) string { return strings.Join([]string{e.typ, e.code, e.msg}, "|") }

// Bind model filters to the raw dictionary once, keeping projection rows
// immutable so readers can fold pointers without copying every contribution.
// Ring extras still carry fromRecord's canonical spelling. Never mutate the
// caller's filter slice: chart and cross-filter gallery may share that input.
func bindScope(fs []scopeFilter, p *projectionData, mcz *modelCanonizer) []scopeFilter {
	if p == nil {
		return fs
	}
	var out []scopeFilter
	for i, f := range fs {
		if f.dim != "model" {
			continue
		}
		if out == nil {
			out = append([]scopeFilter(nil), fs...)
		}
		names := p.dimensions.dict[dimModel].names
		out[i].modelIDs = make([]bool, len(names))
		for id, name := range names {
			out[i].modelIDs[id] = mcz.model(name) == f.id
		}
	}
	if out == nil {
		return fs
	}
	return out
}

// queryDimensions borrows immutable dictionary tables for one read-locked
// projection. Only the small raw-model dictionary is re-grouped when rules
// change; no per-row regex or string-map grouping is needed. Ring/pending
// extras use local IDs after the borrowed namespace, without mutating it.
type queryDimensions struct {
	base     [dimCount]dimensionDictionary
	extra    [dimCount]dimensionDictionary
	modelIDs []uint32
	models   dimensionDictionary
}

func (q *queryDimensions) prepare(p *projectionData, mcz *modelCanonizer) {
	if p == nil {
		return
	}
	q.base = p.dimensions.dict
	raw := q.base[dimModel].names
	q.modelIDs = make([]uint32, len(raw))
	for i, name := range raw {
		id := q.models.intern(mcz.model(name))
		q.modelIDs[i] = id
		q.models.counts[id] += q.base[dimModel].counts[i]
	}
	q.base[dimModel] = q.models
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

func (q *queryDimensions) count(dim int, id uint32) int {
	counts := q.base[dim].counts
	if int(id) < len(counts) {
		return counts[id]
	}
	return 0
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

package web

import (
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// ---------- explorer breakdown endpoint ----------

// jsonEnt is the per-entity aggregate a node-card renders - every field the
// per-dimension KPI sets need, precomputed server-side.
type jsonEnt struct {
	Conversation      *conversationInfo `json:"conversation,omitempty"`
	Name              string            `json:"name"`
	N                 int64             `json:"n"` // affected requests for error/tool cards, occurrences otherwise
	Cost              float64           `json:"cost"`
	In                int64             `json:"in"`
	Out               int64             `json:"out"`
	Cache             int64             `json:"cache"`
	ErrFinal          int64             `json:"err_final"`
	RateLimitRequests int64             `json:"rate_limit_requests"`
	Tools             int64             `json:"tools"` // ToolCalls summed once per affected request: calls, not requests
	// Blended $/Mtok over this entity's COST-REPORTING in+out tokens
	// (Totals' CostInOut rule, mirrored per entity) - nil when the
	// denominator is 0. An unpriced provider's tokens never dilute it.
	CostPerMTok *float64 `json:"cost_per_mtok"`
	TTFTP50     *float64 `json:"ttft_p50"`
	TTFTP95     *float64 `json:"ttft_p95"`
	TPSP50      *float64 `json:"tps_p50"`
	TPSP95      *float64 `json:"tps_p95"`
	ErrEvents   int64    `json:"err_events"` // error-dimension failure events (final plus absorbed)
	Code        string   `json:"code"`
	LastMs      int64    `json:"last_ms"`
	// Synthetic marks the overflow card. Its Name is a display label, not a
	// filter value; clients must not select it or confuse it with a real value
	// that happens to share the label.
	Synthetic bool `json:"synthetic,omitempty"`
}

type explorerPayload struct {
	Conversations conversationSummary `json:"conversation_summary"`
	Dim           string              `json:"dim"`
	Total         int64               `json:"total"`
	ErrorTot      int64               `json:"error_total"`
	Rail          map[string]int      `json:"rail"`
	Groups        []jsonEnt           `json:"groups"`
	Scope         scopePayload        `json:"scope"`
}

// scopePayload follows every filter, including the active dimension's own
// filter. The gallery's Total/ErrorTot intentionally exclude that one filter.
type scopePayload struct {
	Matches int64 `json:"matches"`
	Errors  int64 `json:"errors"`
}

type entAcc struct {
	id                uint32
	name              string
	n                 int
	cost              float64
	in                int64
	out               int64
	cacheR            int64
	tools             int64
	errFin            int
	rateLimitRequests int
	lastRequest       string // first membership in the current row; no history-ID set
	ttftS             []int64
	tpsS              []float64
	ttftN             int
	tpsN              int
	// cost-REPORTING in+out tokens: the blended-$ denominator (unpriced
	// records add tokens but not cost, so they must not dilute).
	costInOut float64
	// error dim only
	code     string
	lastMs   int64
	affected map[string]struct{}
}

var validDims = func() map[string]bool {
	dims := make(map[string]bool, len(dimensionNames))
	for _, name := range dimensionNames {
		dims[name] = true
	}
	return dims
}()

// scopeDimBits derives each filter dimension's grouping bit from the canonical
// dimensionNames order, so the scope matcher and planner cannot drift from the
// dimension vocabulary.
var scopeDimBits = func() map[string]uint32 {
	bits := make(map[string]uint32, len(dimensionNames))
	for i, name := range dimensionNames {
		bits[name] = 1 << i
	}
	return bits
}()

func scopeDimBit(dim string) uint32 { return scopeDimBits[dim] }

// A row may belong to multiple groups, including repeated tool/error
// occurrences. Spans index one flat membership list without per-row slices.
type explorerMemberSpan struct{ start, count uint32 }

// explorerSelectedGroup pairs one returned card's dictionary id with its
// percentile bucket (rank+1). A zero bucket marks an empty open-addressed slot.
type explorerSelectedGroup struct {
	id     uint32
	bucket uint8
}

// explorerDenseSelection bounds the direct-index selected-group table. Common
// small-cardinality dimensions (provider, status, client, key) assign ids below
// it, so their samples keep a single array load per occurrence exactly like the
// old maxGroupID-sized slice. Low selected ids always populate this table, even
// when another selected id is history-scale. This is an internal guardrail, not
// a tunable.
const explorerDenseSelection = 256

// explorerHighSelection is the fixed slot count for the high-id table. It is a
// power of two and far above xpNodeCap (24), so at most xpNodeCap inserts can
// never fill it and linear probing always reaches an empty slot. The bounded
// probe loops below still guard the invariant instead of looping forever if a
// future cap outgrows it. Internal implementation constants, not tunables.
const (
	explorerHighSelection = 128
	explorerHighHash      = 2654435761 // Knuth multiplicative hash
)

// explorerHighSlot is the deterministic initial slot for one high id.
func explorerHighSlot(id uint32) int {
	return int((id * explorerHighHash) & (explorerHighSelection - 1))
}

// explorerSelectedGroups is one payload request's compact selected-group
// lookup, built from the returned top cards only. Dictionary ids are
// dimension-wide positions that can sit far above the card count, so the
// previous dense []uint8 sized by maxGroupID allocated kilobytes to megabytes
// per request to represent at most xpNodeCap entries. Low ids stay in the fixed
// dense table; high ids use one fixed-capacity open-addressed table, so both
// lookups are constant-time probes without allocation or maxGroupID scaling.
// Selections are never carried across requests.
type explorerSelectedGroups struct {
	dense [explorerDenseSelection]uint8
	high  [explorerHighSelection]explorerSelectedGroup
}

// newExplorerSelectedGroups ranks no groups of its own: the payload has already
// capped top at xpNodeCap, and duplicate ids cannot occur because top holds one
// entry per group map key. Every selected id populates exactly one table.
func newExplorerSelectedGroups(top []*entAcc) explorerSelectedGroups {
	var s explorerSelectedGroups
	for i, g := range top {
		bucket := uint8(i + 1)
		if g.id < explorerDenseSelection {
			s.dense[g.id] = bucket
			continue
		}
		slot := explorerHighSlot(g.id)
		for probes := 0; probes < explorerHighSelection; probes++ {
			entry := &s.high[slot]
			if entry.bucket == 0 {
				*entry = explorerSelectedGroup{id: g.id, bucket: bucket}
				break
			}
			if entry.id == g.id {
				entry.bucket = bucket
				break
			}
			slot = (slot + 1) & (explorerHighSelection - 1)
		}
	}
	return s
}

// bucket returns the group's percentile bucket, or zero when the id is not one
// of the returned cards. Low ids are one array load; high ids probe the fixed
// table until their exact id or an empty slot. The loop is bounded, so a missing
// id can never spin even if the capacity invariant were broken.
func (s *explorerSelectedGroups) bucket(id uint32) uint8 {
	if id < explorerDenseSelection {
		return s.dense[id]
	}
	slot := explorerHighSlot(id)
	for probes := 0; probes < explorerHighSelection; probes++ {
		entry := &s.high[slot]
		if entry.bucket == 0 {
			return 0
		}
		if entry.id == id {
			return entry.bucket
		}
		slot = (slot + 1) & (explorerHighSelection - 1)
	}
	return 0
}

// explorerFold accumulates the per-dimension breakdown (groups, rail
// distinct-value counts, full-scope footer counts, error-affected sets) over
// one stream of contributions. The footer never needs a second history scan.
type explorerFold struct {
	lineage       conversationLineage
	projectedRail bool
	dim           string
	dimIndex      int
	statusCode    string
	allFS         []scopeFilter
	grpFS         []scopeFilter
	dims          queryDimensions
	railSeen      [dimCount][]uint64
	railCount     [dimCount]int
	groups        map[uint32]*entAcc
	// other aggregates every distinct value beyond explorerGroupBudget. It
	// participates in ranking like a card but owns no per-value map: an error
	// row's affected-request count comes from lastRequest, which is exact
	// because one row's occurrences arrive contiguously.
	other         entAcc
	otherAffected int
	overflow      bool
	order         metricOrderSource
	rowCount      int
	// Single-valued dimensions store one group id per projected row (4 bytes).
	// Only tool/error need the flat multi-membership list + spans.
	singleMember []uint32
	multiMember  bool
	memberSpans  []explorerMemberSpan
	members      []uint32
	pendingIDs   map[string]struct{}
	seen         map[string]struct{}
	total        int64
	errTot       int64
	// errAffected is the error dimension's share denominator: the union of
	// affected requests represented by its cards. Every contrib is one request
	// folded exactly once, so counting rows with at least one error entry is
	// the union without a second history-scale ID set. It is deliberately
	// broader than errTot (Record.IsError): an absorbed quality failure is an
	// error card membership even when the final status is 200.
	errAffected int64
	scope       scopePayload
}

func newExplorerFold(dim, statusCode string, allFS []scopeFilter) *explorerFold {
	// Cross-filter: the breakdown ignores the active dim's own filter value.
	grpFS := allFS[:0:0]
	for _, f := range allFS {
		if f.dim != dim {
			grpFS = append(grpFS, f)
		}
	}
	e := &explorerFold{
		dim: dim, statusCode: statusCode, allFS: allFS, grpFS: grpFS,
		groups: make(map[uint32]*entAcc),
		other:  entAcc{id: explorerOtherID, name: explorerOtherName},
		seen:   make(map[string]struct{}),
	}
	// Unprepared folds (direct fixtures) still need a names source for the
	// lineage payload; prepare borrows the same query dictionary owner.
	e.lineage.names = &e.dims
	for i, name := range dimensionNames {
		if name == dim {
			e.dimIndex = i
			break
		}
	}
	return e
}

func (e *explorerFold) prepare(p *projectionData, mcz *modelCanonizer) {
	e.dims.prepare(p, mcz)
	e.allFS = bindScope(e.allFS, p, mcz)
	e.grpFS = bindScope(e.grpFS, p, mcz)
	e.lineage.prepare(p, &e.dims)
	if p != nil {
		e.order = p.metrics.allOrderSource()
		// The unfiltered rail is exactly the shared dictionary's occupied
		// memberships. Only unprojected ring/pending names can add anything;
		// scoped rails continue to follow the canonical per-row predicate.
		e.projectedRail = e.statusCode == "" && len(e.allFS) == 0
		if e.projectedRail {
			for dim, dictionary := range e.dims.base {
				if dim == dimModel {
					// Canonical model IDs share the projection's raw membership
					// counts; the canonical dictionary intentionally carries no
					// counts of its own (they change on every append).
					continue
				}
				for id, n := range dictionary.counts {
					if id > 0 && n > 0 {
						e.markRail(dim, uint32(id))
					}
				}
			}
			rawCounts := p.dimensions.dict[dimModel].counts
			for rawID, n := range rawCounts {
				if n > 0 && rawID < len(e.dims.modelIDs) {
					e.markRail(dimModel, e.dims.modelIDs[rawID])
				}
			}
		}
		e.rowCount = len(p.rows)
		e.multiMember = e.dimIndex == dimTool || e.dimIndex == dimError
		// Only pending candidates can be replayed by the captured live snapshot.
		// Never allocate a second full-history request-ID set for this query.
		for id := range e.pendingIDs {
			if _, finalized := p.ids[id]; finalized {
				e.seen[id] = struct{}{}
			}
		}
	}
}

func (e *explorerFold) markRail(dim int, id uint32) {
	if id == 0 {
		return
	}
	word, bit := int(id/64), uint64(1)<<(id%64)
	seen := e.railSeen[dim]
	if word >= len(seen) {
		seen = append(seen, make([]uint64, word+1-len(seen))...)
		e.railSeen[dim] = seen
	}
	if seen[word]&bit == 0 {
		seen[word] |= bit
		e.railCount[dim]++
	}
}

func (e *explorerFold) fold(c *contrib) error {
	// Only ring-only rows can be pending here: prepare already marked every
	// pending ID the projection contains, so hashing the ID for projected rows
	// is a redundant map probe per row.
	if c.rowIndex == 0 {
		if _, pending := e.pendingIDs[c.id]; pending {
			e.seen[c.id] = struct{}{}
		}
	}
	inRail := scopedMatch(c, e.statusCode, e.allFS)
	if inRail {
		e.scope.Matches++
		if c.isErr {
			e.scope.Errors++
		}
	}
	// grpFS is allFS minus the active dim. Do not AND with inRail -
	// that made inGroup ≡ inRail and collapsed the gallery to 100%.
	inGroup := scopedMatch(c, e.statusCode, e.grpFS)
	ids := e.dims.singles(c)
	parent := c.parentConvID
	if c.rowIndex == 0 {
		if name := c.parentLabel(); name != "" {
			parent = e.dims.intern(dimConversation, name)
		}
	}
	e.lineage.observeRow(c, ids, parent, inRail, inGroup && e.dimIndex == dimConversation)
	if inGroup {
		e.total++
		if c.isErr {
			e.errTot++
		}
		// The error gallery denominator is the union of the requests its cards
		// represent. Each contribution is one request, so this counts every
		// affected request exactly once regardless of how many failure events
		// or error signatures it carries.
		if e.dimIndex == dimError && len(c.ent) > 0 {
			e.errAffected++
		}
	}
	if !inRail && !inGroup {
		return nil
	}
	needsRail := inRail && !(e.projectedRail && c.rowIndex > 0)
	for dim, id := range ids {
		if !needsRail && dim != e.dimIndex {
			continue
		}
		if id == 0 {
			continue
		}
		if needsRail {
			e.markRail(dim, id)
		}
		if inGroup && dim == e.dimIndex {
			if _, err := e.foldGroup(id, c); err != nil {
				return err
			}
		}
	}
	if needsRail || e.dimIndex == dimTool {
		for i := range c.toolsL {
			id := e.dims.id(dimTool, c, i)
			if id == 0 {
				continue
			}
			if needsRail {
				e.markRail(dimTool, id)
			}
			if inGroup && e.dimIndex == dimTool {
				if _, err := e.foldGroup(id, c); err != nil {
					return err
				}
			}
		}
	}
	if needsRail || e.dimIndex == dimError {
		for i, ent := range c.ent {
			id := e.dims.id(dimError, c, i)
			if needsRail {
				e.markRail(dimError, id)
			}
			if inGroup && e.dimIndex == dimError {
				g, err := e.foldGroup(id, c)
				if err != nil {
					return err
				}
				if ent.at > g.lastMs {
					g.lastMs = ent.at
				}
				// Mixed overflow errors have no single representative code;
				// leave it empty instead of naming one arbitrary failure.
				if g != &e.other && g.code == "" {
					g.code = ent.code
				}
			}
		}
	}
	return nil
}

func (e *explorerFold) foldGroup(dimKey uint32, c *contrib) (*entAcc, error) {
	g := e.groups[dimKey]
	if g == nil {
		if len(e.groups) >= explorerGroupBudget {
			// Stop allocating exact groups at the budget. The synthetic card
			// aggregates every later distinct value; repeated occurrences of
			// an already tracked value still reach it through the map above.
			e.overflow = true
			g = &e.other
		} else {
			g = &entAcc{id: dimKey, name: e.dims.name(e.dimIndex, dimKey)}
			e.groups[dimKey] = g
		}
	}
	// Health counts are affected requests. Error cards keep occurrence
	// multiplicity for ErrEvents, samples, cost and tokens, while their N is
	// the distinct affected-request count. Tool cards fold one contribution
	// per request per tool name (a nonstream response repeats a name once per
	// call), so their N, share and request-level accumulators are
	// request-based while the calls KPI still sums ToolCalls. Error groups need
	// the affected-ID set for their headline; the overflow bucket only needs
	// the last ID because one row's visits are contiguous. Tool duplicates are
	// contiguous too, so the same last-ID guard dedupes them.
	var firstRequest bool
	if e.dimIndex == dimError && g != &e.other {
		if g.affected == nil {
			g.affected = make(map[string]struct{})
		}
		_, duplicate := g.affected[c.id]
		firstRequest = !duplicate
		if firstRequest {
			g.affected[c.id] = struct{}{}
		}
	} else {
		firstRequest = g.n == 0 || g.lastRequest != c.id
		g.lastRequest = c.id
		if e.dimIndex == dimError && firstRequest {
			e.otherAffected++
		}
	}
	if e.dimIndex == dimTool && !firstRequest {
		return g, nil
	}
	g.n++
	// Same accumulator contract as the chart fold: every contribution is
	// admitted, every step's overflow is checked, nothing re-validates the
	// accumulator itself.
	if err := metrics.AddValue(&g.cost, c.cost); err != nil {
		return nil, err
	}
	if c.cost > 0 {
		if err := metrics.AddValue(&g.costInOut, float64(c.in)); err != nil {
			return nil, err
		}
		if err := metrics.AddValue(&g.costInOut, float64(c.out)); err != nil {
			return nil, err
		}
	}
	if err := metrics.AddCount(&g.in, c.in); err != nil {
		return nil, err
	}
	if err := metrics.AddCount(&g.out, c.out); err != nil {
		return nil, err
	}
	if err := metrics.AddCount(&g.cacheR, c.cacheR); err != nil {
		return nil, err
	}
	if err := metrics.AddCount(&g.tools, c.tools); err != nil {
		return nil, err
	}
	if firstRequest {
		if c.isErr {
			g.errFin++
		}
		if c.has429 {
			g.rateLimitRequests++
		}
	}
	if c.rowIndex > 0 {
		if !e.multiMember {
			// One group per row for every single-valued dimension (client,
			// provider, model, key, status, time, conversation). Previously
			// every explorer request allocated a span and a membership slot
			// per projected row even for these.
			if e.singleMember == nil {
				e.singleMember = make([]uint32, e.rowCount)
			}
			e.singleMember[c.rowIndex-1] = g.id
		} else {
			if e.memberSpans == nil {
				e.memberSpans = make([]explorerMemberSpan, e.rowCount)
			}
			span := &e.memberSpans[c.rowIndex-1]
			if span.count == 0 {
				span.start = uint32(len(e.members))
			}
			span.count++
			e.members = append(e.members, g.id)
		}
	}
	// Durable samples stay in immutable metric order. Only raw ring/pending
	// extras need per-group values; error counts keep occurrence multiplicity
	// while every other dimension contributes one sample per affected request.
	if validMetricSample(c.ttft) {
		g.ttftN++
		if c.rowIndex == 0 {
			g.ttftS = append(g.ttftS, c.ttft)
		}
	}
	if validMetricSample(c.tps) {
		g.tpsN++
		if c.rowIndex == 0 {
			g.tpsS = append(g.tpsS, c.tps)
		}
	}
	return g, nil
}

// groupCount is a card's headline count: affected requests for error and tool
// cards, occurrences for every other dimension. Error keeps event occurrences
// separately in ErrEvents. Ranking and the wire N field read the same owner.
func (e *explorerFold) groupCount(g *entAcc) int {
	if e.dimIndex == dimError {
		if g == &e.other {
			return e.otherAffected
		}
		return len(g.affected)
	}
	return g.n
}

// rankedGroups selects the returned top cards before sample derivation:
// discarded groups never allocate response arrays or perform sample selection.
// Counts follow groupCount; names break ties deterministically, independently
// of dictionary insertion or map order. The overflow bucket ranks by its
// aggregate count but allocates nothing per excluded value.
func (e *explorerFold) rankedGroups() []*entAcc {
	capacity := len(e.groups)
	if e.overflow {
		capacity++
	}
	top := make([]*entAcc, 0, min(capacity, xpNodeCap))
	insert := func(g *entAcc) {
		n := e.groupCount(g)
		at := sort.Search(len(top), func(i int) bool {
			other := e.groupCount(top[i])
			return n > other || n == other && (g.name < top[i].name ||
				g.name == top[i].name && g.id < top[i].id)
		})
		if at == xpNodeCap {
			return
		}
		if len(top) < xpNodeCap {
			top = append(top, nil)
		}
		copy(top[at+1:], top[at:])
		top[at] = g
	}
	for _, g := range e.groups {
		insert(g)
	}
	if e.overflow {
		insert(&e.other)
	}
	return top
}

// selectedVisitor maps projection rows to compact selected-group buckets for
// the metric rank visitors. Multi-valued dimensions walk the row's occurrence
// span and emit one bucket per selected occurrence in source order, preserving
// duplicate tool/error multiplicity; single-valued dimensions keep one group id
// per row. Unselected ids emit nothing.
func (e *explorerFold) selectedVisitor(selected *explorerSelectedGroups) func(row uint32, emit func(uint8)) {
	if e.multiMember {
		return func(row uint32, emit func(uint8)) {
			if uint64(row) >= uint64(len(e.memberSpans)) {
				return
			}
			span := e.memberSpans[row]
			for _, id := range e.members[span.start : span.start+span.count] {
				if bucket := selected.bucket(id); bucket != 0 {
					emit(bucket)
				}
			}
		}
	}
	return func(row uint32, emit func(uint8)) {
		if uint64(row) < uint64(len(e.singleMember)) {
			if bucket := selected.bucket(e.singleMember[row]); bucket != 0 {
				emit(bucket)
			}
		}
	}
}

// errorTotal is the explorer's error_total field. The error breakdown uses the
// union of affected requests represented by its cards, the same unit as card N
// and share; every other dimension keeps the canonical Record.IsError count.
// Keeping the units identical is what guarantees an error card's share is at
// most 100% without a client-side clamp hiding a denominator mismatch.
func (e *explorerFold) errorTotal() int64 {
	if e.dimIndex == dimError {
		return e.errAffected
	}
	return e.errTot
}

func (e *explorerFold) payload() explorerPayload {
	// Every lineage read below happens before the pooled resolution workspace
	// is returned; a later payload build on this view re-resolves.
	defer e.lineage.releaseScratch()
	conversations := e.lineage.summary()
	top := e.rankedGroups()
	// Resolve compact selected-group membership once, then both metrics read
	// the same row spans. Emitting every occurrence preserves duplicate tools
	// and error events while error-card N remains affected-request based.
	selected := newExplorerSelectedGroups(top)
	ttftCounts, tpsCounts := make([]int, len(top)), make([]int, len(top))
	var extraTTFT []int64
	var extraTPS []float64
	var ttftBuckets, tpsBuckets []uint8
	for i, g := range top {
		bucket := uint8(i + 1)
		ttftCounts[i], tpsCounts[i] = g.ttftN, g.tpsN
		extraTTFT = append(extraTTFT, g.ttftS...)
		extraTPS = append(extraTPS, g.tpsS...)
		for range g.ttftS {
			ttftBuckets = append(ttftBuckets, bucket)
		}
		for range g.tpsS {
			tpsBuckets = append(tpsBuckets, bucket)
		}
	}
	visit := e.selectedVisitor(&selected)
	wait, _ := rankPercentilesVisitMerge(e.order.base.ttft, e.order.pending.ttft, ttftCounts, extraTTFT, ttftBuckets, visit)
	speed, _ := rankPercentilesVisitMerge(e.order.base.tps, e.order.pending.tps, tpsCounts, extraTPS, tpsBuckets, visit)
	ents := make([]jsonEnt, 0, len(top))
	for i, g := range top {
		synthetic := g == &e.other
		ent := jsonEnt{
			Name: g.name, N: int64(e.groupCount(g)), Cost: g.cost, In: g.in, Out: g.out,
			Cache: g.cacheR, ErrFinal: int64(g.errFin),
			RateLimitRequests: int64(g.rateLimitRequests),
			Tools:             g.tools, Code: g.code, LastMs: g.lastMs,
			Synthetic: synthetic,
		}
		if e.dimIndex == dimConversation {
			// Resolve lineage for the selected cards only. Materializing every
			// in-scope conversation group allocated a map proportional to the
			// distinct-value count just to look up at most xpNodeCap names.
			if !synthetic {
				ent.Conversation = e.lineage.info(e.lineage.group(g.name))
			}
		}
		ent.CostPerMTok = metrics.ScaledRatio(g.cost, g.costInOut, 1e6)
		if e.dimIndex == dimError {
			ent.ErrEvents = int64(g.n) // fold counted failure EVENTS per group
		}
		ent.TTFTP50, ent.TTFTP95, ent.TPSP50, ent.TPSP95 =
			wait[i][0], wait[i][1], speed[i][0], speed[i][1]
		ents = append(ents, ent)
	}
	rail := make(map[string]int, dimCount)
	for dim, name := range dimensionNames {
		rail[name] = e.railCount[dim]
	}
	return explorerPayload{Dim: e.dim, Total: e.total, ErrorTot: e.errorTotal(), Rail: rail, Groups: ents, Scope: e.scope, Conversations: conversations}
}

// HandleAggExplorer serves the per-dimension breakdown over ALL history. The
// optional f=dim:id filters (everything EXCEPT the active dimension's own
// value - the cross-filter rule: the gallery shows X's distribution over
// records filtered by all OTHER dimensions) scope both the breakdown and its
// share denominator; the rail distinct-value counts and footer scope totals
// follow the FULL filter set. Every count is
// since-inception - computed server-side over the durable history.
func (a *AggAPI) HandleAggExplorer(w http.ResponseWriter, r *http.Request) {
	if !metrics.RejectUnlessGet(w, r) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "dim", "s"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	dim := q.Get("dim")
	if !validDims[dim] {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid dim")
		return
	}
	allFS, statusCode, ok := parseScope(q)
	if !ok {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid filter")
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	payload, err := a.explorer(ctx, dim, allFS, statusCode)
	if err != nil {
		log.Printf("agg explorer: %v", err)
		adminjson.WriteError(w, http.StatusInternalServerError, "aggregate failed")
		return
	}
	writeJSON(w, payload)
}

// dimValue derives one record's value for a single-valued dimension and
// whether to count it (Go mirror of recordMatchesDim; empty keys never
// group, client parity).
func dimValue(dim string, c *contrib) (string, bool) {
	if field := dimField(dim, c); field != nil {
		return *field, *field != ""
	}
	switch dim {
	case "status":
		return contribStatusClass(c), true
	case "time":
		return metrics.TimeBucket(time.UnixMilli(c.start)), true
	}
	return "", false
}

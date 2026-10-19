package web

// Lineage is display metadata, not request routing or descendant filtering.
// The immutable projection owns the resolved topology: distinct conversation
// identities, direct parent declarations, cycle/conflict issues and public-ID
// groups. Explorer requests only mark rail/gallery flags on that topology and
// overlay ring/pending declarations that the projection has not materialized
// yet. Nodes retain interned dictionary slots, never records or content, and
// the per-request view retains only bounded bitsets and overlays.

const (
	conversationMain    = "main"
	conversationSub     = "sub"
	conversationUnknown = "unknown"
)

const (
	conversationIssueNone uint8 = iota
	conversationIssueCycle
	conversationIssueSelfParent
	conversationIssueConflictingParents
)

func conversationIssueName(issue uint8) string {
	switch issue {
	case conversationIssueCycle:
		return "cycle"
	case conversationIssueSelfParent:
		return "self_parent"
	case conversationIssueConflictingParents:
		return "conflicting_parents"
	}
	return ""
}

type conversationSummary struct {
	Main       int `json:"main"`
	Sub        int `json:"sub"`
	Unresolved int `json:"unresolved"`
}

type conversationPivotFilter struct {
	Dim string `json:"dim"`
	ID  string `json:"id"`
}

type conversationInfo struct {
	Role           string                    `json:"role"`
	ParentID       string                    `json:"parent_id,omitempty"`
	ParentObserved *bool                     `json:"parent_observed,omitempty"`
	ParentInScope  *bool                     `json:"parent_in_scope,omitempty"`
	ParentScope    []conversationPivotFilter `json:"parent_scope,omitempty"`
	Issue          string                    `json:"issue,omitempty"`
}

// Session IDs retain their public spelling, but a declaration cannot attach
// one client/credential partition to another partition's matching label.
// Slots are dictionary positions: client/key/conversation for an identity and
// a conversation slot for a declared parent.
type conversationKey struct{ client, key, id uint32 }

type conversationNode struct {
	identity conversationKey
	parent   uint32
	group    uint32
	conflict bool
	issue    uint8
}

type conversationGroupNode struct {
	conv  uint32
	node  uint32 // global node index (projection nodes first, extras after)
	mixed bool
}

// conversationTopology is the projection-owned graph. It is immutable after a
// projection publish; only the projection writer mutates it during append and
// then resolves issues before the next publish.
type conversationTopology struct {
	nodes      []conversationNode
	index      map[conversationKey]uint32
	rowNodes   []uint32 // one entry per projection row: node index+1, 0 when absent
	groups     []conversationGroupNode
	groupIndex map[uint32]uint32

	// Resolution scratch is bounded by the projection and reused by appends.
	parent   []int32
	conflict []bool
	issues   []uint8
	state    []uint8
	path     []uint32
}

func buildConversationTopology(rows []contrib) conversationTopology {
	var t conversationTopology
	t.appendRows(rows)
	t.resolve()
	return t
}

// appendRows records every row's conversation identity and parent declaration.
// rowNodes stays one-to-one with the projection's rows so request folds can
// address a node by row index without a string/dictionary lookup.
func (t *conversationTopology) appendRows(rows []contrib) bool {
	dirty := false
	for i := range rows {
		c := &rows[i]
		conv := c.dimensionIDs[dimConversation]
		if conv == 0 {
			t.rowNodes = append(t.rowNodes, 0)
			continue
		}
		key := conversationKey{
			client: c.dimensionIDs[dimClient],
			key:    c.dimensionIDs[dimKey],
			id:     conv,
		}
		node, changed := t.observe(key, c.parentConvID)
		t.rowNodes = append(t.rowNodes, node+1)
		dirty = dirty || changed
	}
	return dirty
}

func (t *conversationTopology) observe(identity conversationKey, parent uint32) (uint32, bool) {
	if identity.id == 0 {
		return 0, false
	}
	if t.index == nil {
		t.index = make(map[conversationKey]uint32, len(t.nodes)+1)
	}
	if node, ok := t.index[identity]; ok {
		n := &t.nodes[node]
		if parent == 0 || n.conflict {
			return node, false
		}
		if n.parent == 0 {
			n.parent = parent
			return node, true
		}
		if n.parent != parent {
			n.conflict = true
			return node, true
		}
		return node, false
	}
	node := uint32(len(t.nodes))
	n := conversationNode{identity: identity, parent: parent}
	n.group = t.groupFor(identity.id, node)
	t.nodes = append(t.nodes, n)
	t.index[identity] = node
	return node, parent != 0
}

// groupFor keeps the public conversation ID grouping used by the gallery:
// multiple namespace-qualified identities sharing one spelling form one
// mixed group instead of selecting one namespace arbitrarily.
func (t *conversationTopology) groupFor(conv, node uint32) uint32 {
	if t.groupIndex == nil {
		t.groupIndex = make(map[uint32]uint32, len(t.groups)+1)
	}
	if group, ok := t.groupIndex[conv]; ok {
		if t.groups[group].node != node {
			t.groups[group].mixed = true
		}
		return group
	}
	group := uint32(len(t.groups))
	t.groups = append(t.groups, conversationGroupNode{conv: conv, node: node})
	t.groupIndex[conv] = group
	return group
}

// resolve validates every chain in the immutable projection. Cycles,
// self-edges, and conflicting parents invalidate dependent chains too; no
// arbitrary edge removal can fabricate a root.
func (t *conversationTopology) resolve() {
	count := len(t.nodes)
	for i := range t.nodes {
		t.nodes[i].issue = conversationIssueNone
	}
	if cap(t.parent) < count {
		t.parent = make([]int32, count)
	} else {
		t.parent = t.parent[:count]
	}
	if cap(t.conflict) < count {
		t.conflict = make([]bool, count)
	} else {
		t.conflict = t.conflict[:count]
		clear(t.conflict)
	}
	for i := range t.nodes {
		n := &t.nodes[i]
		t.parent[i] = -1
		t.conflict[i] = n.conflict
		if n.parent == 0 {
			continue
		}
		parent := conversationKey{client: n.identity.client, key: n.identity.key, id: n.parent}
		if node, ok := t.index[parent]; ok {
			t.parent[i] = int32(node)
		}
	}
	if cap(t.issues) < count {
		t.issues = make([]uint8, count)
	} else {
		t.issues = t.issues[:count]
	}
	if cap(t.state) < count {
		t.state = make([]uint8, count)
	} else {
		t.state = t.state[:count]
	}
	t.path = t.path[:0]
	resolveConversationIssues(t.parent, t.conflict, t.issues, t.state, t.path)
	for i := range t.nodes {
		t.nodes[i].issue = t.issues[i]
	}
}

// resolveConversationIssues is the one iterative resolver shared by the
// projection topology and a request's pending overlay. parent is -1 for an
// unobserved parent; conflict and issue slices are indexed by node.
func resolveConversationIssues(parent []int32, conflict []bool, issue, state []uint8, path []uint32) {
	clear(issue)
	clear(state)
	for start := range parent {
		if state[start] == 2 {
			continue
		}
		path = path[:0]
		var found uint8
		for node := int32(start); node >= 0; {
			if state[node] == 2 {
				found = issue[node]
				break
			}
			if state[node] == 1 {
				found = conversationIssueCycle
				break
			}
			state[node] = 1
			path = append(path, uint32(node))
			if conflict[node] {
				found = conversationIssueConflictingParents
				break
			}
			if parent[node] == node {
				found = conversationIssueSelfParent
				break
			}
			if parent[node] < 0 {
				break
			}
			node = parent[node]
		}
		for _, node := range path {
			state[node] = 2
			issue[node] = found
		}
	}
}

// conversationGroup is one public conversation ID selected into the payload.
type conversationGroup struct {
	lineage *conversationLineage
	index   uint32
}

// conversationLineage is one request's view over the projection topology.
// canonical bitsets carry rail/gallery membership; extras and overrides carry
// ring/pending declarations without mutating the shared topology.
type conversationLineage struct {
	base  *conversationTopology
	names *queryDimensions

	railNodes  []uint64
	railGroups []uint64
	groupFlags []uint64

	extras          []conversationNode
	extraIndex      map[conversationKey]uint32
	extraGroups     []conversationGroupNode
	extraGroupIndex map[uint32]uint32
	mixedGroups     []uint64

	overrides map[uint32]conversationNode // base node index -> effective declaration
	dirty     bool
	issues    []uint8
	state     []uint8
	path      []uint32

	// testPayload is nil in production. Explorer requests copy a package test
	// hook here so tests can sequence an append against the payload's first
	// lineage read without replacing the real fold or append paths.
	testPayload func()
}

func (l *conversationLineage) prepare(p *projectionData, q *queryDimensions) {
	l.names = q
	if p != nil {
		l.base = &p.lineage
	}
}

func (l *conversationLineage) baseNodes() uint32 {
	if l.base == nil {
		return 0
	}
	return uint32(len(l.base.nodes))
}

func (l *conversationLineage) baseGroups() uint32 {
	if l.base == nil {
		return 0
	}
	return uint32(len(l.base.groups))
}

func (l *conversationLineage) groupCount() uint32 {
	return l.baseGroups() + uint32(len(l.extraGroups))
}

// observe is the standalone entry point used by lineage unit tests: it interns
// names into the same query dictionary owner the explorer uses.
func (l *conversationLineage) observe(c *contrib, inRail, inGroup bool) {
	if c.conv == "" {
		return
	}
	if l.names == nil {
		l.names = &queryDimensions{}
	}
	ids := [dimTool]uint32{
		dimClient:       l.names.intern(dimClient, c.client),
		dimKey:          l.names.intern(dimKey, c.key),
		dimConversation: l.names.intern(dimConversation, c.conv),
	}
	parent := uint32(0)
	if c.parentConv != "" {
		parent = l.names.intern(dimConversation, c.parentConv)
	}
	l.observeIDs(ids, parent, inRail, inGroup)
}

// observeRow records one contribution. Projected rows address their node
// through the topology's per-row table (no map lookup); ring/pending extras
// resolve by interned identity once.
func (l *conversationLineage) observeRow(c *contrib, ids [dimTool]uint32, parent uint32, inRail, inGroup bool) {
	if c.rowIndex > 0 && l.base != nil && int(c.rowIndex) <= len(l.base.rowNodes) {
		if node := l.base.rowNodes[c.rowIndex-1]; node != 0 {
			index := node - 1
			l.markNode(index, l.base.nodes[index].group, inRail, inGroup)
			l.declareBase(index, parent)
			return
		}
	}
	l.observeIDs(ids, parent, inRail, inGroup)
}

// observeIDs resolves an unprojected (or manually built) contribution by its
// interned identity.
func (l *conversationLineage) observeIDs(ids [dimTool]uint32, parent uint32, inRail, inGroup bool) {
	conv := ids[dimConversation]
	if conv == 0 {
		return
	}
	key := conversationKey{client: ids[dimClient], key: ids[dimKey], id: conv}
	if l.base != nil {
		if node, ok := l.base.index[key]; ok {
			l.markNode(node, l.base.nodes[node].group, inRail, inGroup)
			l.declareBase(node, parent)
			return
		}
	}
	if l.extraIndex != nil {
		if local, ok := l.extraIndex[key]; ok {
			l.markExtra(local, inRail, inGroup)
			l.declareExtra(local, parent)
			return
		}
	}
	l.addExtra(key, parent, inRail, inGroup)
}

func (l *conversationLineage) markNode(node, group uint32, inRail, inGroup bool) {
	if inRail {
		markConversationBit(&l.railNodes, node)
		markConversationBit(&l.railGroups, group)
	}
	if inGroup {
		markConversationBit(&l.groupFlags, group)
	}
}

func markConversationBit(words *[]uint64, index uint32) {
	word := int(index / 64)
	for len(*words) <= word {
		*words = append(*words, 0)
	}
	(*words)[word] |= uint64(1) << (index % 64)
}

func conversationBit(words []uint64, index uint32) bool {
	word := int(index / 64)
	return word < len(words) && words[word]&(uint64(1)<<(index%64)) != 0
}

func (l *conversationLineage) addExtra(key conversationKey, parent uint32, inRail, inGroup bool) {
	local := uint32(len(l.extras))
	global := l.baseNodes() + local
	group := l.groupForExtra(key.id, global)
	l.extras = append(l.extras, conversationNode{identity: key, parent: parent, group: group})
	if l.extraIndex == nil {
		l.extraIndex = make(map[conversationKey]uint32)
	}
	l.extraIndex[key] = local
	if parent != 0 {
		l.dirty = true
	}
	l.markNode(global, group, inRail, inGroup)
}

func (l *conversationLineage) markExtra(local uint32, inRail, inGroup bool) {
	n := &l.extras[local]
	l.markNode(l.baseNodes()+local, n.group, inRail, inGroup)
}

func (l *conversationLineage) groupForExtra(conv, node uint32) uint32 {
	if l.base != nil {
		if group, ok := l.base.groupIndex[conv]; ok {
			// A different namespace-qualified identity sharing this public ID
			// is a per-request mixed group; the projection stays untouched.
			markConversationBit(&l.mixedGroups, group)
			return group
		}
	}
	if group, ok := l.extraGroupIndex[conv]; ok {
		local := group - l.baseGroups()
		if l.extraGroups[local].node != node {
			l.extraGroups[local].mixed = true
		}
		return group
	}
	group := l.groupCount()
	l.extraGroups = append(l.extraGroups, conversationGroupNode{conv: conv, node: node})
	if l.extraGroupIndex == nil {
		l.extraGroupIndex = make(map[uint32]uint32)
	}
	l.extraGroupIndex[conv] = group
	return group
}

func (l *conversationLineage) declareBase(node uint32, parent uint32) {
	if parent == 0 || l.base == nil {
		return
	}
	if l.base.nodes[node].conflict {
		return // already unresolved; no declaration can change the role
	}
	decl, ok := l.overrides[node]
	if !ok {
		baseParent := l.base.nodes[node].parent
		if baseParent == parent {
			return
		}
		if l.overrides == nil {
			l.overrides = make(map[uint32]conversationNode)
		}
		decl = conversationNode{parent: baseParent}
		if decl.parent == 0 {
			decl.parent = parent
		} else {
			decl.conflict = true
		}
		l.overrides[node] = decl
		l.dirty = true
		return
	}
	if decl.parent == parent {
		return
	}
	if decl.parent == 0 {
		decl.parent = parent
	} else {
		decl.conflict = true
	}
	l.overrides[node] = decl
	l.dirty = true
}

func (l *conversationLineage) declareExtra(local uint32, parent uint32) {
	if parent == 0 {
		return
	}
	n := &l.extras[local]
	if n.conflict {
		return
	}
	if n.parent == 0 {
		n.parent = parent
		l.dirty = true
		return
	}
	if n.parent != parent {
		n.conflict = true
		l.dirty = true
	}
}

// solve materializes per-request issues only when a ring/pending declaration
// changed the topology. Otherwise the projection's resolved issues answer
// every role lookup.
func (l *conversationLineage) solve() {
	if !l.dirty {
		return
	}
	baseNodes := l.baseNodes()
	total := int(baseNodes) + len(l.extras)
	if cap(l.issues) < total {
		l.issues = make([]uint8, total)
	} else {
		l.issues = l.issues[:total]
	}
	if cap(l.state) < total {
		l.state = make([]uint8, total)
	} else {
		l.state = l.state[:total]
	}
	parent := make([]int32, total)
	conflict := make([]bool, total)
	for i := uint32(0); i < baseNodes; i++ {
		n := &l.base.nodes[i]
		pid := n.parent
		conflict[i] = n.conflict
		if decl, ok := l.overrides[i]; ok {
			pid = decl.parent
			conflict[i] = conflict[i] || decl.conflict
		}
		parent[i] = l.parentIndex(n.identity, pid, baseNodes)
	}
	for i := range l.extras {
		n := &l.extras[i]
		conflict[baseNodes+uint32(i)] = n.conflict
		parent[baseNodes+uint32(i)] = l.parentIndex(n.identity, n.parent, baseNodes)
	}
	l.path = l.path[:0]
	resolveConversationIssues(parent, conflict, l.issues, l.state, l.path)
	l.dirty = false
}

func (l *conversationLineage) parentIndex(identity conversationKey, parent uint32, baseNodes uint32) int32 {
	if parent == 0 {
		return -1
	}
	key := conversationKey{client: identity.client, key: identity.key, id: parent}
	if l.base != nil {
		if node, ok := l.base.index[key]; ok {
			return int32(node)
		}
	}
	if l.extraIndex != nil {
		if local, ok := l.extraIndex[key]; ok {
			return int32(baseNodes + local)
		}
	}
	return -1
}

func (l *conversationLineage) issueAt(node uint32) uint8 {
	if node < l.baseNodes() {
		if l.issues != nil {
			return l.issues[node]
		}
		return l.base.nodes[node].issue
	}
	local := node - l.baseNodes()
	if l.issues != nil && int(node) < len(l.issues) {
		return l.issues[node]
	}
	if int(local) < len(l.extras) {
		return 0 // only resolved declarations set extras dirty/issue
	}
	return conversationIssueNone
}

func (l *conversationLineage) parentAt(node uint32) uint32 {
	if node < l.baseNodes() {
		if decl, ok := l.overrides[node]; ok {
			return decl.parent
		}
		return l.base.nodes[node].parent
	}
	local := node - l.baseNodes()
	if int(local) < len(l.extras) {
		return l.extras[local].parent
	}
	return 0
}

func (l *conversationLineage) identityAt(node uint32) conversationKey {
	if node < l.baseNodes() {
		return l.base.nodes[node].identity
	}
	local := node - l.baseNodes()
	if int(local) < len(l.extras) {
		return l.extras[local].identity
	}
	return conversationKey{}
}

func (l *conversationLineage) groupConv(group uint32) uint32 {
	baseGroups := l.baseGroups()
	if group < baseGroups {
		return l.base.groups[group].conv
	}
	local := group - baseGroups
	if int(local) < len(l.extraGroups) {
		return l.extraGroups[local].conv
	}
	return 0
}

func (l *conversationLineage) groupNode(group uint32) uint32 {
	baseGroups := l.baseGroups()
	if group < baseGroups {
		return l.base.groups[group].node
	}
	local := group - baseGroups
	if int(local) < len(l.extraGroups) {
		return l.extraGroups[local].node
	}
	return 0
}

func (l *conversationLineage) groupMixed(group uint32) bool {
	baseGroups := l.baseGroups()
	if group < baseGroups {
		return l.base.groups[group].mixed || conversationBit(l.mixedGroups, group)
	}
	local := group - baseGroups
	return int(local) < len(l.extraGroups) && l.extraGroups[local].mixed
}

func (l *conversationLineage) groupRole(group uint32) string {
	if l.groupMixed(group) {
		return conversationUnknown
	}
	node := l.groupNode(group)
	if l.issueAt(node) != conversationIssueNone {
		return conversationUnknown
	}
	if l.parentAt(node) != 0 {
		return conversationSub
	}
	return conversationMain
}

func (l *conversationLineage) summary() conversationSummary {
	if l.testPayload != nil {
		l.testPayload()
	}
	l.solve()
	var summary conversationSummary
	for group := uint32(0); group < l.groupCount(); group++ {
		if !conversationBit(l.railGroups, group) {
			continue
		}
		switch l.groupRole(group) {
		case conversationMain:
			summary.Main++
		case conversationSub:
			summary.Sub++
		default:
			summary.Unresolved++
		}
	}
	return summary
}

// groups materializes the conversation gallery map only when the conversation
// dimension is the active breakdown. Other dimensions never read it.
func (l *conversationLineage) groups() map[string]conversationGroup {
	l.solve()
	var groups map[string]conversationGroup
	for group := uint32(0); group < l.groupCount(); group++ {
		if !conversationBit(l.groupFlags, group) {
			continue
		}
		if l.names == nil {
			continue
		}
		if groups == nil {
			groups = make(map[string]conversationGroup)
		}
		name := l.names.name(dimConversation, l.groupConv(group))
		groups[name] = conversationGroup{lineage: l, index: group}
	}
	return groups
}

// group resolves one selected card's public conversation name to its in-scope
// group without materializing every group in the request. Explorer payloads
// therefore allocate metadata only for their bounded top cards.
func (l *conversationLineage) group(name string) conversationGroup {
	l.solve()
	if l.names == nil {
		return conversationGroup{}
	}
	conv := l.conversationID(name)
	if conv == 0 {
		return conversationGroup{}
	}
	var index uint32
	var ok bool
	if l.base != nil {
		index, ok = l.base.groupIndex[conv]
	}
	if !ok {
		index, ok = l.extraGroupIndex[conv]
	}
	if !ok || !conversationBit(l.groupFlags, index) {
		return conversationGroup{}
	}
	return conversationGroup{lineage: l, index: index}
}

// conversationID is the inverse of queryDimensions.name for this request's
// conversation dictionary, without interning an unknown selected-card name.
func (l *conversationLineage) conversationID(name string) uint32 {
	if l.names == nil || name == "" {
		return 0
	}
	base := l.names.base[dimConversation]
	if id := base.ids[name]; id != 0 {
		return id
	}
	id := l.names.extra[dimConversation].ids[name]
	if id == 0 {
		return 0
	}
	if count := len(base.names); count > 0 {
		id += uint32(count - 1)
	}
	return id
}

// resolve keeps the standalone unit-test contract: it returns the rail summary
// and the gallery groups for the declarations observed so far.
func (l *conversationLineage) resolve() (conversationSummary, map[string]conversationGroup) {
	return l.summary(), l.groups()
}

func (l *conversationLineage) info(g conversationGroup) *conversationInfo {
	if g.lineage == nil {
		return nil
	}
	l.solve()
	info := &conversationInfo{Role: l.groupRole(g.index)}
	if info.Role == conversationUnknown {
		if l.groupMixed(g.index) {
			info.Issue = "multiple_scopes"
			return info
		}
		info.Issue = conversationIssueName(l.issueAt(l.groupNode(g.index)))
		return info
	}
	node := l.groupNode(g.index)
	parent := l.parentAt(node)
	if parent == 0 {
		return info
	}
	identity := l.identityAt(node)
	info.ParentID = l.names.name(dimConversation, parent)
	parentNode := l.parentIndex(identity, parent, l.baseNodes())
	observed := parentNode >= 0
	inScope := observed && conversationBit(l.railNodes, uint32(parentNode))
	info.ParentObserved, info.ParentInScope = &observed, &inScope
	// Empty IDs cannot be represented by the existing hash router. Keep the
	// parent label, but withhold the pivot rather than broadening its scope.
	if identity.client != 0 && identity.key != 0 {
		info.ParentScope = []conversationPivotFilter{
			{Dim: dimensionNames[dimClient], ID: l.names.name(dimClient, identity.client)},
			{Dim: dimensionNames[dimKey], ID: l.names.name(dimKey, identity.key)},
			{Dim: dimensionNames[dimConversation], ID: info.ParentID},
		}
	}
	return info
}

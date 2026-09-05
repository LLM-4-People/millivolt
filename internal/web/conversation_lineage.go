package web

// Lineage is display metadata, not request routing or descendant filtering.
// The existing explorer scan feeds this owner before filtering, so ancestors
// outside the view remain visible without another history scan. Nodes retain
// only distinct scoped identities and direct declarations, never records or
// content. Resolution is iterative O(nodes), with O(nodes) worst-case memory.

const (
	conversationMain    = "main"
	conversationSub     = "sub"
	conversationUnknown = "unknown"
)

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
type conversationKey struct{ client, key, id string }

func (k conversationKey) withID(id string) conversationKey {
	k.id = id
	return k
}

type conversationNode struct {
	identity conversationKey
	parent   string
	conflict bool
	inRail   bool
	inGroup  bool
	state    uint8
	issue    string
}

type conversationLineage struct {
	nodes map[conversationKey]*conversationNode
}

func (l *conversationLineage) observe(c *contrib, inRail, inGroup bool) {
	if c.conv == "" {
		return
	}
	if l.nodes == nil {
		l.nodes = make(map[conversationKey]*conversationNode)
	}
	id := conversationKey{client: c.client, key: c.key, id: c.conv}
	n := l.nodes[id]
	if n == nil {
		n = &conversationNode{identity: id}
		l.nodes[id] = n
	}
	n.inRail = n.inRail || inRail
	n.inGroup = n.inGroup || inGroup
	// Omission is not a contradictory declaration of top-level parentage.
	// Conflicting nonempty declarations never become last-write-wins.
	if c.parentConv != "" {
		if n.parent != "" && n.parent != c.parentConv {
			n.conflict = true
		} else {
			n.parent = c.parentConv
		}
	}
}

type conversationGroup struct {
	node  *conversationNode
	mixed bool
}

func mergeConversationGroup(groups map[string]conversationGroup, n *conversationNode) {
	g := groups[n.identity.id]
	if g.node == nil {
		g.node = n
	} else if g.node != n {
		// Existing galleries group the public ID across namespaces. Keep
		// that contract, but never select one namespace's parent arbitrarily.
		g.mixed = true
	}
	groups[n.identity.id] = g
}

func (g conversationGroup) role() string {
	if g.mixed || g.node.issue != "" {
		return conversationUnknown
	}
	if g.node.parent != "" {
		return conversationSub
	}
	return conversationMain
}

// resolve validates every observed chain, including ancestors excluded by
// scope. A missing ancestor is not an observed main conversation. Cycles,
// self-edges, and conflicting parents invalidate dependent chains too; no
// arbitrary edge removal can fabricate a root. Repeated calls remain safe.
func (l *conversationLineage) resolve() (conversationSummary, map[string]conversationGroup) {
	for _, n := range l.nodes {
		n.state, n.issue = 0, ""
	}
	var path []*conversationNode
	for _, start := range l.nodes {
		if start.state == 2 {
			continue
		}
		path = path[:0]
		issue := ""
		for n := start; n != nil; n = l.nodes[n.identity.withID(n.parent)] {
			if n.state == 2 {
				issue = n.issue
				break
			}
			if n.state == 1 {
				issue = "cycle"
				break
			}
			n.state = 1
			path = append(path, n)
			if n.conflict {
				issue = "conflicting_parents"
				break
			}
			if n.parent == n.identity.id {
				issue = "self_parent"
				break
			}
			if n.parent == "" {
				break
			}
		}
		for _, n := range path {
			n.state, n.issue = 2, issue
		}
	}
	// Counts partition the existing rail's distinct public IDs, not requests.
	// Ambiguous same-ID namespaces count once as unresolved, matching the
	// gallery instead of silently changing its long-standing ID grouping.
	rail := make(map[string]conversationGroup)
	groups := make(map[string]conversationGroup)
	for _, n := range l.nodes {
		if n.inRail {
			mergeConversationGroup(rail, n)
		}
		if n.inGroup {
			mergeConversationGroup(groups, n)
		}
	}
	var summary conversationSummary
	for _, g := range rail {
		switch g.role() {
		case conversationMain:
			summary.Main++
		case conversationSub:
			summary.Sub++
		default:
			summary.Unresolved++
		}
	}
	return summary, groups
}

func (l *conversationLineage) info(g conversationGroup) *conversationInfo {
	if g.node == nil {
		return nil
	}
	info := &conversationInfo{Role: g.role()}
	if g.mixed {
		info.Issue = "multiple_scopes"
		return info
	}
	n := g.node
	if n.issue != "" {
		info.Issue = n.issue
		return info
	}
	if n.parent == "" {
		return info
	}
	info.ParentID = n.parent
	parent := l.nodes[n.identity.withID(n.parent)]
	observed, inScope := parent != nil, parent != nil && parent.inRail
	info.ParentObserved, info.ParentInScope = &observed, &inScope
	// Empty IDs cannot be represented by the existing hash router. Keep the
	// parent label, but withhold the pivot rather than broadening its scope.
	if n.identity.client != "" && n.identity.key != "" {
		info.ParentScope = []conversationPivotFilter{
			{Dim: dimensionNames[dimClient], ID: n.identity.client},
			{Dim: dimensionNames[dimKey], ID: n.identity.key},
			{Dim: dimensionNames[dimConversation], ID: n.parent},
		}
	}
	return info
}

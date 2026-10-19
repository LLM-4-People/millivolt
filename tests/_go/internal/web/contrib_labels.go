package web

// Test helpers for the live label sidecar. Production code never reconstructs
// labels from a projection; these helpers keep fixtures readable and exercise
// the projected-to-live round trip where a benchmark needs raw contributions.

// rowLabels returns a contribution's live sidecar, allocating an empty one
// when a fixture mutates only some labels.
func rowLabels(c *contrib) *contribLabels {
	if c.labels == nil {
		c.labels = &contribLabels{}
	}
	return c.labels
}

// projectedLabels resolves a projected row's labels from its dictionaries,
// for fixtures that rebuild a projection from previously projected rows.
func projectedLabels(p *projectionData, c *contrib) *contribLabels {
	name := func(dim int) string {
		names := p.dimensions.dict[dim].names
		if int(c.dimensionIDs[dim]) < len(names) {
			return names[c.dimensionIDs[dim]]
		}
		return ""
	}
	parent := ""
	if names := p.dimensions.dict[dimConversation].names; int(c.parentConvID) < len(names) {
		parent = names[c.parentConvID]
	}
	return &contribLabels{
		client: name(dimClient), prov: name(dimProvider), model: name(dimModel),
		key: name(dimKey), conv: name(dimConversation), parentConv: parent,
	}
}

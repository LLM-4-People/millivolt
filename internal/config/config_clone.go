package config

// cloneStrMap copies a string map, preserving nil (a nil map stays nil so
// "absent" and "empty" never merge into one value).
func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// overrideBodyEmpty reports whether a body section carries no field to
// rewrite (nil, or both token fields unset). validateRequestOverrides
// normalizes such a body to nil and yamlwrite renders it as "body: {}".
func overrideBodyEmpty(b *OverrideBody) bool {
	return b == nil || (b.MaxTokens == nil && b.MaxCompletionTokens == nil)
}

// cloneRequestOverrides deep-copies the rule list with all nested
// collections and the body pointers, preserving nil (a nil slice or nil
// body stays nil so "absent" and "empty" never merge into one value).
// Clone and the Map export share this one copy owner, so a snapshot or an
// exported payload can never alias a live config.
func cloneRequestOverrides(rs []RequestOverride) []RequestOverride {
	if rs == nil {
		return nil
	}
	out := make([]RequestOverride, len(rs))
	for i, r := range rs {
		ro := RequestOverride{
			Client:        r.Client,
			Provider:      r.Provider,
			Model:         r.Model,
			Headers:       cloneStrMap(r.Headers),
			RemoveHeaders: append([]string(nil), r.RemoveHeaders...),
		}
		if r.Body != nil {
			body := &OverrideBody{}
			if r.Body.MaxTokens != nil {
				v := *r.Body.MaxTokens
				body.MaxTokens = &v
			}
			if r.Body.MaxCompletionTokens != nil {
				v := *r.Body.MaxCompletionTokens
				body.MaxCompletionTokens = &v
			}
			ro.Body = body
		}
		out[i] = ro
	}
	return out
}

// cloneSubConversations deep-copies the entry list with the nested params
// slice, preserving nil (a nil slice stays nil so "absent" and "empty"
// never merge into one value). Clone and the Map export share this one
// copy owner, so a snapshot or an exported payload can never alias a live
// config.
func cloneSubConversations(ss []SubConversation) []SubConversation {
	if ss == nil {
		return nil
	}
	out := make([]SubConversation, len(ss))
	for i, s := range ss {
		out[i] = SubConversation{
			Client: s.Client,
			Params: append([]string(nil), s.Params...),
			Strip:  s.Strip,
		}
	}
	return out
}

// Clone returns a deep copy of c, so a reload can swap in a fully independent
// snapshot: the Providers map, its nested slices/maps, and the AllowedBaseURLs
// slice are all copied, never shared with a prior snapshot. The proxy treats
// each snapshot as immutable, so a live snapshot is never mutated by a reload.
func (c *Config) Clone() *Config {
	out := *c // value copy of all scalar fields
	if c.AllowedBaseURLs != nil {
		out.AllowedBaseURLs = append([]string(nil), c.AllowedBaseURLs...)
	}
	if c.StormStatusCodes != nil {
		out.StormStatusCodes = append([]string{}, c.StormStatusCodes...)
	}
	if c.RetryableErrorClasses != nil {
		out.RetryableErrorClasses = append([]string{}, c.RetryableErrorClasses...)
	}
	if c.Providers != nil {
		out.Providers = make(map[string]ProviderOverride, len(c.Providers))
		for k, v := range c.Providers {
			out.Providers[k] = ProviderOverride{
				CostKeys:    append([]string(nil), v.CostKeys...),
				UsageKeys:   cloneStrMap(v.UsageKeys),
				ModelsPath:  v.ModelsPath,
				ModelsKeys:  cloneStrMap(v.ModelsKeys),
				EnsureTools: append([]string(nil), v.EnsureTools...),
				Headers:     cloneStrMap(v.Headers),
			}
		}
	}
	if c.ProviderAliases != nil {
		out.ProviderAliases = make(map[string]string, len(c.ProviderAliases))
		for k, v := range c.ProviderAliases {
			out.ProviderAliases[k] = v
		}
	}
	if c.ModelRules != nil {
		out.ModelRules = make([]ModelRule, len(c.ModelRules))
		copy(out.ModelRules, c.ModelRules)
	}
	out.RequestOverrides = cloneRequestOverrides(c.RequestOverrides)
	out.SubConversations = cloneSubConversations(c.SubConversations)
	return &out
}

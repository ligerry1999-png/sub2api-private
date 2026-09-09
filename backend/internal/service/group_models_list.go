package service

// GroupModelsListConfig is retained as a source-compatibility alias for
// private migration/test code. Runtime group state uses ModelAllowlist.
type GroupModelsListConfig = GroupModelAllowlist

func (g *Group) CustomModelsListEnabled() bool {
	return g != nil && g.ModelAllowlist.Enabled && len(g.ModelAllowlist.Models) > 0
}

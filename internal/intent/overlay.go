package intent

// OverlayReport names what a draft did to the declaration it was laid over, so
// lint can say which of the draft's entries are new and which stand in for one
// the repository already declares.
type OverlayReport struct {
	AddedComponents    []string
	ReplacedComponents []string
	AddedRules         []string
	ReplacedRules      []string
	// Replaced lists the whole sections the draft declared and so took over:
	// layers, consumes, serves, dependencies, service, use_recipe.
	Replaced []string
}

// Overlay lays a draft over the declaration the repository already has, so a
// proposal is checked against the law it would join rather than in place of it.
//
// Components and rules merge by name and id: a draft entry with the name or id of
// an existing one replaces it, any other is added. The other sections are one
// decision each, not lists of independent entries (a layer order is the order),
// so a draft that declares one replaces the repository's, and a draft that does
// not keeps it. Neither argument is modified.
func Overlay(base, draft *Declaration) (*Declaration, OverlayReport) {
	var rep OverlayReport
	if draft == nil {
		return base, rep
	}
	out := Declaration{Source: draft.Source}
	if base != nil {
		out = *base
		out.Components = append([]ConstraintComponent(nil), base.Components...)
		out.Rules = append([]ConstraintRule(nil), base.Rules...)
	}
	if draft.Service != (Service{}) {
		out.Service = draft.Service
		rep.Replaced = append(rep.Replaced, "service")
	}
	if len(draft.Layers) > 0 {
		out.Layers = draft.Layers
		rep.Replaced = append(rep.Replaced, "layers")
	}
	if len(draft.Consumes) > 0 {
		out.Consumes = draft.Consumes
		rep.Replaced = append(rep.Replaced, "consumes")
	}
	if len(draft.Serves) > 0 {
		out.Serves = draft.Serves
		rep.Replaced = append(rep.Replaced, "serves")
	}
	if len(draft.Dependencies) > 0 {
		out.Dependencies = draft.Dependencies
		rep.Replaced = append(rep.Replaced, "dependencies")
	}
	if !draft.UseRecipe.IsZero() {
		out.UseRecipe = draft.UseRecipe
		rep.Replaced = append(rep.Replaced, "use_recipe")
	}
	for _, c := range draft.Components {
		if i := componentIndex(out.Components, c.Name); i >= 0 {
			out.Components[i] = c
			rep.ReplacedComponents = append(rep.ReplacedComponents, c.Name)
			continue
		}
		out.Components = append(out.Components, c)
		rep.AddedComponents = append(rep.AddedComponents, c.Name)
	}
	for _, r := range draft.Rules {
		if i := ruleIndex(out.Rules, r.ID); i >= 0 {
			out.Rules[i] = r
			rep.ReplacedRules = append(rep.ReplacedRules, r.ID)
			continue
		}
		out.Rules = append(out.Rules, r)
		rep.AddedRules = append(rep.AddedRules, r.ID)
	}
	return &out, rep
}

func componentIndex(list []ConstraintComponent, name string) int {
	for i := range list {
		if name != "" && list[i].Name == name {
			return i
		}
	}
	return -1
}

func ruleIndex(list []ConstraintRule, id string) int {
	for i := range list {
		if id != "" && list[i].ID == id {
			return i
		}
	}
	return -1
}

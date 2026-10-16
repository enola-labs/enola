package tsextractor

import (
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/litfold"
)

// emberEnrich applies Ember classification to the already-extracted declaration
// facts of one file. Each template segment attaches to the declaration that owns
// it — the class whose body embeds it (which is thereby a component, whatever
// its superclass), or the `const Name = <template>…` binding it initializes —
// and a segment owned by neither is the file's standalone default component,
// synthesized the way a script-less Vue SFC is. Service classes, @service
// injections and ember-data models gain their props and companion facts.
func emberEnrich(kinds *tsutil.KindTable, result []facts.Fact, root *sitter.Node, src []byte, relFile string,
	aliases map[string]tsAlias, segments []emberTemplateSegment) []facts.Fact {

	en := &emberEnricher{
		kinds:    kinds,
		relFile:  relFile,
		dir:      factpath.Dir(relFile),
		base:     strings.TrimSuffix(filepath.Base(relFile), filepath.Ext(relFile)),
		segments: segments,
		bindings: buildEmberImportBindings(kinds, root, src, relFile, aliases),
		folds:    emberBuildFoldMap(kinds, root, src),
		claimed:  make(map[int]bool),
	}
	classes := collectEmberClasses(kinds, root, src, en.bindings, en.folds)
	templateBindings := collectEmberTemplateBindings(kinds, root, src)
	en.addLocalBindings(classes, templateBindings)

	en.markDefaultExport(result, emberDefaultExportName(kinds, root, src))
	if emberFrameworkRegisteredFile(relFile) {
		markFrameworkRegistered(result)
	}
	for ci := range classes {
		result = en.enrichClass(result, &classes[ci])
	}
	for _, tb := range templateBindings {
		en.enrichTemplateBinding(result, tb)
	}
	if isEmberTemplateTagFile(relFile) {
		result = en.standaloneComponent(result)
	}
	return result
}

type emberEnricher struct {
	kinds    *tsutil.KindTable
	relFile  string
	dir      string
	base     string
	segments []emberTemplateSegment
	bindings emberImportBindings
	folds    *litfold.Assignments
	claimed  map[int]bool
}

// A template may also render a component declared in its own file (a named
// sibling binding or class) — those locals are statically known, so they join
// the resolution set. Imports win on a name collision.
func (en *emberEnricher) addLocalBindings(classes []emberClassInfo, templateBindings []emberBindingInfo) {
	for _, cls := range classes {
		if cls.name != "" {
			if _, taken := en.bindings.internal[cls.name]; !taken {
				en.bindings.internal[cls.name] = en.dir + "." + cls.name
			}
		}
	}
	for _, tb := range templateBindings {
		if _, taken := en.bindings.internal[tb.name]; !taken {
			en.bindings.internal[tb.name] = en.dir + "." + tb.name
		}
	}
}

func (en *emberEnricher) markDefaultExport(result []facts.Fact, defaultName string) {
	if defaultName == "" {
		return
	}
	factName := en.dir + "." + defaultName
	for i := range result {
		if result[i].Kind == facts.KindSymbol && result[i].Name == factName {
			result[i].SetProp(EmberDefaultExportProp, true)
			return
		}
	}
}

func markFrameworkRegistered(result []facts.Fact) {
	for i := range result {
		if result[i].Kind == facts.KindSymbol && result[i].PropAny("receiver") == nil {
			result[i].SetProp("framework_registered", true)
		}
	}
}

func (en *emberEnricher) refsFor(owns func(emberTemplateSegment) bool) []string {
	var owned []emberTemplateSegment
	for _, seg := range en.segments {
		if owns(seg) {
			owned = append(owned, seg)
		}
	}
	return emberTemplateRefs(owned, en.bindings)
}

func (en *emberEnricher) segmentText(start, end int) string {
	var b strings.Builder
	for _, seg := range en.segments {
		if seg.Start >= start && seg.Start < end {
			b.WriteString(seg.Content)
			b.WriteByte(0)
		}
	}
	return b.String()
}

func (en *emberEnricher) resolveYieldHash(yh []string) []string {
	resolved := make([]string, 0, len(yh))
	for _, e := range yh {
		if k, v, ok := strings.Cut(e, "=?"); ok {
			if target, bound := en.bindings.internal[v]; bound {
				resolved = append(resolved, k+"=@"+target)
			}
			continue
		}
		resolved = append(resolved, e)
	}
	sort.Strings(resolved)
	return resolved
}

func (en *emberEnricher) applySegmentProps(f *facts.Fact, start, end int) {
	text := en.segmentText(start, end)
	if text == "" {
		return
	}
	if typed := scanTypedLiteralInvocations(text, en.folds); len(typed) > 0 {
		f.SetProp(EmberInvocationsProp, mergeSorted(propStringsLocal(f.PropAny(EmberInvocationsProp)), typed))
	}
	if yh := scanEmberYieldHash(text); len(yh) > 0 {
		if resolved := en.resolveYieldHash(yh); len(resolved) > 0 {
			f.SetProp(EmberYieldHashProp, resolved)
		}
	}
	if ctx := scanEmberContextualUses(text); len(ctx) > 0 {
		resolved := make([]string, 0, len(ctx))
		for _, pair := range ctx {
			comp, key, _ := strings.Cut(pair, "#")
			if target, ok := en.bindings.internal[comp]; ok {
				resolved = append(resolved, target+"#"+key)
			} else {
				resolved = append(resolved, pair)
			}
		}
		sort.Strings(resolved)
		f.SetProp(EmberContextualProp, resolved)
	}
	if n, samples := countDynamicInvocations(text, en.folds); n > 0 {
		f.SetProp(EmberDynamicCountProp, n)
		f.SetProp(EmberDynamicSamplesProp, samples)
	}
}

func (en *emberEnricher) linksFor(start, end int) []string {
	seen := make(map[string]bool)
	for _, seg := range en.segments {
		if seg.Start < start || seg.Start >= end {
			continue
		}
		for _, l := range scanEmberRouteLinks(seg.Content) {
			seen[l] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return sortedSet(seen)
}

func attachCalls(f *facts.Fact, targets []string) {
	for _, t := range targets {
		if t != f.Name && !f.HasRelation(facts.RelCalls, t) {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
		}
	}
}

func markComponent(f *facts.Fact) {
	f.SetProp("web_component", "component")
	f.SetProp("framework", EmberFramework)
}

func (en *emberEnricher) enrichClass(result []facts.Fact, cls *emberClassInfo) []facts.Fact {
	if cls.name == "" {
		return result
	}
	hasTemplate := false
	classRefs := en.refsFor(func(seg emberTemplateSegment) bool {
		owns := seg.Start >= cls.start && seg.Start < cls.end
		if owns {
			hasTemplate = true
			en.claimed[seg.Start] = true
		}
		return owns
	})
	factName := en.dir + "." + cls.name
	for i := range result {
		if result[i].Kind != facts.KindSymbol || result[i].Name != factName {
			continue
		}
		en.enrichClassSymbol(&result[i], cls, hasTemplate, classRefs)
		break
	}
	if cls.isModel {
		result = append(result, en.modelStorage(cls, factName))
	}
	return result
}

func (en *emberEnricher) enrichClassSymbol(f *facts.Fact, cls *emberClassInfo, hasTemplate bool, classRefs []string) {
	if cls.isComponent || hasTemplate {
		markComponent(f)
		attachCalls(f, classRefs)
		en.applySegmentProps(f, cls.start, cls.end)
	}

	merged := make(map[string]bool)
	for _, l := range en.linksFor(cls.start, cls.end) {
		merged[l] = true
	}
	for _, l := range cls.codeLinks {
		merged[l] = true
	}
	if len(merged) > 0 {
		f.SetProp(EmberRouteLinksProp, sortedSet(merged))
	}
	if cls.isDefault {
		f.SetProp(EmberDefaultExportProp, true)
	}
	if cls.isService {
		f.SetProp("ember_service", en.base)
	}
	if role := emberDataRoleForFile(en.relFile); role != "" {
		f.SetProp(EmberDataRoleProp, role)
	}
	if len(cls.services) > 0 {
		f.SetProp(EmberServicesProp, cls.services)
	}
}

func (en *emberEnricher) modelStorage(cls *emberClassInfo, factName string) facts.Fact {
	props := map[string]any{
		"storage_kind": "model",
		"framework":    "ember-data",
		"table":        en.base,
		"language":     "typescript",
	}
	if len(cls.relationships) > 0 {
		props[EmberRelationshipsProp] = cls.relationships
	}
	if len(cls.attrTransforms) > 0 {
		props[EmberAttrTransformsProp] = cls.attrTransforms
	}
	return facts.Fact{
		Kind:      facts.KindStorage,
		Name:      factName,
		File:      en.relFile,
		Line:      cls.line,
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: en.dir}},
	}
}

func (en *emberEnricher) enrichTemplateBinding(result []facts.Fact, tb emberBindingInfo) {
	refs := en.refsFor(func(seg emberTemplateSegment) bool {
		owns := seg.Start >= tb.start && seg.Start < tb.end
		if owns {
			en.claimed[seg.Start] = true
		}
		return owns
	})
	if refs == nil && !anySegmentIn(en.segments, tb.start, tb.end) {
		return
	}
	factName := en.dir + "." + tb.name
	for i := range result {
		if result[i].Kind != facts.KindSymbol || result[i].Name != factName {
			continue
		}
		markComponent(&result[i])
		attachCalls(&result[i], refs)
		if links := en.linksFor(tb.start, tb.end); len(links) > 0 {
			result[i].SetProp(EmberRouteLinksProp, links)
		}
		en.applySegmentProps(&result[i], tb.start, tb.end)
		return
	}
}

func (en *emberEnricher) unclaimed() []emberTemplateSegment {
	var out []emberTemplateSegment
	for _, seg := range en.segments {
		if !en.claimed[seg.Start] {
			out = append(out, seg)
		}
	}
	return out
}

func (en *emberEnricher) standaloneComponent(result []facts.Fact) []facts.Fact {
	refs := en.refsFor(func(seg emberTemplateSegment) bool { return !en.claimed[seg.Start] })
	segs := en.unclaimed()
	if len(segs) == 0 {
		return result
	}
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: en.dir}}
	for _, t := range refs {
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	props := map[string]any{
		"symbol_kind":          facts.SymbolFunc,
		"exported":             true,
		"language":             "typescript",
		"web_component":        "component",
		"framework":            EmberFramework,
		EmberDefaultExportProp: true,
	}
	en.standaloneProps(props, segs)
	return append(result, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      en.dir + "." + fileSymbolName(en.relFile),
		File:      en.relFile,
		Line:      1,
		Props:     props,
		Relations: rels,
	})
}

func (en *emberEnricher) standaloneProps(props map[string]any, segs []emberTemplateSegment) {
	var text strings.Builder
	linkSeen := make(map[string]bool)
	for _, seg := range segs {
		text.WriteString(seg.Content)
		text.WriteByte(0)
		for _, l := range scanEmberRouteLinks(seg.Content) {
			linkSeen[l] = true
		}
	}
	if len(linkSeen) > 0 {
		props[EmberRouteLinksProp] = sortedSet(linkSeen)
	}
	body := text.String()
	if typed := scanTypedLiteralInvocations(body, en.folds); len(typed) > 0 {
		props[EmberInvocationsProp] = typed
	}
	if yh := scanEmberYieldHash(body); len(yh) > 0 {
		if resolved := en.resolveYieldHash(yh); len(resolved) > 0 {
			props[EmberYieldHashProp] = resolved
		}
	}
	if ctx := scanEmberContextualUses(body); len(ctx) > 0 {
		props[EmberContextualProp] = ctx
	}
	if n, samples := countDynamicInvocations(body, en.folds); n > 0 {
		props[EmberDynamicCountProp] = n
		props[EmberDynamicSamplesProp] = samples
	}
}

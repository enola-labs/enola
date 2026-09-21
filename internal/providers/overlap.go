package providers

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// overlapEvidenceCap bounds the pairs kept per kind as evidence, the way the
// receipt caps every other sample: enough to see the shape, never the whole
// list.
const overlapEvidenceCap = 10

// CommitSourceProvider and CommitSourceGit say where a provider record's
// commit and dirty flag came from.
const (
	CommitSourceProvider = "provider"
	CommitSourceGit      = "git at merge"
)

// conflictKinds are the relation kinds where the extractor states one
// answer per source and a provider naming another is a contradiction rather
// than an addition: a class has one direct ancestor chain, a constant read
// resolves to one declaration. A call edge has no such exclusivity, so a
// provider call the extractor lacks is provider-only, never a conflict.
var conflictKinds = map[string]bool{
	facts.RelImplements: true,
	facts.RelDependsOn:  true,
}

// Account fills each ran provider's record with its overlap against the
// extractor's facts and the tree its facts describe. merged is every provider
// fact that survived the merge, stamped with its provider's name; extracted
// is the extractor's own account of the same repository. A provider that
// wrote its commit into its census is believed; the rest read git at merge
// time, and the record says which.
func Account(records []facts.ProviderRecord, merged, extracted []facts.Fact, git *facts.GitInfo) {
	byProvider := map[string][]facts.Fact{}
	for _, f := range merged {
		name, _ := f.PropAny(PropProvider).(string)
		byProvider[name] = append(byProvider[name], f)
	}
	for i := range records {
		r := &records[i]
		if r.Skipped {
			continue
		}
		r.Overlap = Overlap(extracted, byProvider[r.Name])
		if r.Commit != "" {
			r.CommitSource = CommitSourceProvider
			continue
		}
		if git != nil && git.Commit != "" {
			r.Commit = git.Commit
			r.Dirty = git.Dirty
			r.CommitSource = CommitSourceGit
		}
	}
}

// Overlap joins one provider's relations against the extractor's by source
// symbol and relation kind. Every provider relation lands in exactly one
// bucket: already_resolved (the extractor has the same edge), respelled (the
// same target under another spelling), conflict (a kind with one answer, and
// the extractor's answer differs), no_extractor_symbol (the extractor never
// declared the source), or provider_only. Counts only; nothing here changes
// which edges the graph holds.
func Overlap(extracted, provider []facts.Fact) map[string]*facts.RelationOverlap {
	index := extractorIndex(extracted)
	out := map[string]*facts.RelationOverlap{}
	for _, f := range provider {
		source := relationSource(f)
		for _, rel := range f.Relations {
			o := out[rel.Kind]
			if o == nil {
				o = &facts.RelationOverlap{}
				out[rel.Kind] = o
			}
			targets, declared := index.targets(source, rel.Kind)
			if !declared {
				o.NoExtractorSymbol++
				continue
			}
			if targets[rel.Target] {
				o.AlreadyResolved++
				continue
			}
			spelled := spelling(rel.Target)
			if extractorTarget, ok := index.bySpelling(source, rel.Kind, spelled); ok {
				o.Respelled++
				if len(o.Respellings) < overlapEvidenceCap {
					o.Respellings = append(o.Respellings, facts.TargetPair{Source: source, Provider: rel.Target, Extractor: extractorTarget})
				}
				continue
			}
			if conflictKinds[rel.Kind] && len(targets) > 0 && !transitive(f) {
				o.Conflict++
				if len(o.Conflicts) < overlapEvidenceCap {
					o.Conflicts = append(o.Conflicts, facts.TargetPair{Source: source, Provider: rel.Target, Extractor: firstTarget(targets)})
				}
				continue
			}
			o.ProviderOnly++
		}
	}
	return out
}

// PropAncestorDistance is the hop count a provider writes on an ancestor
// fact. Only a direct ancestor states something the extractor also states;
// a grandparent or an included module further up the chain is an addition
// the extractor never claimed to hold, so it can only be provider_only.
const PropAncestorDistance = "ancestor_distance"

func transitive(f facts.Fact) bool {
	switch d := f.PropAny(PropAncestorDistance).(type) {
	case int:
		return d > 1
	case float64:
		return d > 1
	}
	return false
}

// relationSource names the symbol a provider fact's relations leave from. A
// provider states an edge as a dependency fact named "<tag>: <source> ->
// <target>" (the shape the Prism and Rubydex producers write), so the source
// is the segment between the tag and the arrow; any other fact's relations
// leave from the fact itself.
func relationSource(f facts.Fact) string {
	if f.Kind != facts.KindDependency {
		return f.Name
	}
	name := f.Name
	if i := strings.Index(name, ": "); i >= 0 {
		name = name[i+2:]
	}
	if i := strings.LastIndex(name, " -> "); i >= 0 {
		name = name[:i]
	}
	return name
}

// extractorRelations is the extractor's relations indexed by source name and
// kind, with the set of targets and a second index by normalised spelling.
type extractorRelations struct {
	declared map[string]bool
	byKind   map[string]map[string]map[string]bool
	spelled  map[string]map[string]map[string]string
}

func extractorIndex(extracted []facts.Fact) *extractorRelations {
	idx := &extractorRelations{
		declared: make(map[string]bool, len(extracted)),
		byKind:   map[string]map[string]map[string]bool{},
		spelled:  map[string]map[string]map[string]string{},
	}
	for _, f := range extracted {
		idx.declared[f.Name] = true
		for _, rel := range f.Relations {
			kinds := idx.byKind[f.Name]
			if kinds == nil {
				kinds = map[string]map[string]bool{}
				idx.byKind[f.Name] = kinds
			}
			targets := kinds[rel.Kind]
			if targets == nil {
				targets = map[string]bool{}
				kinds[rel.Kind] = targets
			}
			targets[rel.Target] = true
			spelledKinds := idx.spelled[f.Name]
			if spelledKinds == nil {
				spelledKinds = map[string]map[string]string{}
				idx.spelled[f.Name] = spelledKinds
			}
			bySpelling := spelledKinds[rel.Kind]
			if bySpelling == nil {
				bySpelling = map[string]string{}
				spelledKinds[rel.Kind] = bySpelling
			}
			if _, seen := bySpelling[spelling(rel.Target)]; !seen {
				bySpelling[spelling(rel.Target)] = rel.Target
			}
		}
	}
	return idx
}

// targets returns the extractor's targets for a source and kind, and whether
// the extractor declared the source at all. A declared source with no
// relations of the kind answers an empty set and true.
func (idx *extractorRelations) targets(source, kind string) (map[string]bool, bool) {
	if !idx.declared[source] {
		return nil, false
	}
	return idx.byKind[source][kind], true
}

func (idx *extractorRelations) bySpelling(source, kind, spelled string) (string, bool) {
	target, ok := idx.spelled[source][kind][spelled]
	return target, ok
}

// spelling reduces a target name to what it names: the singleton scope a
// provider writes as `<Foo>` is the class `Foo`, a leading `::` is the same
// constant, and the separator between an owner and its method carries no
// meaning across producers.
func spelling(target string) string {
	s := strings.ReplaceAll(target, "<", "")
	s = strings.ReplaceAll(s, ">", "")
	s = strings.TrimPrefix(s, "::")
	s = strings.ReplaceAll(s, "::<", "::")
	return strings.ReplaceAll(s, "#", ".")
}

func firstTarget(targets map[string]bool) string {
	names := make([]string, 0, len(targets))
	for t := range targets {
		names = append(names, t)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

package engine

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// unseenCauseCap bounds the skip causes kept per provider, matching the file
// census: the biggest holes first, the rest summed into the provider's own
// receipt block.
const unseenCauseCap = 5

// deadExemptionTitle is how the constraints explainer titles an exemption
// whose witness matched no violation; counted here, never re-derived.
const deadExemptionTitle = "Constraint exemption on "

// outsideGraphKinds are the relation kinds whose unresolved targets say the
// graph did not hold a thing rather than that a name was unreadable: a
// dependency on a gem or an import of a package nobody indexed.
var outsideGraphKinds = []string{facts.RelDependsOn, facts.RelImports}

// unseenCensus assembles the one account of what this run could not see,
// after the providers merged and the explainers ran, from counts the engine
// already holds. Nothing is measured again: the ignore tally comes from the
// walk, the provider causes from their census records, the outside-graph
// counts from the store's relations against its own names, the dead
// exemptions from the explainer's findings, the dynamic-feature classes from
// the prefix list the Ruby extractor already attaches to a file.
func (e *Engine) unseenCensus(skips walkSkips, records []facts.ProviderRecord, insights []facts.Insight) *facts.UnseenCensus {
	u := &facts.UnseenCensus{
		FilesExcludedByIgnore: skips.count,
		DirsExcludedByIgnore:  skips.dirCount,
		OutsideGraph:          map[string]int{},
		OutsideGraphPrefixes:  map[string]int{},
	}
	for _, r := range records {
		skip := facts.ProviderSkip{Name: r.Name}
		if r.Skipped {
			skip.Reason = r.Reason
		} else if r.Census != nil {
			causes := append([]facts.CensusCause(nil), r.Census.SkipCauses...)
			sort.SliceStable(causes, func(i, j int) bool { return causes[i].Count > causes[j].Count })
			if len(causes) > unseenCauseCap {
				causes = causes[:unseenCauseCap]
			}
			skip.Causes = causes
		}
		if skip.Reason != "" || len(skip.Causes) > 0 {
			u.ProviderSkips = append(u.ProviderSkips, skip)
		}
	}

	all := e.store.FactsRef()
	names := make(map[string]bool, len(all))
	dynamicFiles := map[string]bool{}
	for _, f := range all {
		names[f.Name] = true
		if f.Kind == facts.KindFileRef && f.Props["dynamic_send_prefixes"] != nil {
			dynamicFiles[f.File] = true
		}
	}
	for _, f := range all {
		for _, rel := range f.Relations {
			if !isOutsideGraphKind(rel.Kind) || names[rel.Target] {
				continue
			}
			// TypeScript's internal dependency target is often an implicit,
			// extensionless graph node rather than a standalone fact. The extractor
			// has already proven it local; counting it as outside would turn healthy
			// aliased imports into the very coverage warning meant to diagnose broken
			// aliases.
			if f.Kind == facts.KindDependency {
				source, _ := f.Props["source"].(string)
				if source == facts.DepSourceInternal || source == facts.DepSourceFramework {
					continue
				}
			}
			u.OutsideGraph[rel.Kind]++
			if rel.Kind == facts.RelImports {
				if prefix := unresolvedImportPrefix(rel.Target); prefix != "" {
					u.OutsideGraphPrefixes[prefix]++
				}
			}
		}
		if f.Kind == facts.KindSymbol && f.Props["symbol_kind"] == facts.SymbolClass && dynamicFiles[f.File] {
			u.DynamicFeatureClasses++
		}
	}
	for _, in := range insights {
		if strings.HasPrefix(in.Title, deadExemptionTitle) && strings.Contains(in.Title, " matches nothing") {
			u.DeadExemptions++
		}
	}
	return u
}

func unresolvedImportPrefix(target string) string {
	target = strings.Trim(strings.TrimSpace(target), "/")
	if target == "" || strings.HasPrefix(target, ".") {
		return ""
	}
	parts := strings.Split(target, "/")
	if len(parts) < 2 {
		return ""
	}
	if strings.HasPrefix(parts[0], "@") {
		if len(parts) < 3 {
			return ""
		}
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

func isOutsideGraphKind(kind string) bool {
	for _, k := range outsideGraphKinds {
		if k == kind {
			return true
		}
	}
	return false
}

package facts

import "sort"

// CanonicalSymbols collapses conditional-compilation duplicates in a slice of
// symbol facts. A type or member declared once per branch of a #if/#else block is
// emitted as multiple same-name facts, because the Swift and C++ extractors walk
// BOTH branches (the compile-time condition is not evaluated). Every such fact
// carries conditional=true (GAP-SW-10). This returns a slice in which, among the
// facts tagged conditional, only the first per (Name, File) is kept; every
// non-conditional fact passes through untouched, so genuine overloads that share a
// base name — which the extractors also emit as same-name facts — are never
// collapsed. Order is preserved, so callers stay deterministic.
//
// Use it in consumers that COUNT symbols from the flat fact list — exported-surface,
// complexity-outliers, package-metrics — so a #if type is not scored twice. God-class
// and hotspots already key their aggregation by Name and collapse the duplicate
// implicitly, so they do not need it. It must NOT be applied to the raw query/store
// path: both declarations are real and stay individually queryable
// (query_facts(kind=symbol, prop=conditional, prop_value=true)).
func CanonicalSymbols(syms []Fact) []Fact {
	out := make([]Fact, 0, len(syms))
	var seen map[[2]string]bool
	for _, s := range syms {
		if cond, _ := s.PropAny("conditional").(bool); cond {
			key := [2]string{s.Name, s.File}
			if seen == nil {
				seen = make(map[[2]string]bool, len(syms))
			}
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, s)
	}
	return out
}

func sortsBefore(a, b Fact) bool {
	if a.Repo != b.Repo {
		return a.Repo < b.Repo
	}
	if a.File != b.File {
		return a.File < b.File
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Kind < b.Kind
}

// SortByLocation orders fs, in place, by repository, file, line, name and kind: the
// order in which an explainer that keeps the first fact per name keeps the same one
// however the store came to hold them (the members' order, a refresh).
func SortByLocation(fs []Fact) {
	sort.SliceStable(fs, func(i, j int) bool { return sortsBefore(fs[i], fs[j]) })
}

// DistinctByNode keeps one fact per graph node: the one that sorts first by
// repository, file and line. An explainer scoring a node reports it through one of the
// facts declaring it, and which one used to be whichever the store held first, so a
// finding's file followed the members' order and moved on a refresh, which reorders
// the store. A name declared in several repositories is a node in each, and is kept
// once per repository. The result keeps the order of first appearance.
func DistinctByNode(fs []Fact, g *Graph) []Fact {
	best := make(map[string]int, len(fs))
	out := make([]Fact, 0, len(fs))
	for _, f := range fs {
		key := g.KeyOf(&f)
		i, seen := best[key]
		if !seen {
			best[key] = len(out)
			out = append(out, f)
			continue
		}
		if sortsBefore(f, out[i]) {
			out[i] = f
		}
	}
	return out
}

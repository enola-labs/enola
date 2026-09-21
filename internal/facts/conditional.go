package facts

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

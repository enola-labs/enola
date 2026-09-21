package rubyextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// resolveImports derives internal module-coupling edges for Ruby and returns them
// as synthetic dependency facts. Rails autoloads constants, so internal coupling
// is expressed through constant references — class inheritance, include/extend
// mixins, ActiveRecord associations, and method calls — whose relation targets are
// Ruby constant names, not directory paths. None of those relation kinds are
// counted by the coupling consumers (graph, package metrics, explain hotspots),
// which only count dependency facts whose `imports` target matches a module Name.
//
// This pass builds a constant -> declaring-module-dir index, resolves every
// cross-module constant reference to a srcDir -> destDir edge, and emits one
// synthetic dependency fact per unique edge (with an `imports` relation to the
// destination module dir). It also resolves require_relative paths and Packwerk
// package.yml dependencies, and classifies require/require_relative facts as
// internal/stdlib/external in place. This mirrors the Python extractor's resolve
// pass; it never guesses — every edge comes from a real parsed reference.
func resolveImports(allFacts []facts.Fact, isRails bool) []facts.Fact {
	ix := buildConstIndex(allFacts)
	moduleNames := collectModuleNames(allFacts)

	// edges maps a src->dst pair to its coupling kind. When the same edge arises
	// from several references the hardest kind wins (couplingRank), so an edge that
	// is also a real reference/inheritance is never downgraded to "association" —
	// only association-ONLY edges stay tagged association and get excluded from
	// cycle detection.
	edges := map[[2]string]string{}
	add := func(src, dst, kind string) {
		if src == "" || dst == "" || src == dst {
			return // skip empties and self-edges
		}
		key := [2]string{src, dst}
		if cur, ok := edges[key]; !ok || couplingRank(kind) > couplingRank(cur) {
			edges[key] = kind
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		switch f.Kind {
		case facts.KindSymbol:
			src := declaresTarget(f)
			for _, rel := range f.Relations {
				switch rel.Kind {
				case facts.RelImplements: // inheritance
					add(src, ix.resolve(rel.Target, src), facts.CouplingInheritance)
				case facts.RelCalls:
					if c := constFromCall(rel.Target); c != "" {
						add(src, ix.resolve(c, src), facts.CouplingReference)
					}
				}
			}
		case facts.KindDependency:
			src := fileDir(f.File)
			for j := range f.Relations {
				rel := &f.Relations[j]
				switch rel.Kind {
				case facts.RelImplements: // include/extend/prepend mixins
					if dst := ix.resolve(rel.Target, src); dst != "" {
						add(src, dst, facts.CouplingMixin)
						setSource(f, "internal")
					} else {
						setSource(f, "external")
					}
				case facts.RelDependsOn: // ActiveRecord associations
					if dst := ix.resolve(rel.Target, src); dst != "" {
						add(src, dst, facts.CouplingAssociation)
						setSource(f, "internal")
					} else {
						setSource(f, "external")
					}
				case facts.RelImports: // require / require_relative
					classifyRequire(f, rel, src, moduleNames, add)
				}
			}
		case facts.KindModule:
			// Packwerk package.yml dependencies: explicit module -> module edges.
			for _, rel := range f.Relations {
				if rel.Kind == facts.RelDependsOn {
					add(packwerkDir(f.Name), packwerkDir(rel.Target), facts.CouplingPackwerk)
				}
			}
		}
	}

	return emitEdges(edges, isRails)
}

// applyTableNamePrefixes corrects every derived model table with the prefix its
// root namespace declares, mutating the facts in place. Rails puts
// `table_name_prefix` in front of the name it derives from the class, so a model
// under such a namespace stores in a relation the derivation alone never names:
// `Engagement::Survey` is engagement_surveys, while `surveys` is another model's
// table entirely, and a graph that hands out the unprefixed name answers with a
// relation the model never reads. The declaration lives in one file and the
// models it governs in others, which is why the correction is a whole-repo pass
// rather than a per-file one.
//
// A table_source of "declared" is left exactly as the source states it: Rails
// does not prefix a `self.table_name`, so prefixing one would replace a stated
// fact with a derived guess.
//
// The namespaces enclosing a model are searched innermost first and the first
// one that declared a prefix decides, which is what Rails' own
// `module_parents.detect` does and is not the same as taking the outermost:
// `Packages::Debian::Publication` stores in packages_debian_publications, not in
// packages_publications. A namespace whose prefix could not be read stops the
// search rather than handing the model to an outer namespace, and a module
// declaring two different prefixes across files resolves to none — which one
// applies cannot be derived from the name.
func applyTableNamePrefixes(allFacts []facts.Fact) {
	prefixes := map[string]string{}
	for _, f := range allFacts {
		if f.Kind != facts.KindSymbol || f.Props == nil {
			continue
		}
		prefix, _ := f.PropAny("table_name_prefix").(string)
		if prefix == "" {
			continue
		}
		if prior, seen := prefixes[f.Name]; seen && prior != prefix {
			prefixes[f.Name] = ""
			continue
		}
		prefixes[f.Name] = prefix
	}
	if len(prefixes) == 0 {
		return
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindStorage || f.Props == nil {
			continue
		}
		if kind, _ := f.PropAny("storage_kind").(string); kind != "model" {
			continue
		}
		if source, _ := f.PropAny("table_source").(string); source != "derived" {
			continue
		}
		table, _ := f.PropAny("table").(string)
		if table == "" {
			continue
		}
		segments := strings.Split(f.Name, "::")
		for i := len(segments) - 1; i > 0; i-- {
			prefix, declared := prefixes[strings.Join(segments[:i], "::")]
			if !declared {
				continue
			}
			if prefix != "" {
				f.SetProp("table", prefix+table)
			}
			break
		}
	}
}

// couplingRank orders coupling kinds so the "hardest" (most import-like) wins when
// one edge arises from multiple references. Association is weakest: any other kind
// overrides it, so an edge is tagged "association" only when associations are its
// sole source — which is exactly the set the cycles explainer excludes.
func couplingRank(kind string) int {
	switch kind {
	case facts.CouplingAssociation:
		return 0
	case facts.CouplingReference:
		return 1
	case facts.CouplingMixin:
		return 2
	case facts.CouplingInheritance:
		return 3
	case facts.CouplingRequire:
		return 4
	case facts.CouplingPackwerk:
		return 5
	default:
		return 1
	}
}

// constIndex resolves a Ruby constant reference to the slash dir of the module
// that declares it.
type constIndex struct {
	qualified map[string]string   // "Orders::Order" -> "app/models/orders"
	bare      map[string][]string // "Order" -> ["app/models/orders", ...] (sorted, deduped)
}

// buildConstIndex indexes every class/module/constant symbol by its qualified and
// bare names. Source dir is the declares-relation target (fallback fileDir).
func buildConstIndex(allFacts []facts.Fact) *constIndex {
	ix := &constIndex{qualified: map[string]string{}, bare: map[string][]string{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch sk, _ := f.PropAny("symbol_kind").(string); sk {
		case facts.SymbolClass, facts.SymbolInterface, facts.SymbolConstant:
		default:
			continue
		}
		dir := declaresTarget(f)
		if dir == "" {
			continue
		}
		qn := stripLeadingColons(f.Name)
		if qn == "" {
			continue
		}
		// Qualified: prefer the shortest declaring dir on collision (nearest root).
		if cur, ok := ix.qualified[qn]; !ok || shorter(dir, cur) {
			ix.qualified[qn] = dir
		}
		bare := lastSegment(qn)
		ix.bare[bare] = append(ix.bare[bare], dir)
	}
	for k, dirs := range ix.bare {
		ix.bare[k] = sortDedupDirs(dirs)
	}
	return ix
}

// resolve returns the declaring module dir of a constant reference, or "". src is
// the referring symbol's dir, used to disambiguate bare-name collisions: Ruby
// constant lookup is lexical, so a declarer in the referrer's own namespace wins.
func (ix *constIndex) resolve(ref, src string) string {
	ref = stripLeadingColons(ref)
	if ref == "" {
		return ""
	}
	if dir, ok := ix.qualified[ref]; ok {
		return dir
	}
	dirs := ix.bare[lastSegment(ref)]
	switch len(dirs) {
	case 0:
		return ""
	case 1:
		return dirs[0]
	}
	// Ambiguous bare name (several declarers of the same simple name). Prefer the
	// candidate sharing the longest leading path with the referrer's dir. If the
	// best match is not strictly better than the runner-up, or no candidate shares
	// any namespace with the referrer, the reference is genuinely ambiguous — drop
	// it rather than fabricate an edge to an arbitrary (shortest-dir) declarer.
	best, bestScore := "", 0
	tie := false
	for _, d := range dirs {
		s := commonPrefixSegments(src, d)
		switch {
		case s > bestScore:
			best, bestScore, tie = d, s, false
		case s == bestScore:
			tie = true
		}
	}
	if bestScore == 0 || tie {
		return ""
	}
	return best
}

// commonPrefixSegments counts the equal leading '/'-separated path segments of a
// and b (e.g. "app/models/x" vs "app/models/y" → 2, "app/x" vs "lib/y" → 0).
func commonPrefixSegments(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}

// classifyRequire resolves a require/require_relative dependency fact: relative
// requires are intra-project (resolved to a module dir when possible); absolute
// requires are stdlib or external. Sets Props["source"] in place.
func classifyRequire(f *facts.Fact, rel *facts.Relation, src string, moduleNames map[string]bool, add func(s, d, kind string)) {
	raw := rel.Target
	isRel, _ := f.PropAny("require_relative").(bool)
	switch {
	case isRel || strings.HasPrefix(raw, "."):
		if dst := resolveRequireRelative(raw, src, moduleNames); dst != "" {
			add(src, dst, facts.CouplingRequire)
		}
		setSource(f, "internal")
	case rubyStdlib[raw] || rubyStdlib[firstPathSeg(raw)]:
		setSource(f, "stdlib")
	default:
		setSource(f, "external")
	}
}

// resolveRequireRelative resolves a relative require path (e.g. "../helper")
// against the importing file's dir, then walks up to the nearest known module.
// Returns "" if it cannot be placed inside the project.
func resolveRequireRelative(raw, importerDir string, moduleNames map[string]bool) string {
	p := strings.TrimSuffix(raw, ".rb")
	base := importerDir
	if base == "" {
		base = "."
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			// stay
		case "..":
			base = parentDir(base)
		default:
			if base == "." {
				base = seg
			} else {
				base = base + "/" + seg
			}
		}
	}
	// The resolved path points at a file; its module is the containing dir, walked
	// up to the nearest known module.
	return nearestModule(fileDir(base), moduleNames)
}

// nearestModule walks up dir's ancestors until it finds a known module, or "".
func nearestModule(dir string, moduleNames map[string]bool) string {
	cur := dir
	for cur != "" && cur != "." {
		if moduleNames[cur] {
			return cur
		}
		cur = parentDir(cur)
	}
	if moduleNames[cur] {
		return cur
	}
	return ""
}

// emitEdges builds one synthetic dependency fact per unique edge. File is set to
// "<srcDir>/_coupling.rb" so that consumers deriving the source module via
// fileDir(File) recover exactly srcDir (a bare srcDir would lose its last segment).
func emitEdges(edges map[[2]string]string, isRails bool) []facts.Fact {
	out := make([]facts.Fact, 0, len(edges))
	for e, kind := range edges {
		src, dst := e[0], e[1]
		props := map[string]any{
			"language":             "ruby",
			"source":               "internal",
			"synthetic_coupling":   true,
			facts.PropCouplingKind: kind,
		}
		if isRails {
			props["framework"] = "rails"
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindDependency,
			Name:      src + " -> " + dst,
			File:      src + "/_coupling.rb",
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: dst}},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- helpers ---

// constFromCall turns a calls-target into its receiver constant, or "" when the
// receiver is not a constant. Splits on the LAST '.' so the inner '::' of a
// namespaced receiver is preserved: "Foo::Bar.method" -> "Foo::Bar",
// "Account.active" -> "Account", "var.method" -> "" (lowercase receiver).
func constFromCall(target string) string {
	target = stripLeadingColons(target)
	dot := strings.LastIndex(target, ".")
	if dot < 0 {
		return ""
	}
	recv := target[:dot]
	if recv == "" || !startsUpper(recv) {
		return ""
	}
	return recv
}

// declaresTarget returns a fact's declares-relation target (its module dir),
// falling back to the directory of its file.
func declaresTarget(f *facts.Fact) string {
	for _, rel := range f.Relations {
		if rel.Kind == facts.RelDeclares {
			return rel.Target
		}
	}
	return fileDir(f.File)
}

// collectModuleNames returns the set of module-fact Names.
func collectModuleNames(allFacts []facts.Fact) map[string]bool {
	m := make(map[string]bool)
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindModule {
			m[allFacts[i].Name] = true
		}
	}
	return m
}

// setSource sets Props["source"] if not already set.
func setSource(f *facts.Fact, source string) {
	if f.Props == nil {
		f.Props = map[string]any{}
	}
	if _, ok := f.Prop("source"); !ok {
		f.SetProp("source", source)
	}
}

// packwerkDir normalizes a Packwerk package name/target: "root" -> ".".
func packwerkDir(name string) string {
	if name == "root" {
		return "."
	}
	return name
}

// fileDir returns the directory portion of a slash file path, or "." for a bare
// filename.
func fileDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

// parentDir returns the parent of a slash dir path, clamped at ".".
func parentDir(p string) string {
	if p == "" || p == "." {
		return "."
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

// stripLeadingColons removes a leading "::" from a Ruby constant reference.
func stripLeadingColons(s string) string {
	return strings.TrimPrefix(s, "::")
}

// lastSegment returns the final "::"-separated segment ("Orders::Order" -> "Order").
func lastSegment(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	return s
}

// firstPathSeg returns the segment before the first '/' ("net/http" -> "net").
func firstPathSeg(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// startsUpper reports whether the first character is an ASCII uppercase letter
// (a Ruby constant always starts uppercase; a variable receiver does not).
func startsUpper(s string) bool {
	return len(s) > 0 && s[0] >= 'A' && s[0] <= 'Z'
}

// shorter reports whether dir a is "nearer a source root" than b: fewer path
// segments, then lexicographically smaller.
func shorter(a, b string) bool {
	sa, sb := strings.Count(a, "/"), strings.Count(b, "/")
	if sa != sb {
		return sa < sb
	}
	return a < b
}

// sortDedupDirs sorts dirs by the shorter() order and removes duplicates.
func sortDedupDirs(dirs []string) []string {
	sort.Slice(dirs, func(i, j int) bool { return shorter(dirs[i], dirs[j]) })
	out := dirs[:0:0]
	var prev string
	for i, d := range dirs {
		if i == 0 || d != prev {
			out = append(out, d)
		}
		prev = d
	}
	return out
}

// rubyStdlib is the set of Ruby standard-library require names, used to split
// non-internal requires into "stdlib" vs "external" in the dependency breakdown.
var rubyStdlib = map[string]bool{
	"set": true, "json": true, "yaml": true, "psych": true, "date": true,
	"time": true, "securerandom": true, "digest": true, "openssl": true,
	"net/http": true, "net/https": true, "net/smtp": true, "net/imap": true,
	"net/pop": true, "net/ftp": true, "net": true, "fileutils": true,
	"logger": true, "forwardable": true, "singleton": true, "ostruct": true,
	"pathname": true, "uri": true, "base64": true, "csv": true, "erb": true,
	"tempfile": true, "stringio": true, "benchmark": true, "monitor": true,
	"timeout": true, "thread": true, "fiber": true, "socket": true,
	"resolv": true, "ipaddr": true, "zlib": true, "stringscanner": true,
	"strscan": true, "io/console": true, "io/wait": true, "pp": true,
	"pstore": true, "delegate": true, "observer": true, "comparable": true,
	"enumerator": true, "rational": true, "complex": true, "bigdecimal": true,
	"prime": true, "matrix": true, "abbrev": true, "shellwords": true,
	"optparse": true, "getoptlong": true, "tsort": true, "weakref": true,
	"objspace": true, "coverage": true, "ripper": true, "readline": true,
	"etc": true, "fcntl": true, "syslog": true, "open3": true, "open-uri": true,
	"tmpdir": true, "find": true, "rbconfig": true, "mkmf": true, "rubygems": true,
}

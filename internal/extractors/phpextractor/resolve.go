package phpextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// resolveImports derives internal module-coupling edges for PHP and returns them as
// synthetic dependency facts. PHP coupling is expressed through references whose
// targets are fully-qualified symbol names (class inheritance, trait use, static
// and global-function calls, instantiations) and through `use` imports — none of
// which the coupling consumers (graph, package metrics, hotspots) count, because
// they only follow dependency facts whose `imports` target matches a module Name.
//
// This pass builds an FQN -> declaring-module-dir index, resolves every cross-module
// reference to a srcDir -> destDir edge, classifies each `use` import as
// internal/external in place, and emits one synthetic dependency fact per unique
// edge. It never guesses — every edge comes from a real parsed reference.
func resolveImports(allFacts []facts.Fact, isWordPress bool) []facts.Fact {
	ix := buildFQNIndex(allFacts)

	edges := map[[2]string]bool{}
	add := func(src, dst string) {
		if src == "" || dst == "" || src == dst {
			return
		}
		edges[[2]string{src, dst}] = true
	}

	for i := range allFacts {
		f := &allFacts[i]
		switch f.Kind {
		case facts.KindSymbol:
			src := declaresTarget(f)
			for _, rel := range f.Relations {
				switch rel.Kind {
				case facts.RelImplements, facts.RelInstantiates:
					add(src, ix.resolve(rel.Target))
				case facts.RelCalls:
					if recv := receiverFromCall(rel.Target); recv != "" {
						add(src, ix.resolve(recv))
					}
				}
			}
		case facts.KindDependency:
			src := fileDir(f.File)
			for j := range f.Relations {
				rel := &f.Relations[j]
				if rel.Kind != facts.RelImports {
					continue
				}
				if dst := ix.resolve(rel.Target); dst != "" {
					add(src, dst)
					setSource(f, "internal")
				} else {
					setSource(f, "external")
				}
			}
		}
	}

	return emitEdges(edges, isWordPress)
}

// fqnIndex resolves a PHP symbol reference to the slash dir of its declaring module.
type fqnIndex struct {
	qualified map[string]string   // "App\Models\User" -> "app/models"
	bare      map[string][]string // "User" -> ["app/models", ...] (sorted, deduped)
}

// buildFQNIndex indexes every class/interface/trait/enum/function/constant symbol by
// its fully-qualified and bare names. The declaring dir is the declares-relation
// target (fallback: the file's directory). Methods and properties are excluded —
// their owning type is already indexed.
func buildFQNIndex(allFacts []facts.Fact) *fqnIndex {
	ix := &fqnIndex{qualified: map[string]string{}, bare: map[string][]string{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch sk, _ := f.PropAny("symbol_kind").(string); sk {
		case facts.SymbolClass, facts.SymbolInterface, facts.SymbolEnum,
			facts.SymbolFunc, facts.SymbolConstant:
		default:
			continue
		}
		// Skip class members (qualified with "::"); only top-level FQNs are indexed.
		if strings.Contains(f.Name, "::") {
			continue
		}
		dir := declaresTarget(f)
		if dir == "" {
			continue
		}
		qn := strings.TrimPrefix(f.Name, "\\")
		if cur, ok := ix.qualified[qn]; !ok || shorter(dir, cur) {
			ix.qualified[qn] = dir
		}
		bare := lastNsSegment(qn)
		ix.bare[bare] = append(ix.bare[bare], dir)
	}
	for k, dirs := range ix.bare {
		ix.bare[k] = sortDedupDirs(dirs)
	}
	return ix
}

// resolve returns the declaring module dir of a reference, or "".
func (ix *fqnIndex) resolve(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "\\")
	if ref == "" {
		return ""
	}
	if dir, ok := ix.qualified[ref]; ok {
		return dir
	}
	if dirs := ix.bare[lastNsSegment(ref)]; len(dirs) > 0 {
		return dirs[0] // pre-sorted: shortest dir, then lexicographic
	}
	return ""
}

// receiverFromCall turns a calls-target into the symbol whose declaring module the
// call couples to: the class part of a static call ("Foo::bar" -> "Foo") or the
// whole target for a global-function call ("helper_fn" -> "helper_fn"). A bare
// instance-method name simply fails to resolve, producing no edge.
func receiverFromCall(target string) string {
	if i := strings.Index(target, "::"); i >= 0 {
		return target[:i]
	}
	return target
}

// emitEdges builds one synthetic dependency fact per unique edge. File is set to
// "<srcDir>/_coupling.php" so consumers deriving the source module via fileDir(File)
// recover exactly srcDir.
func emitEdges(edges map[[2]string]bool, isWordPress bool) []facts.Fact {
	out := make([]facts.Fact, 0, len(edges))
	for e := range edges {
		src, dst := e[0], e[1]
		props := map[string]any{
			"language":           "php",
			"source":             "internal",
			"synthetic_coupling": true,
		}
		if isWordPress {
			props["framework"] = "wordpress"
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindDependency,
			Name:      src + " -> " + dst,
			File:      src + "/_coupling.php",
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: dst}},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- helpers ---

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

// setSource sets Props["source"] if not already set.
func setSource(f *facts.Fact, source string) {
	if f.Props == nil {
		f.Props = map[string]any{}
	}
	if _, ok := f.Prop("source"); !ok {
		f.SetProp("source", source)
	}
}

// fileDir returns the directory portion of a slash file path, or "." for a bare
// filename.
func fileDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
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

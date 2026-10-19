package rubyextractor

import (
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// packwerkSample is the reference behind a coupling edge, as a reader would open
// it: the file, the line and the symbol making the reference.
type packwerkSample struct {
	file   string
	symbol string
	line   int
}

// before orders samples by position, so the one kept per edge does not depend on
// the order facts were extracted in.
func (a packwerkSample) before(b packwerkSample) bool {
	if a.file != b.file {
		return a.file < b.file
	}
	if a.line != b.line {
		return a.line < b.line
	}
	return a.symbol < b.symbol
}

// sampleOf is the reference a fact makes when resolveImports reads an edge off it.
// A mixin or association is declared by the scope named before the arrow.
func sampleOf(f *facts.Fact) packwerkSample {
	s := packwerkSample{file: f.File, line: f.Line}
	switch f.Kind {
	case facts.KindSymbol:
		s.symbol = f.Name
	case facts.KindDependency:
		if scope, _, ok := strings.Cut(f.Name, " -> "); ok {
			s.symbol = scope
		}
	}
	return s
}

// packwerkPack is one package.yml as annotatePackwerkUsage reads it back off the
// module fact parsePackwerk emitted.
type packwerkPack struct {
	idx      int
	name     string // module fact name: the path, or "root"
	dir      string // the path, "." for root
	enforce  bool
	declared map[string]bool
}

// annotatePackwerkUsage compares each packwerk package's declared dependencies
// with the ones its code references, and records the result on the package's
// module fact (see the PropPackwerk* props).
//
// Observed dependencies are the resolved directory edges lifted to packages: each
// end goes to the package whose path is the longest prefix of its directory, and a
// directory under no package belongs to the root package when the repository has
// one. An edge inside one package is no dependency, and a directory that belongs
// to no package at all (no root package.yml) leaves its edges out rather than
// inventing an owner. package.yml's own edges are what is compared, never part of
// what is observed.
//
// Undeclared dependencies are recorded only for a package that enforces
// dependencies, because that is the setting under which packwerk asks for the
// declaration. Unused ones are recorded for every package: a declaration means the
// same thing whether or not it is enforced.
func annotatePackwerkUsage(allFacts []facts.Fact, edges map[[2]string]string, samples map[[2]string]packwerkSample) {
	var packs []*packwerkPack
	byDir := map[string]*packwerkPack{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindModule || !f.PropBool("packwerk") {
			continue
		}
		p := &packwerkPack{idx: i, name: f.Name, dir: packwerkDir(f.Name), enforce: f.PropBool("enforce_dependencies"), declared: map[string]bool{}}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDependsOn {
				p.declared[packName(rel.Target)] = true
			}
		}
		packs = append(packs, p)
		byDir[p.dir] = p
	}
	if len(packs) == 0 {
		return
	}

	owner := func(dir string) *packwerkPack {
		var best *packwerkPack
		for _, p := range packs {
			if p.dir == "." {
				continue
			}
			if (dir == p.dir || strings.HasPrefix(dir, p.dir+"/")) && (best == nil || len(p.dir) > len(best.dir)) {
				best = p
			}
		}
		if best == nil {
			best = byDir["."]
		}
		return best
	}

	observed := map[*packwerkPack]map[string]bool{}
	evidence := map[*packwerkPack]map[string]packwerkSample{}
	for e, kind := range edges {
		if kind == facts.CouplingPackwerk {
			continue
		}
		src, dst := owner(e[0]), owner(e[1])
		if src == nil || dst == nil || src == dst {
			continue
		}
		if observed[src] == nil {
			observed[src] = map[string]bool{}
			evidence[src] = map[string]packwerkSample{}
		}
		observed[src][dst.name] = true
		if s, ok := samples[e]; ok {
			if prev, seen := evidence[src][dst.name]; !seen || s.before(prev) {
				evidence[src][dst.name] = s
			}
		}
	}

	for _, p := range packs {
		f := &allFacts[p.idx]
		seen := sortedKeys(observed[p])
		f.SetProp(facts.PropPackwerkObserved, seen)

		var unused []string
		for dep := range p.declared {
			if !observed[p][dep] {
				unused = append(unused, dep)
			}
		}
		slices.Sort(unused)
		if len(unused) > 0 {
			f.SetProp(facts.PropPackwerkUnused, unused)
		}

		if !p.enforce {
			continue
		}
		var undeclared []string
		ev := map[string]any{}
		for _, dep := range seen {
			if p.declared[dep] {
				continue
			}
			undeclared = append(undeclared, dep)
			if s, ok := evidence[p][dep]; ok {
				ev[dep] = map[string]any{"file": s.file, "symbol": s.symbol, "line": s.line}
			}
		}
		if len(undeclared) > 0 {
			f.SetProp(facts.PropPackwerkUndeclared, undeclared)
			f.SetProp(facts.PropPackwerkUndeclaredEvidence, ev)
		}
	}
}

// packName normalizes a package.yml dependency to module-fact naming: "." and
// "root" are the root package, and a trailing slash is not part of a path.
func packName(dep string) string {
	dep = strings.TrimSuffix(strings.TrimSpace(dep), "/")
	if dep == "." || dep == "" {
		return "root"
	}
	return dep
}

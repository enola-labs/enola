package layers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Declared layer patterns — the layer-intent half of intent compilation. A
// repo whose intent facts declare layers gets its pattern built from the
// declaration at confidence 1.0 (a declared taxonomy is stated, not
// recognised) and verdicted by the SAME violation machinery the heuristic
// patterns use; repos without declarations keep the heuristic path untouched.
//
// Ordering is outermost-first: earlier entries may depend on later ones,
// never the reverse. Level runs opposite to declaration order (the last,
// innermost entry has the lowest level) so the existing wrong-direction test
// reads unchanged.
//
// Glob support is deliberately the ceiling the layer-intent decision set:
// an exact module path, or a `prefix/**` subtree — nothing more.

// declaredPatterns builds the archPattern for the one repo named by repo, out
// of the declarations that name it as their owner. A declaration governs the
// codebase it is written for and nothing else: read across a union, one repo's
// declared taxonomy verdicted another repo's imports wherever their module names
// happened to collide, and reported it at confidence 1.0.
func declaredPatterns(store *facts.Store, repo string) []*archPattern {
	type entry struct {
		name  string
		order int
		paths []string
	}
	byRepo := map[string][]entry{}
	for _, f := range store.ByKind(facts.KindIntent) {
		if f.PropString("intent_kind") != "layer" {
			continue
		}
		order := 0
		if o, ok := f.PropAny("order").(int); ok {
			order = o
		} else if o, ok := f.PropAny("order").(float64); ok {
			order = int(o)
		}
		paths, _ := f.PropAny("paths").([]string)
		if paths == nil {
			if raw, ok := f.PropAny("paths").([]any); ok {
				for _, p := range raw {
					if sp, ok := p.(string); ok {
						paths = append(paths, sp)
					}
				}
			}
		}
		owner := f.PropString("intent_owner")
		if owner == "" {
			owner = f.Repo
		}
		if owner != repo {
			continue
		}
		byRepo[owner] = append(byRepo[owner], entry{name: f.PropString("layer_name"), order: order, paths: paths})
	}
	if len(byRepo) == 0 {
		return nil
	}

	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	var patterns []*archPattern
	for _, repo := range repos {
		entries := byRepo[repo]
		sort.Slice(entries, func(i, j int) bool { return entries[i].order < entries[j].order })
		layerDefs := map[string]*layerDef{}
		for _, en := range entries {
			layerDefs[en.name] = &layerDef{
				Name:     en.name,
				Patterns: en.paths,
				// Outermost-first declaration order, and Level's existing meaning is
				// lower = inner: the last declared (innermost) entry gets level 0.
				Level: len(entries) - 1 - en.order,
			}
		}
		modules := map[string]string{}
		for _, m := range store.ByKind(facts.KindModule) {
			if m.Repo != repo {
				continue
			}
			stripped := strings.TrimPrefix(m.Name, repo+"/")
			for _, en := range entries {
				if matchDeclaredLayerPath(stripped, en.paths) {
					modules[m.Name] = en.name
					break
				}
			}
		}
		patterns = append(patterns, &archPattern{
			Name:       fmt.Sprintf("declared (%s)", repo),
			Confidence: 1.0,
			Layers:     layerDefs,
			Modules:    modules,
		})
	}
	return patterns
}

// matchDeclaredLayerPath applies the bounded glob dialect: exact module path,
// or `prefix/**` matching the prefix's whole subtree (and the prefix itself).
//
// Both sides are normalised to fact-path form first. The declaration is already
// normalised when it is parsed (intent.Declaration.Normalize), and module names
// are normalised when they enter the store — but a snapshot is a durable file,
// and one written by an older Windows build carries `src\lib` in both. Matching
// dialect-to-dialect here means such a snapshot classifies correctly when it is
// read back rather than reproducing the original silence.
func matchDeclaredLayerPath(module string, globs []string) bool {
	module = factpath.Slash(module)
	for _, g := range globs {
		g = factpath.Declared(g)
		if prefix, ok := strings.CutSuffix(g, "/**"); ok {
			if module == prefix || strings.HasPrefix(module, prefix+"/") {
				return true
			}
			continue
		}
		if module == g {
			return true
		}
	}
	return false
}

// vacuousDeclarationConfidence is the confidence a declaration-governs-nothing
// advisory carries. It is deliberately BELOW check.DefaultMinConfidence, the
// 1.00 floor `--fail-on=layers` gates at, and the reason is the same one that
// makes the declared-order description informational: declaring a layer order
// must not fail the pull request that declares it, and the first snapshot after
// someone writes `layers:` is exactly when a mismatched path shows up. So this
// reports, loudly and in the findings list, without gating.
//
// It matches the constraints explainer's empty-component advisory, which exists
// for the identical reason — see its comment: vacuous compliance must never read
// as compliance.
const vacuousDeclarationConfidence = 0.4

// vacuousDeclarationInsights reports a declared layer order that governs nothing,
// or a layer within it that does.
//
// This is the finding whose absence made issue #242 so hard to diagnose. A
// declaration whose paths match no module is not an error anywhere: it parses, it
// validates, it compiles into intent facts, and the explainer emits a finding at
// confidence 1.00 titled "Architecture pattern: declared" — an announcement that a
// layer order is IN FORCE. The only trace of the truth was the number `0` inside
// that sentence, and a reporter on Windows read past it exactly as anyone would.
//
// Two advisories rather than one, because the two failures need different fixes.
// Nothing classified at all is usually one systematic mistake — a wrong root, or
// paths in a dialect the matcher does not read. A single empty layer among
// populated ones is usually a directory that moved out from under one entry.
func vacuousDeclarationInsights(dp *archPattern, modules []facts.Fact) []facts.Insight {
	layerNames := make([]string, 0, len(dp.Layers))
	for name := range dp.Layers {
		layerNames = append(layerNames, name)
	}
	sort.Strings(layerNames)

	if len(dp.Modules) == 0 {
		// Name a few measured module paths. The mismatch is nearly always visible
		// the moment the declared paths and the real ones are read side by side,
		// and the reader should not have to go find the second list.
		sample := make([]string, 0, len(modules))
		for _, m := range modules {
			sample = append(sample, m.Name)
		}
		sort.Strings(sample)
		if len(sample) > 5 {
			sample = sample[:5]
		}
		evidence := make([]facts.Evidence, 0, len(layerNames)+len(sample))
		for _, name := range layerNames {
			evidence = append(evidence, facts.Evidence{
				Fact:   "layer " + name,
				Detail: "declared paths: " + strings.Join(dp.Layers[name].Patterns, ", "),
			})
		}
		for _, mod := range sample {
			evidence = append(evidence, facts.Evidence{Fact: mod, Detail: "a module path this snapshot measured"})
		}
		return []facts.Insight{{
			Title: fmt.Sprintf("Declared layer order %s classifies no modules", dp.Name),
			Description: fmt.Sprintf(
				"The declaration names %d layers and not one of their paths matches any of the %d modules measured here, so the order governs nothing: no import can violate it, and --fail-on=layers has nothing to grade. A declaration in force and a declaration matching nothing report the same way otherwise, which is what this advisory exists to prevent. Layer paths are repo-relative and forward-slash on every host, in one of two forms: an exact module path, or a prefix/** subtree.",
				len(dp.Layers), len(modules)),
			Confidence: vacuousDeclarationConfidence,
			Evidence:   evidence,
			Actions: []string{
				"Compare the declared paths against the module paths listed in the evidence",
				"Check the paths are relative to the repo root, not to a sub-directory",
				"Run `enola constraints lint` to see what each layer selects before snapshotting",
			},
		}}
	}

	var insights []facts.Insight
	for _, name := range layerNames {
		populated := false
		for _, layer := range dp.Modules {
			if layer == name {
				populated = true
				break
			}
		}
		if populated {
			continue
		}
		insights = append(insights, facts.Insight{
			Title: fmt.Sprintf("Declared layer %q classifies no modules", name),
			Description: fmt.Sprintf(
				"Its sibling layers classify %d modules between them, so the declaration is being read — this one entry selects nothing. A layer with no members takes part in no verdict: an import into it cannot be a violation, and one out of it cannot be either. Either the code it named moved, or the paths never matched.",
				len(dp.Modules)),
			Confidence: vacuousDeclarationConfidence,
			Evidence: []facts.Evidence{{
				Fact:   "layer " + name,
				Detail: "declared paths: " + strings.Join(dp.Layers[name].Patterns, ", "),
			}},
			Actions: []string{
				"Fix the layer's paths if the code moved",
				"Remove the layer from the order if the distinction is retired",
			},
		})
	}
	return insights
}

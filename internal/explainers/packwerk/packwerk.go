// Package packwerk reports where a packwerk package's declared dependencies and
// the ones its code references disagree.
//
// package.yml is the declared architecture of a packwerk monolith, and it drifts
// in both directions: a dependency stays declared long after the last reference to
// it is gone, and a reference to a package nobody declared works anyway, because
// Rails autoloads it. Packwerk's own check reports only the second, and only for
// the references it is asked about. The Ruby extractor compares the two on every
// package module fact (facts.PropPackwerk*); this explainer turns the differences
// into findings, one per package and direction.
package packwerk

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

const (
	// unusedConfidence is below 1 because some uses are no edge: a constant named in
	// a string and looked up at runtime, a class named in YAML, a reference from a
	// test file the snapshot does not index. A declared dependency only those reach
	// is reported unused.
	unusedConfidence = 0.7
	// undeclaredConfidence is below 1 because a bare constant can resolve to the
	// wrong one of two packages declaring the same name; the evidence names the
	// reference so a reader can check.
	undeclaredConfidence = 0.9
	// titleSample caps the dependencies a title names; the evidence lists them all.
	titleSample = 3
)

type Explainer struct{}

func New() *Explainer { return &Explainer{} }

func (e *Explainer) Name() string { return "packwerk" }

func (e *Explainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	var packs []facts.Fact
	for _, f := range store.ByKind(facts.KindModule) {
		if f.PropBool("packwerk") {
			packs = append(packs, f)
		}
	}
	slices.SortFunc(packs, func(a, b facts.Fact) int { return strings.Compare(a.Repo+"\x00"+a.Name, b.Repo+"\x00"+b.Name) })

	var out []facts.Insight
	for _, p := range packs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if in, ok := undeclared(p); ok {
			out = append(out, in)
		}
		if in, ok := unused(p); ok {
			out = append(out, in)
		}
	}
	return out, nil
}

// undeclared reports the packages p references without declaring them, leaving
// out those its package_todo.yml already records.
func undeclared(p facts.Fact) (facts.Insight, bool) {
	todo := map[string]bool{}
	for _, d := range stringList(p.PropAny(facts.PropPackwerkTodo)) {
		todo[d] = true
	}
	var deps []string
	acknowledged := 0
	for _, d := range stringList(p.PropAny(facts.PropPackwerkUndeclared)) {
		if todo[d] {
			acknowledged++
			continue
		}
		deps = append(deps, d)
	}
	if len(deps) == 0 {
		return facts.Insight{}, false
	}
	samples, _ := p.PropAny(facts.PropPackwerkUndeclaredEvidence).(map[string]any)
	var evidence []facts.Evidence
	for _, d := range deps {
		ev := facts.Evidence{Fact: p.Name, Detail: fmt.Sprintf("%s references %s, which its package.yml does not declare", p.Name, d)}
		if s, ok := samples[d].(map[string]any); ok {
			ev.File, _ = s["file"].(string)
			ev.Symbol, _ = s["symbol"].(string)
			ev.Line = intOf(s["line"])
		}
		evidence = append(evidence, ev)
	}
	desc := fmt.Sprintf("Code in %s references %d package(s) its package.yml does not list: %s. "+
		"Each reference resolves at runtime because Rails autoloads the constant, which is how the "+
		"coupling went unnoticed; the evidence names the first reference to each.",
		p.Name, len(deps), strings.Join(deps, ", "))
	if acknowledged > 0 {
		desc += fmt.Sprintf(" %d more undeclared package(s) are already recorded in package_todo.yml and left out.", acknowledged)
	}
	return facts.Insight{
		Title:       fmt.Sprintf("Undeclared packwerk dependencies: %s → %s", p.Name, sample(deps)),
		Description: desc,
		Confidence:  undeclaredConfidence,
		Evidence:    evidence,
		Actions: []string{
			"Declare the dependency in package.yml if the coupling is intended",
			"Otherwise route the reference through the other package's public API, or move the code",
		},
		Metrics: map[string]any{"package": p.Name, "direction": "undeclared", "dependencies": deps, "acknowledged": acknowledged},
	}, true
}

// unused reports the packages p declares and never references.
func unused(p facts.Fact) (facts.Insight, bool) {
	deps := stringList(p.PropAny(facts.PropPackwerkUnused))
	if len(deps) == 0 {
		return facts.Insight{}, false
	}
	var evidence []facts.Evidence
	for _, d := range deps {
		evidence = append(evidence, facts.Evidence{
			File:   p.File,
			Fact:   p.Name,
			Detail: fmt.Sprintf("%s declares %s; no reference from its code resolves into it", p.Name, d),
		})
	}
	return facts.Insight{
		Title: fmt.Sprintf("Unused packwerk dependencies: %s declares %s", p.Name, sample(deps)),
		Description: fmt.Sprintf("%s's package.yml declares %d package(s) its code never references: %s. "+
			"A stale declaration hides the package's real coupling and lets a new reference in without review. "+
			"Uses that are no edge — a constant looked up by name, a class named in YAML, a reference only "+
			"from tests — are not seen, so check before removing.",
			p.Name, len(deps), strings.Join(deps, ", ")),
		Confidence: unusedConfidence,
		Evidence:   evidence,
		Actions:    []string{"Remove the declaration from package.yml once no by-name or test-only use remains"},
		Metrics:    map[string]any{"package": p.Name, "direction": "unused", "dependencies": deps},
	}, true
}

// sample names up to titleSample dependencies, and how many more there are.
func sample(deps []string) string {
	if len(deps) <= titleSample {
		return strings.Join(deps, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(deps[:titleSample], ", "), len(deps)-titleSample)
}

// stringList reads a list prop, which a restored snapshot delivers as []any.
func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// intOf reads a number prop, which a restored snapshot delivers as float64.
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

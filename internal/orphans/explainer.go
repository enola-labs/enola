package orphans

import (
	"context"
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/explainers/vendoredcandidates"
	"github.com/enola-labs/enola/internal/facts"
)

// maxIndividualInsights bounds how many per-symbol dead-code insights the
// explainer emits, so a large dead codebase does not flood query_insights. The
// remainder is reported as a single rollup; find_orphans returns the full list.
const maxIndividualInsights = 50

// Explainer adapts dead-code detection to enola's explainer subsystem so its
// findings surface via the OSS query_insights tool (explainer="dead-code"),
// alongside the OSS explainers. It runs during generate_snapshot and reuses the
// same collect/classify core as the find_orphans MCP tool.
type Explainer struct{}

// NewExplainer creates a dead-code Explainer.
func NewExplainer() *Explainer { return &Explainer{} }

// Name is the explainer identifier used as the insight Source and the
// query_insights explainer= filter value.
func (e *Explainer) Name() string { return "dead-code" }

// Explain classifies orphans (mode=both, all visibility) and maps them to
// insights, surfacing high/medium-confidence candidates individually.
func (e *Explainer) Explain(_ context.Context, store *facts.Store) ([]facts.Insight, error) {
	syms, refSources := collect(store)
	orphans := classify(syms, refSources, options{Mode: "both", Visibility: "all"})
	sortOrphans(orphans)
	return orphansToInsights(orphans), nil
}

// moreActionable orders orphans so the most actionable cleanups come first:
// detection confidence (high → medium → low), then visibility (unexported →
// exported), then class (isolated → unreferenced), with package/name as a stable
// tiebreak for deterministic output.
func moreActionable(a, b Orphan) bool {
	// Somebody else's code, last. On gmsh, 20 of the 51 reported candidates sat
	// under contrib/, which is a real finding about a vendored solver and not something
	// the reader is going to delete. Ranking rather than excluding is deliberate: a
	// dependency-named directory routinely holds first-party code too (gmsh's own
	// contrib/mobile/ is gmsh's), and hiding it would be the silent kind of wrong.
	// The tool still returns every candidate either way.
	if va, vb := vendoredcandidates.UnderDependencyParent(a.File), vendoredcandidates.UnderDependencyParent(b.File); va != vb {
		return !va
	}
	if ra, rb := confidenceRank(a.Confidence), confidenceRank(b.Confidence); ra != rb {
		return ra < rb
	}
	if a.Exported != b.Exported {
		return !a.Exported // unexported (safe to delete within repo) first
	}
	if ra, rb := classRank(a.Class), classRank(b.Class); ra != rb {
		return ra < rb
	}
	if a.Package != b.Package {
		return a.Package < b.Package
	}
	return a.Name < b.Name
}

func confidenceRank(c string) int {
	switch c {
	case confHigh:
		return 0
	case confMedium:
		return 1
	default:
		return 2
	}
}

func classRank(c string) int {
	if c == classIsolated {
		return 0
	}
	return 1
}

// orphansToInsights emits one insight per high/medium-confidence orphan (capped),
// plus a rollup covering the omitted high/medium and all low-confidence ones.
// Low-confidence orphans are noisy (dispatch/type usage isn't edge-tracked), so
// they are aggregated rather than surfaced individually.
//
// The capped individual-insight budget is spent on the most ACTIONABLE candidates
// first — ranked by detection confidence (high before medium) then visibility
// (unexported before exported), since high-confidence unexported functions are the
// safest, highest-signal deletions (reliable call tracking, no out-of-snapshot
// consumers). Without this the budget went to whatever sorted first alphabetically,
// burying the actionable set in the rollup behind exported/lower-signal symbols.
func orphansToInsights(orphans []Orphan) []facts.Insight {
	ranked := make([]Orphan, len(orphans))
	copy(ranked, orphans)
	sort.SliceStable(ranked, func(i, j int) bool { return moreActionable(ranked[i], ranked[j]) })

	out := make([]facts.Insight, 0)
	var lowCount, dropped int
	for _, o := range ranked {
		var conf float64
		switch o.Confidence {
		case confHigh:
			conf = 0.8
		case confMedium:
			conf = 0.6
		default:
			lowCount++
			continue
		}
		if len(out) >= maxIndividualInsights {
			dropped++
			continue
		}
		loc := o.File
		if o.Line > 0 {
			loc = fmt.Sprintf("%s:%d", o.File, o.Line)
		}
		ev := []facts.Evidence{{
			Symbol: o.Name,
			File:   o.File,
			Detail: fmt.Sprintf("%s · %s · %s", o.Kind, o.Class, loc),
		}}
		if o.Note != "" {
			ev = append(ev, facts.Evidence{Symbol: o.Name, Detail: o.Note})
		}
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Dead code candidate: %s %s", o.Kind, o.Name),
			Description: fmt.Sprintf("%s %q is %s — nothing in the snapshot graph references it (%s confidence). "+
				"References are matched by short name and biased conservative, so verify against dynamic/reflective "+
				"dispatch and consumers outside the snapshot before removing.", o.Kind, o.Name, o.Class, o.Confidence),
			Confidence: conf,
			Evidence:   ev,
			Actions:    []string{"Confirm no dynamic/reflective or out-of-snapshot callers, then remove the symbol"},
		})
	}
	if dropped > 0 || lowCount > 0 {
		out = append(out, facts.Insight{
			Title: fmt.Sprintf("Additional dead-code candidates: %d more (see find_orphans)", dropped+lowCount),
			Description: fmt.Sprintf("%d further high/medium-confidence orphan(s) were omitted from individual insights "+
				"and %d low-confidence orphan(s) (methods/types/consts/vars — usage not edge-tracked) were not surfaced "+
				"individually. Use the find_orphans tool (output_mode=full) for the complete list.", dropped, lowCount),
			Confidence: 0.4,
			Actions:    []string{"Run find_orphans with confidence=high and visibility=unexported for the safest cleanup set"},
		})
	}
	return out
}

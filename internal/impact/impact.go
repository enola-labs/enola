// Package impact computes the blast radius of one node, for impact_analysis and enola impact.
package impact

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/resolve"
)

// Request is one impact query; a zero bound means its default.
type Request struct {
	Target         string
	MaxDepth       int
	MaxNodes       int
	IncludeForward bool
}

// Report is the traversal, plus how the target was resolved when not by exact name.
type Report struct {
	Resolution *resolve.NameResolution `json:"resolution,omitempty"`
	facts.ImpactResult
}

// ErrNoFacts and ErrNoGraph are values so each caller can word its own remedy.
var (
	ErrNoFacts = errors.New("no facts available")
	ErrNoGraph = errors.New("no graph available")
)

// Analyze resolves the target and walks its dependents. An ambiguous target returns candidates, not an error.
func Analyze(r resolve.Resolver, req Request) (Report, error) {
	store := r.Store
	if store == nil || store.Count() == 0 {
		return Report{}, ErrNoFacts
	}
	graph := store.Graph()
	if graph == nil {
		return Report{}, ErrNoGraph
	}

	if req.Target == "" {
		return Report{}, errors.New("target is required")
	}

	targetName, res, err := r.NodeName(req.Target)
	if err != nil {
		return Report{}, err
	}
	if targetName != "" {
		canonical, normalization := canonicalTarget(store, targetName)
		if canonical != targetName {
			targetName = canonical
			// An exact file_ref normally has no resolution note. Surface this
			// normalization because it materially changes what was traversed.
			if res == nil {
				res = normalization
			}
		}
	}

	// Over threshold: refuse to guess; return resolution with empty results.
	if res != nil && res.Matched == "" {
		return Report{
			Resolution: res,
			ImpactResult: facts.ImpactResult{
				Target:  req.Target,
				ByDepth: map[int][]facts.TraversalNode{},
				Edges:   []facts.TraversalEdge{},
			},
		}, nil
	}

	result := graph.ImpactSet(targetName, req.MaxDepth, req.MaxNodes, req.IncludeForward)
	return Report{Resolution: res, ImpactResult: result}, nil
}

// Resolved reports whether a traversal ran; when false, no dependents means "not asked", not "none".
func (r Report) Resolved() bool {
	return r.Resolution == nil || r.Resolution.Matched != ""
}

// JSON encodes the report as impact_analysis does for output_mode=full.
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// canonicalTarget maps reference-only TypeScript/JavaScript file nodes to the
// extensionless module target imports actually point at. A file_ref records top-level
// calls for dead-code analysis; it is not the dependency node, so reverse traversal from
// it can truthfully see no edges while the corresponding module has many callers.
func canonicalTarget(store *facts.Store, target string) (string, *resolve.NameResolution) {
	exact := store.LookupByExactName(target)
	referenceOnly := false
	for _, f := range exact {
		switch f.Kind {
		case facts.KindFileRef, facts.KindTestRef:
			referenceOnly = true
		default:
			return target, nil
		}
	}
	if !referenceOnly {
		return target, nil
	}

	ext := strings.ToLower(filepath.Ext(target))
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
	default:
		return target, nil
	}
	candidate := strings.TrimSuffix(target, filepath.Ext(target))
	// A module target may be an implicit graph node (relations name it even when no
	// standalone fact does), so confirm it through either a fact or a relation.
	confirmed := len(store.LookupByExactName(candidate)) > 0
	if !confirmed {
		for _, f := range store.All() {
			for _, rel := range f.Relations {
				if rel.Target == candidate {
					confirmed = true
					break
				}
			}
			if confirmed {
				break
			}
		}
	}
	if !confirmed {
		return target, nil
	}
	return candidate, &resolve.NameResolution{Query: target, Matched: candidate, AutoPicked: true}
}

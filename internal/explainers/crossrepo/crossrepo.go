// Package crossrepo provides an explainer that summarizes the cross-repo
// dependency edges synthesized by internal/linkers/crossrepo into
// human-readable insights for the LLM context and insights.json output.
package crossrepo

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// CrossRepoExplainer turns synthetic cross_repo dependency facts into insights.
type CrossRepoExplainer struct{}

// New creates a new CrossRepoExplainer.
func New() *CrossRepoExplainer {
	return &CrossRepoExplainer{}
}

func (e *CrossRepoExplainer) Name() string {
	return "crossrepo"
}

// Explain reads the cross_repo dependency facts the linker already added to the
// store and emits one summarizing insight describing how the repositories
// depend on each other. It returns nothing for single-repo snapshots.
func (e *CrossRepoExplainer) Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error) {
	var out []facts.Insight
	if in := dependencyInsight(store); in != nil {
		out = append(out, *in)
	}
	if in := sharedCodeInsight(store); in != nil {
		out = append(out, *in)
	}
	return out, nil
}

// queryByType fetches the cross-repo dependency facts of one type, name-sorted.
func queryByType(store *facts.Store, typ string) []facts.Fact {
	ff, _ := store.QueryAdvanced(facts.QueryOpts{
		Kind:      facts.KindDependency,
		Prop:      "type",
		PropValue: typ,
		Limit:     500,
	})
	sort.Slice(ff, func(i, j int) bool { return ff[i].Name < ff[j].Name })
	return ff
}

// dependencyInsight summarizes the real, directional cross-repo edges — the ones whose
// depends_on relations traversal can follow.
func dependencyInsight(store *facts.Store) *facts.Insight {
	deps := queryByType(store, facts.TypeCrossRepo)
	if len(deps) == 0 {
		return nil
	}

	evidence := make([]facts.Evidence, 0, len(deps))
	var lines []string
	for _, d := range deps {
		detail := edgeDetail(d)
		evidence = append(evidence, facts.Evidence{
			Fact:   d.Name,
			Detail: detail,
		})
		lines = append(lines, d.Name+" ("+detail+")")
	}

	return &facts.Insight{
		Title: fmt.Sprintf("Cross-repo dependencies (%d edges)", len(deps)),
		Description: "These service-to-service dependencies span repositories: " +
			strings.Join(lines, "; ") + ". Traverse from a repo label (service node) " +
			"to follow requests across repos.",
		Confidence: 0.9,
		Evidence:   evidence,
	}
}

// sharedCodeInsight reports repo pairs that declare many of the same distinctive type
// names without any import or call between them. Confidence is deliberately moderate
// and the wording deliberately hedged: the linker compares NAMES, never file contents,
// so it cannot tell code copied and kept in sync from code that has since diverged, or
// from two teams independently modelling one domain with the same vocabulary.
func sharedCodeInsight(store *facts.Store) *facts.Insight {
	pairs := queryByType(store, facts.TypeCrossRepoSharedCode)
	if len(pairs) == 0 {
		return nil
	}

	evidence := make([]facts.Evidence, 0, len(pairs))
	var lines []string
	verified := false
	for _, p := range pairs {
		count := propInt(p, "symbol_count")
		detail := fmt.Sprintf("%d shared type(s)", count)
		// name_match_count is only set when source verification narrowed the set, so
		// its presence is what tells us the counts were measured rather than inferred.
		if names := propInt(p, "name_match_count"); names > count {
			detail = fmt.Sprintf("%d of %d shared type name(s) backed by near-identical files", count, names)
			verified = true
		}
		evidence = append(evidence, facts.Evidence{Fact: p.Name, Detail: detail})
		lines = append(lines, p.Name+" ("+detail+")")
	}

	// A verified count is a measurement of the files, not an inference from names, so
	// it earns materially higher confidence than the name-only fallback.
	confidence, caveat := 0.5, "Matching names do not prove the code is still shared, so diff the declarations before relying on it."
	if verified {
		confidence = 0.8
		caveat = "Counts are verified by comparing the declaring files, not by name alone; " +
			"names that matched without matching code were dropped."
	}

	return &facts.Insight{
		Title: fmt.Sprintf("Shared code between repos (%d pair(s))", len(pairs)),
		Description: "These repo pairs declare the same distinctive types, " +
			"which usually means copied or vendored code, or a common ancestor: " +
			strings.Join(lines, "; ") + ". This is NOT a dependency — neither repo imports " +
			"or calls the other — so these pairs carry no graph edge and do not appear in " +
			"traverse, find_path or impact_analysis. Treat it as a maintenance signal: a fix " +
			"in one may be needed in the other. " + caveat,
		Confidence: confidence,
		Evidence:   evidence,
	}
}

// edgeDetail renders a short description of what justifies a cross-repo edge.
func edgeDetail(d facts.Fact) string {
	var parts []string
	if v := stringSlice(d.PropAny("via")); len(v) > 0 {
		parts = append(parts, "via "+strings.Join(v, "+"))
	}
	if n := propInt(d, "endpoint_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d endpoint(s)", n))
	}
	if n := propInt(d, "import_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d import(s)", n))
	}
	if n := propInt(d, "symbol_count"); n > 0 {
		parts = append(parts, fmt.Sprintf("%d shared symbol(s)", n))
	}
	if len(parts) == 0 {
		return "cross-repo dependency"
	}
	return strings.Join(parts, ", ")
}

// stringSlice reads a string-slice prop, tolerating both the in-memory []string
// form and the []any form a JSON round-trip through facts.jsonl produces (JSON
// arrays decode as []any, not []string) — the slice analog of propInt's float64
// handling. Non-string elements are skipped.
func stringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// propInt reads an int-valued prop, tolerating the float64 form that survives a
// JSON round-trip through facts.jsonl.
func propInt(d facts.Fact, key string) int {
	if d.Props == nil {
		return 0
	}
	switch v := d.PropAny(key).(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

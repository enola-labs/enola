// Package extcoverage lets any extractor report what it saw and could not
// resolve, in the shape the cross-repo layer already emits.
//
// The mechanism was built for the Ruby extractor and its ADR predicted this
// exact problem: "a zero from an extractor that never adopted it reads the same
// as a zero from one that did." A year of that is nineteen extractors and one
// reporter — so the helper moved out of the Ruby package, which is the whole
// difference between a mechanism and one extractor's private habit.
package extcoverage

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Fact accounts for what an extractor resolved and what it could not, keyed by
// a named cause so the number is a task rather than a total.
//
// It returns false when the extractor had nothing to look at. An extractor that
// examined nothing must not report a confident zero — that is the failure this
// whole mechanism exists to prevent, and reproducing it one level down would be
// its own joke.
func Fact(repoPath, name, edgeType string, resolved int, unresolved map[string]int) (facts.Fact, bool) {
	total := 0
	for _, n := range unresolved {
		total += n
	}
	if resolved == 0 && total == 0 {
		return facts.Fact{}, false
	}

	causes := make([]string, 0, len(unresolved))
	for cause := range unresolved {
		causes = append(causes, cause)
	}
	sort.Strings(causes)

	entry := map[string]any{
		"edge_type":  edgeType,
		"detected":   resolved + total,
		"resolved":   resolved,
		"unresolved": total,
	}
	props := map[string]any{
		"extractor":     extractorOf(name),
		"language":      extractorOf(name),
		"edge_coverage": []map[string]any{entry},
	}
	if len(causes) > 0 {
		counts := make([]string, 0, len(causes))
		for _, cause := range causes {
			counts = append(counts, fmt.Sprintf("%s=%d", cause, unresolved[cause]))
		}
		// Naming what was unread is what makes the number actionable: "59 unread
		// declarations" is a metric, "jsonapi_resources=59" is a task.
		props["unresolved_macros"] = strings.Join(counts, ",")
	}
	return facts.Fact{
		Kind:  facts.KindExtraction,
		Name:  name,
		File:  filepath.Base(repoPath),
		Props: props,
	}, true
}

// extractorOf reads the extractor's name off the fact name, which is written
// "<extractor>:<surface>" — "ruby:routes", "typescript:templates".
func extractorOf(name string) string {
	if extractor, _, found := strings.Cut(name, ":"); found {
		return extractor
	}
	return name
}

// Counts is what one repository resolved, for its coverage fact.
type Counts struct{ Resolved, Unresolved, WithMisses int }

// ByRepo accumulates Counts per repository label, for a pass that runs over a
// multi-repo store: a binder, which sees every repository at once.
type ByRepo map[string]*Counts

// For returns the counts for repo, creating them on first use.
func (b ByRepo) For(repo string) *Counts {
	c := b[repo]
	if c == nil {
		c = &Counts{}
		b[repo] = c
	}
	return c
}

// Facts builds one coverage fact per repository, in label order, each tagged with
// its repository. missesProp, when non-empty, records how many items had misses.
//
// Per repository because the alternative was wrong in a way that moved: summed
// over the whole store and filed under the first label the store listed, which in
// a multi-repo store was an arbitrary repository, a different one from run to run.
func (b ByRepo) Facts(name, edgeType, cause, missesProp string) []facts.Fact {
	repos := make([]string, 0, len(b))
	for repo := range b {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	var out []facts.Fact
	for _, repo := range repos {
		c := b[repo]
		root := repo
		if root == "" {
			root = "."
		}
		fact, ok := Fact(root, name, edgeType, c.Resolved, map[string]int{cause: c.Unresolved})
		if !ok {
			continue
		}
		fact.Repo = repo
		if missesProp != "" && c.WithMisses > 0 {
			fact.SetProp(missesProp, c.WithMisses)
		}
		out = append(out, fact)
	}
	return out
}

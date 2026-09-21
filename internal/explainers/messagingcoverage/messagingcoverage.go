// Package messagingcoverage turns AsyncAPI-to-code binding verdicts into
// actionable contract coverage findings.
package messagingcoverage

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

const maxEvidence = 25

type Explainer struct{}

func New() *Explainer           { return &Explainer{} }
func (*Explainer) Name() string { return "messaging-coverage" }

type item struct {
	name, file, symbol, detail string
	line                       int
}

// Explain emits the two highest-value messaging coverage findings: static code
// operations with no declared contract, and declared operations for which Enola
// detected no implementation. The latter is deliberately framed as a candidate
// because wrappers, dynamic topics and unsupported client libraries may be missed.
func (*Explainer) Explain(_ context.Context, store *facts.Store) ([]facts.Insight, error) {
	undeclared := map[string]map[string]item{}
	unimplemented := map[string]map[string]item{}
	duplicates := map[string]map[string]item{}
	for _, f := range store.ByKind(facts.KindStorage) {
		repo := f.Repo
		if repo == "" {
			repo = "(unlabeled)"
		}
		switch f.PropString(facts.PropMessagingContractStatus) {
		case facts.MessagingContractStatusUndeclared:
			addItem(undeclared, repo, item{
				name: f.Name, file: f.File, line: f.Line, symbol: f.PropString("code_symbol"),
				detail: fmt.Sprintf("%s %s has no matching AsyncAPI operation", f.PropString(facts.PropMessagingOperation), f.Name),
			})
		}
		if f.PropString(facts.PropMessagingImplementationStatus) == facts.MessagingImplementationUnimplemented &&
			f.PropString(facts.PropMessagingCanonicalFile) == "" {
			label := f.PropString("operationId")
			if label == "" {
				label = f.Name
			}
			addItem(unimplemented, repo, item{
				name: f.Name, file: f.File,
				detail: fmt.Sprintf("AsyncAPI operation %s (%s %s) has no detected implementation",
					label, f.PropString(facts.PropMessagingOperation), f.Name),
			})
		}
		if others, ok := f.PropAny(facts.PropMessagingDuplicateOf).([]string); ok && len(others) > 0 {
			addItem(duplicates, repo, item{
				name: f.Name, file: f.File,
				detail: fmt.Sprintf("%s %s also declared, inconsistently, in %s", f.PropString(facts.PropMessagingOperation), f.Name, strings.Join(others, ", ")),
			})
		}
	}

	var insights []facts.Insight
	for _, repo := range sortedRepos(undeclared) {
		items := sortedItems(undeclared[repo])
		insights = append(insights, facts.Insight{
			Title: fmt.Sprintf("Undeclared messaging operations: %d call site(s) in %s have no AsyncAPI contract", len(items), repo),
			Description: fmt.Sprintf("Enola detected %d static messaging call site(s) in %s whose topic and direction match no AsyncAPI operation in the repository. Samples: %s.",
				len(items), repo, samples(items)),
			Confidence: 0.9,
			Evidence:   evidence(items),
			Actions:    []string{"Declare the operation in AsyncAPI or load the specification that owns it", "Remove the call if the messaging operation is obsolete"},
		})
	}
	for _, repo := range sortedRepos(unimplemented) {
		items := sortedItems(unimplemented[repo])
		insights = append(insights, facts.Insight{
			Title: fmt.Sprintf("Unimplemented messaging contract candidates: %d AsyncAPI operation(s) in %s have no detected code", len(items), repo),
			Description: fmt.Sprintf("Enola found no supported code implementation for %d AsyncAPI operation(s) in %s. These are candidates only: wrappers, dynamic topics, unsupported client libraries, or implementations outside the snapshot may exist. Samples: %s.",
				len(items), repo, samples(items)),
			Confidence: 0.65,
			Evidence:   evidence(items),
			Actions:    []string{"Verify each operation against wrappers and runtime topic configuration", "Add the implementing repository or client library to the snapshot if it is missing"},
		})
	}
	for _, repo := range sortedRepos(duplicates) {
		items := sortedItems(duplicates[repo])
		insights = append(insights, facts.Insight{
			Title: fmt.Sprintf("Conflicting messaging contracts: %d operation(s) in %s declared inconsistently across specs", len(items), repo),
			Description: fmt.Sprintf("Enola found %d AsyncAPI operation(s) in %s declared more than once with different content — a different operationId, message or schema — across the specs loaded. A binding to either cannot be resolved. Samples: %s.",
				len(items), repo, samples(items)),
			Confidence: 0.9,
			Evidence:   evidence(items),
			Actions:    []string{"Reconcile the conflicting spec files, or remove the stale one"},
		})
	}
	return insights, nil
}

func addItem(groups map[string]map[string]item, repo string, it item) {
	if groups[repo] == nil {
		groups[repo] = map[string]item{}
	}
	key := it.file + "\x00" + fmt.Sprint(it.line) + "\x00" + it.name + "\x00" + it.symbol
	groups[repo][key] = it
}

func sortedRepos(groups map[string]map[string]item) []string {
	out := make([]string, 0, len(groups))
	for repo := range groups {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

func sortedItems(in map[string]item) []item {
	out := make([]item, 0, len(in))
	for _, it := range in {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].symbol < out[j].symbol
	})
	return out
}

func evidence(items []item) []facts.Evidence {
	n := len(items)
	if n > maxEvidence {
		n = maxEvidence
	}
	out := make([]facts.Evidence, 0, n)
	for _, it := range items[:n] {
		out = append(out, facts.Evidence{Fact: it.name, File: it.file, Symbol: it.symbol, Detail: it.detail})
	}
	return out
}

func samples(items []item) string {
	n := len(items)
	if n > maxEvidence {
		n = maxEvidence
	}
	labels := make([]string, 0, n)
	for _, it := range items[:n] {
		labels = append(labels, it.detail)
	}
	out := strings.Join(labels, "; ")
	if len(items) > n {
		out += fmt.Sprintf(" (+%d more)", len(items)-n)
	}
	return out
}

package check

import (
	"fmt"
	"sort"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/providers"
	"github.com/enola-labs/enola/pkg/plugin"
)

const (
	ProducerExtractor = "extractor"
	ProducerProvider  = "provider"

	SideBaseline = "baseline"
	SideCurrent  = "current"
)

type FactFileOwner interface {
	OwnsFactFile(relFile string) bool
}

type FileOwnership map[string]func(relFile string) bool

func OwnershipFromExtractors(exts []plugin.Extractor) FileOwnership {
	owners := make(FileOwnership, len(exts))
	for _, ext := range exts {
		switch o := ext.(type) {
		case plugin.FileOwner:
			owners[ext.Name()] = o.OwnsFile
		case FactFileOwner:
			owners[ext.Name()] = o.OwnsFactFile
		}
	}
	return owners
}

type ExcludedProducer struct {
	Name                     string `json:"name"`
	Kind                     string `json:"kind"`
	LackedBy                 string `json:"lacked_by"`
	BaselineFactsExcluded    int    `json:"baseline_facts_excluded"`
	CurrentFactsExcluded     int    `json:"current_facts_excluded"`
	BaselineFindingsExcluded int    `json:"baseline_findings_excluded"`
	CurrentFindingsExcluded  int    `json:"current_findings_excluded"`
}

type IntersectionGrading struct {
	SharedExtractors []string           `json:"shared_extractors"`
	SharedProviders  []string           `json:"shared_providers,omitempty"`
	Excluded         []ExcludedProducer `json:"excluded"`
}

func (g IntersectionGrading) Families() int {
	return len(g.SharedExtractors) + len(g.SharedProviders)
}

func RegradeIntersection(declined Verdict, base, current *facts.Snapshot, p Policy, owners FileOwnership, currentFindings []facts.Insight, focus string, measurements ...Measurement) Verdict {
	if declined.Status != StatusIncomparable || base == nil || current == nil {
		return declined
	}
	for _, k := range declined.BlockingKinds {
		if k != diff.WarnExtractorSet && k != diff.WarnProviderSet {
			return declined
		}
	}
	excluded := disputedProducers(base.Meta, current.Meta)
	if len(excluded) == 0 {
		return declined
	}
	matchers := make([]*producerMatcher, 0, len(excluded))
	for i := range excluded {
		m, err := newProducerMatcher(&excluded[i], owners)
		if err != nil {
			declined.ComparabilityWarnings = append(declined.ComparabilityWarnings, err.Error())
			return declined
		}
		matchers = append(matchers, m)
	}

	filteredBase := excludeSide(base, matchers, SideBaseline)
	filteredCurrent := excludeSide(current, matchers, SideCurrent)

	sharedExtractors := sharedNames(base.Meta.Extractors, current.Meta.Extractors)
	sharedProviders := sharedNames(facts.RanProviders(base.Meta.Providers), facts.RanProviders(current.Meta.Providers))
	filteredBase.Meta = restrictMeta(base.Meta, sharedExtractors, sharedProviders)
	filteredCurrent.Meta = restrictMeta(current.Meta, sharedExtractors, sharedProviders)

	gradable := make([]facts.Insight, 0, len(currentFindings))
	for _, in := range currentFindings {
		if matcherForInsight(matchers, in) == nil {
			gradable = append(gradable, in)
		}
	}

	d := diff.Compute(filteredBase, filteredCurrent)
	if focus != "" {
		d = d.Focused(focus)
	}
	v := EvaluateCurrent(d, p, gradable, measurements...)
	switch v.Status {
	case StatusClean:
		v.Status = StatusPartialClean
	case StatusRegression:
		v.Status = StatusPartialRegression
	default:
		return declined
	}
	v.Intersection = &IntersectionGrading{
		SharedExtractors: sharedExtractors,
		SharedProviders:  sharedProviders,
		Excluded:         excluded,
	}
	return v
}

type producerMatcher struct {
	producer    *ExcludedProducer
	matchesFact func(f facts.Fact) bool
	ownsFile    func(rel string) bool
	factNames   map[string]bool
	factFiles   map[string]bool
}

func newProducerMatcher(ex *ExcludedProducer, owners FileOwnership) (*producerMatcher, error) {
	if ex.Kind == ProducerProvider {
		name := ex.Name
		return &producerMatcher{
			producer:    ex,
			matchesFact: func(f facts.Fact) bool { return f.PropAny(providers.PropProvider) == name },
		}, nil
	}
	owns, ok := owners[ex.Name]
	if !ok {
		return nil, fmt.Errorf("cannot grade the intersection: extractor %s declares no file ownership, so its facts cannot be attributed — the decline stands", ex.Name)
	}
	return &producerMatcher{
		producer:    ex,
		matchesFact: func(f facts.Fact) bool { return f.File != "" && owns(f.File) },
		ownsFile:    owns,
	}, nil
}

func (m *producerMatcher) matchesInsight(in facts.Insight) bool {
	for _, ev := range in.Evidence {
		if ev.Fact != "" && m.factNames[ev.Fact] {
			return true
		}
		if ev.Symbol != "" && m.factNames[ev.Symbol] {
			return true
		}
		if ev.File != "" && (m.factFiles[ev.File] || (m.ownsFile != nil && m.ownsFile(ev.File))) {
			return true
		}
	}
	return false
}

func (m *producerMatcher) countFact(side string) {
	if side == SideBaseline {
		m.producer.BaselineFactsExcluded++
	} else {
		m.producer.CurrentFactsExcluded++
	}
}

func (m *producerMatcher) countFinding(side string) {
	if side == SideBaseline {
		m.producer.BaselineFindingsExcluded++
	} else {
		m.producer.CurrentFindingsExcluded++
	}
}

func excludeSide(s *facts.Snapshot, matchers []*producerMatcher, side string) *facts.Snapshot {
	for _, m := range matchers {
		m.factNames = map[string]bool{}
		m.factFiles = map[string]bool{}
	}
	kept := make([]facts.Fact, 0, len(s.Facts))
	for _, f := range s.Facts {
		m := matcherForFact(matchers, f)
		if m == nil {
			kept = append(kept, f)
			continue
		}
		m.countFact(side)
		if f.Name != "" {
			m.factNames[f.Name] = true
		}
		if f.File != "" {
			m.factFiles[f.File] = true
		}
	}
	keptInsights := make([]facts.Insight, 0, len(s.Insights))
	for _, in := range s.Insights {
		m := matcherForInsight(matchers, in)
		if m == nil {
			keptInsights = append(keptInsights, in)
			continue
		}
		m.countFinding(side)
	}
	out := *s
	out.Facts = kept
	out.Insights = keptInsights
	return &out
}

func matcherForFact(matchers []*producerMatcher, f facts.Fact) *producerMatcher {
	for _, m := range matchers {
		if m.matchesFact(f) {
			return m
		}
	}
	return nil
}

func matcherForInsight(matchers []*producerMatcher, in facts.Insight) *producerMatcher {
	for _, m := range matchers {
		if m.matchesInsight(in) {
			return m
		}
	}
	return nil
}

func disputedProducers(base, cur facts.SnapshotMeta) []ExcludedProducer {
	var out []ExcludedProducer
	add := func(names []string, kind, lackedBy string) {
		for _, name := range names {
			out = append(out, ExcludedProducer{Name: name, Kind: kind, LackedBy: lackedBy})
		}
	}
	add(namesMissingFrom(base.Extractors, cur.Extractors), ProducerExtractor, SideCurrent)
	add(namesMissingFrom(cur.Extractors, base.Extractors), ProducerExtractor, SideBaseline)
	baseRan, curRan := facts.RanProviders(base.Providers), facts.RanProviders(cur.Providers)
	add(namesMissingFrom(baseRan, curRan), ProducerProvider, SideCurrent)
	add(namesMissingFrom(curRan, baseRan), ProducerProvider, SideBaseline)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func namesMissingFrom(want, have []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

func sharedNames(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	var out []string
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		if set[s] && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func restrictMeta(m facts.SnapshotMeta, sharedExtractors, sharedProviders []string) facts.SnapshotMeta {
	out := m
	out.Extractors = append([]string(nil), sharedExtractors...)
	shared := make(map[string]bool, len(sharedProviders))
	for _, name := range sharedProviders {
		shared[name] = true
	}
	var records []facts.ProviderRecord
	for _, r := range m.Providers {
		if !r.Skipped && shared[r.Name] {
			records = append(records, r)
		}
	}
	out.Providers = records
	return out
}

package mining

import (
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

const (
	FamilyPropImplication = "prop-implication"
	FamilyNaming          = "naming"
	FamilyForbidEdge      = "forbid-edge"
	FamilyAllowOnly       = "allow-only"
	FamilyMethodPresence  = "method-presence"
)

var familyOrder = []string{
	FamilyAllowOnly, FamilyForbidEdge, FamilyMethodPresence, FamilyNaming, FamilyPropImplication,
}

var minedKindOrder = []string{facts.KindModule, facts.KindRoute, facts.KindStorage, facts.KindSymbol}

var minedVias = []string{facts.RelCalls, facts.RelDependsOn, facts.RelImplements, facts.RelImports}

type Config struct {
	MinSupport         int
	MinConfidence      float64
	MaxExceptions      int
	IncludeTautologies bool
}

func DefaultConfig() Config {
	return Config{MinSupport: 10, MinConfidence: 0.9, MaxExceptions: 20}
}

type Exception struct {
	Name   string `json:"name"`
	File   string `json:"file,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Witness is one fact that conforms to a candidate's statement, kept so a
// scaffold can show the rule what it is for before it shows what it forbids.
// A naming witness carries the name and its file; an edge witness carries the
// source name, its file and the edge's target.
type Witness struct {
	Name   string `json:"name"`
	File   string `json:"file,omitempty"`
	Target string `json:"target,omitempty"`
}

const maxWitnesses = 3

type Candidate struct {
	Family      string                       `json:"family"`
	Kind        string                       `json:"kind,omitempty"`
	Service     string                       `json:"service,omitempty"`
	Identity    string                       `json:"identity"`
	Statement   string                       `json:"statement"`
	Numerator   int                          `json:"numerator"`
	Denominator int                          `json:"denominator"`
	Confidence  float64                      `json:"confidence"`
	Exceptions  []Exception                  `json:"exceptions"`
	Witnesses   []Witness                    `json:"witnesses,omitempty"`
	Components  []intent.ConstraintComponent `json:"-"`
	Rule        intent.ConstraintRule        `json:"-"`
	YAML        string                       `json:"yaml"`

	pairKey string
}

func (c Candidate) Score() float64 { return c.Confidence * float64(c.Denominator) }

type SuppressedCount struct {
	Family               string `json:"family"`
	BelowSupportFloor    int    `json:"below_support_floor"`
	OverExceptionCeiling int    `json:"over_exception_ceiling"`
	Tautological         int    `json:"tautological"`
}

type Report struct {
	Config     Config
	FactCount  int
	Candidates []Candidate
	Suppressed []SuppressedCount
}

func Mine(store *facts.Store, cfg Config) *Report {
	if cfg.MinSupport < 1 {
		cfg.MinSupport = 1
	}
	labels := store.RepoLabels()
	sort.Strings(labels)

	suppressed := map[string]*SuppressedCount{}
	for _, family := range familyOrder {
		suppressed[family] = &SuppressedCount{Family: family}
	}

	var candidates []Candidate
	if len(labels) > 1 {
		for _, label := range labels {
			s := newScope(store.ByRepo(label), label, cfg, suppressed)
			candidates = append(candidates, s.mine()...)
		}
	} else {
		s := newScope(store.All(), "", cfg, suppressed)
		candidates = append(candidates, s.mine()...)
	}

	report := &Report{Config: cfg, FactCount: store.Count()}
	report.Candidates = finalize(candidates)
	for _, family := range familyOrder {
		report.Suppressed = append(report.Suppressed, *suppressed[family])
	}
	return report
}

func finalize(candidates []Candidate) []Candidate {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Family != candidates[j].Family {
			return candidates[i].Family < candidates[j].Family
		}
		if candidates[i].Service != candidates[j].Service {
			return candidates[i].Service < candidates[j].Service
		}
		return candidates[i].Statement < candidates[j].Statement
	})
	seen := map[string]int{}
	for i := range candidates {
		id := candidates[i].Rule.ID
		seen[id]++
		if n := seen[id]; n > 1 {
			candidates[i].Rule.ID = id + "-" + strconv.Itoa(n)
		}
		candidates[i].YAML = renderDeclarationYAML(candidates[i].Components, candidates[i].Rule)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score() != candidates[j].Score() {
			return candidates[i].Score() > candidates[j].Score()
		}
		return false
	})
	return candidates
}

type member struct {
	name string
	file string
	fact facts.Fact
}

type scope struct {
	cfg        Config
	service    string
	members    map[string][]member
	allFacts   map[string][]member
	suppressed map[string]*SuppressedCount
}

func newScope(ff []facts.Fact, service string, cfg Config, suppressed map[string]*SuppressedCount) *scope {
	s := &scope{
		cfg:        cfg,
		service:    service,
		members:    map[string][]member{},
		allFacts:   map[string][]member{},
		suppressed: suppressed,
	}
	mined := map[string]bool{facts.KindDependency: true}
	for _, kind := range minedKindOrder {
		mined[kind] = true
	}
	for _, f := range ff {
		if !mined[f.Kind] || f.Name == "" {
			continue
		}
		s.allFacts[f.Kind] = append(s.allFacts[f.Kind], member{name: f.Name, file: f.IdentityFile(), fact: f})
	}
	for kind := range s.allFacts {
		all := s.allFacts[kind]
		sort.Slice(all, func(i, j int) bool {
			if all[i].name != all[j].name {
				return all[i].name < all[j].name
			}
			return all[i].file < all[j].file
		})
	}
	for _, kind := range minedKindOrder {
		var deduped []member
		lastName := ""
		for _, m := range s.allFacts[kind] {
			if m.file == "" || m.name == lastName {
				continue
			}
			lastName = m.name
			deduped = append(deduped, m)
		}
		s.members[kind] = deduped
	}
	return s
}

func (s *scope) mine() []Candidate {
	var out []Candidate
	out = append(out, s.mineProps()...)
	out = append(out, s.mineNaming()...)
	out = append(out, s.mineEdges()...)
	out = append(out, s.mineDefines()...)
	return out
}

func (s *scope) admit(family string, support, exceptions int) bool {
	if support < s.cfg.MinSupport {
		s.suppressed[family].BelowSupportFloor++
		return false
	}
	if exceptions > s.cfg.MaxExceptions {
		s.suppressed[family].OverExceptionCeiling++
		return false
	}
	return true
}

func (s *scope) kindComponent(kind string, ms []member) intent.ConstraintComponent {
	return intent.ConstraintComponent{
		Name:    slug("mined", kind),
		Service: s.service,
		Kind:    kind,
		Match:   topPatterns(ms),
	}
}

func (s *scope) clusterComponent(cluster, kind string) intent.ConstraintComponent {
	return intent.ConstraintComponent{
		Name:    slug("mined", cluster),
		Service: s.service,
		Kind:    kind,
		Match:   []string{cluster + "/**"},
	}
}

func (s *scope) statement(text string) string {
	if s.service == "" {
		return text
	}
	return "[" + s.service + "] " + text
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

func topPatterns(ms []member) []string {
	set := map[string]bool{}
	for _, m := range ms {
		if i := strings.IndexByte(m.file, '/'); i > 0 {
			set[m.file[:i]+"/**"] = true
		} else {
			set[m.file] = true
		}
	}
	patterns := make([]string, 0, len(set))
	for p := range set {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	return patterns
}

func clusterOf(file string) string {
	segs := strings.Split(file, "/")
	switch {
	case len(segs) >= 3:
		return segs[0] + "/" + segs[1]
	case len(segs) == 2:
		return segs[0]
	default:
		return ""
	}
}

func clusterPrefixes(file string) []string {
	segs := strings.Split(file, "/")
	depth := len(segs) - 1
	if depth > 3 {
		depth = 3
	}
	prefixes := make([]string, 0, depth)
	for i := 1; i <= depth; i++ {
		prefixes = append(prefixes, strings.Join(segs[:i], "/"))
	}
	return prefixes
}

func identityKey(parts ...string) string {
	escaped := make([]string, len(parts))
	for i, part := range parts {
		part = strings.ReplaceAll(part, `\`, `\\`)
		escaped[i] = strings.ReplaceAll(part, "|", `\|`)
	}
	return strings.Join(escaped, "|")
}

func slug(parts ...string) string {
	var b strings.Builder
	lastDash := true
	for _, part := range parts {
		for _, r := range strings.ToLower(part) {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			switch {
			case ok:
				b.WriteRune(r)
				lastDash = false
			case !lastDash:
				b.WriteByte('-')
				lastDash = true
			}
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

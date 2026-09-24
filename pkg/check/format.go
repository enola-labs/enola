package check

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
)

// Format names one writer over a finished Verdict. Every writer reads the
// verdict the check already computed and recomputes nothing, so a fifth
// format is a table row and never a second grading path.
type Format string

const (
	FormatText        Format = "text"
	FormatJSON        Format = "json"
	FormatSARIF       Format = "sarif"
	FormatAnnotations Format = "annotations"
)

// Host names the CI the annotations writer renders for. It is a flag, never
// detected from the environment: a run on a laptop renders what the run in CI
// rendered, and two machines never print one verdict two ways without saying so.
type Host string

const (
	HostNone      Host = ""
	HostBuildkite Host = "buildkite"
	HostGitHub    Host = "github"
)

// Formats is every format `check -format` accepts, in the order --help lists them.
var Formats = []Format{FormatText, FormatJSON, FormatSARIF, FormatAnnotations}

// ParseFormat accepts the names in Formats and nothing else.
func ParseFormat(s string) (Format, error) {
	for _, f := range Formats {
		if string(f) == s {
			return f, nil
		}
	}
	names := make([]string, len(Formats))
	for i, f := range Formats {
		names[i] = string(f)
	}
	return "", fmt.Errorf("unknown format %q: expected one of %s", s, strings.Join(names, ", "))
}

// ParseHost accepts buildkite, github, or nothing.
func ParseHost(s string) (Host, error) {
	switch Host(s) {
	case HostNone, HostBuildkite, HostGitHub:
		return Host(s), nil
	}
	return "", fmt.Errorf("unknown host %q: expected buildkite or github", s)
}

// Output is everything a writer needs besides the verdict itself: which format
// to render, and the two things only some formats read — the CI host the
// annotations writer targets, and the tool identity the SARIF driver block is
// attributed to. The zero value renders text as enola.
type Output struct {
	Format Format
	Host   Host
	Link   string
	// Tool attributes a SARIF document to the binary that produced it. Ignored
	// by every other format. Zero means enola; see Tool.
	Tool Tool
}

// Write renders the verdict in the named format. Text is Render, JSON is JSON;
// the two host writers are the new rows. Annotations need a host, because
// without one there is no markup to write and an empty document would read
// as "no findings".
func (v Verdict) Write(o Output) ([]byte, error) {
	switch o.Format {
	case FormatText:
		return []byte(v.Render()), nil
	case FormatJSON:
		return v.JSON()
	case FormatSARIF:
		return v.SARIF(o.Tool)
	case FormatAnnotations:
		if o.Host == HostNone {
			return nil, fmt.Errorf("-format annotations needs -host buildkite or -host github")
		}
		return v.Annotations(o.Host, o.Link)
	}
	return nil, fmt.Errorf("unknown format %q", o.Format)
}

// bucket is one named section of the verdict, with the level a host should
// show its findings at. The order is the order the text verdict reads them in:
// what failed, what was reported, what was excused, what went away.
type bucket struct {
	name     string
	level    string
	findings []facts.Insight
}

const (
	levelError   = "error"
	levelWarning = "warning"
	levelNote    = "note"
	levelNone    = "none"
)

func (v Verdict) buckets() []bucket {
	return []bucket{
		{"failure", levelError, v.Failures},
		{"advisory", levelWarning, v.Advisories},
		{"declared", levelWarning, v.Declared},
		{"descriptive", levelNote, v.Descriptive},
		{"incidental", levelNote, v.Incidental},
		{"suppressed", levelNote, v.Suppressed},
		{"exempted", levelNote, v.Exempted},
		{"silenced", levelNote, v.Silenced},
		{"undeclared", levelNote, v.Undeclared},
		{"unattributed", levelNote, v.Unattributed},
		{"resolved", levelNone, v.Resolved},
	}
}

var (
	ruleTitle     = regexp.MustCompile(`^(?:Strict constraint|Advisory constraint|Constraint|Exempted from constraint) (\S+?):? `)
	becauseSuffix = regexp.MustCompile(`(?:^|\s)(?:Rule because|Because): (.+)$`)
)

// ruleOf is the rule a finding reports under: the declared constraint's id
// when the finding is a constraint verdict, the explainer's name otherwise.
// A declared rule is read from the evidence the explainer stamps, then from
// the title, never guessed from the description.
func ruleOf(in facts.Insight) string {
	for _, ev := range in.Evidence {
		if strings.HasPrefix(ev.Fact, "rule: ") {
			return strings.TrimPrefix(ev.Fact, "rule: ")
		}
	}
	if m := ruleTitle.FindStringSubmatch(in.Title); m != nil {
		return m[1]
	}
	if in.Source == "" {
		return "unknown"
	}
	return in.Source
}

// reasonOf is the `because` a declared rule carries, which the constraints
// explainer appends to every verdict's description. Findings from other
// explainers have no reason the team wrote, so their description stands in.
func reasonOf(in facts.Insight) string {
	if m := becauseSuffix.FindStringSubmatch(in.Description); m != nil {
		return strings.TrimSpace(m[1])
	}
	return oneLine(in.Description)
}

// excuseOf is the ledger entry or exemption that kept a finding out of the
// gate, for the two buckets that have one. The suppression is re-matched from
// the policy the verdict recorded, so the excuse named is the one that applied.
func (v Verdict) excuseOf(name string, in facts.Insight) string {
	switch name {
	case "suppressed":
		for _, s := range v.Policy.Suppressions {
			if s.suppresses(in) {
				return fmt.Sprintf("suppressed by %s on %s: %s", s.Owner, s.Date, s.Reason)
			}
		}
		return "suppressed by the ledger"
	case "exempted":
		for _, ev := range in.Evidence {
			if strings.HasPrefix(ev.Fact, "rule: ") && strings.HasPrefix(ev.Detail, "exempted by ") {
				return ev.Detail
			}
		}
		return "exempted by the declaration"
	}
	return ""
}

// position is the first evidence with a measured line. A finding that cites
// several files is placed at the first one the explainer listed, which is the
// witness the text verdict frames too.
func position(in facts.Insight) (facts.Evidence, bool) {
	for _, ev := range in.Evidence {
		if ev.Line > 0 && ev.File != "" {
			return ev, true
		}
	}
	return facts.Evidence{}, false
}

func actionOf(in facts.Insight) string {
	if len(in.Actions) == 0 {
		return ""
	}
	return in.Actions[0]
}

// policyOf says what the policy did with a finding's explainer: fail, warn
// (a fail-on explainer under --warn-only), or report.
func (v Verdict) policyOf(in facts.Insight) string {
	enforced := false
	for _, name := range v.Policy.FailExplainers {
		if name == in.Source {
			enforced = true
		}
	}
	switch {
	case enforced && v.Policy.WarnOnly:
		return "warn"
	case enforced:
		return "fail"
	}
	return "report"
}

// placed is one finding with the bucket it sits in and where it points, the
// unit both host writers iterate. Ordered bucket first, then file and line,
// then identity, so two runs over one verdict print the same document.
type placed struct {
	bucket   bucket
	finding  facts.Insight
	evidence facts.Evidence
	located  bool
	identity string
}

func (v Verdict) placements() []placed {
	var out []placed
	for _, b := range v.buckets() {
		for _, in := range b.findings {
			ev, ok := position(in)
			out = append(out, placed{bucket: b, finding: in, evidence: ev, located: ok, identity: diff.FindingIdentity(in)})
		}
	}
	order := map[string]int{}
	for i, b := range v.buckets() {
		order[b.name] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if order[a.bucket.name] != order[b.bucket.name] {
			return order[a.bucket.name] < order[b.bucket.name]
		}
		if a.evidence.File != b.evidence.File {
			return a.evidence.File < b.evidence.File
		}
		if a.evidence.Line != b.evidence.Line {
			return a.evidence.Line < b.evidence.Line
		}
		return a.identity < b.identity
	})
	return out
}

// legacyHostPath is hostPath for a verdict with no Sources: the fact's file
// with the repository label a union snapshot prefixes removed when the rest
// resolves in the working directory, and the file as recorded otherwise.
func legacyHostPath(file string) string {
	if _, err := os.Stat(filepath.Join(frameRoot, filepath.FromSlash(file))); err == nil { //factpath:host
		return file
	}
	if _, rest, ok := strings.Cut(file, "/"); ok {
		if _, err := os.Stat(filepath.Join(frameRoot, filepath.FromSlash(rest))); err == nil { //factpath:host
			return rest
		}
	}
	return file
}

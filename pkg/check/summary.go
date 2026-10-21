package check

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plain"
)

// Summary is the verdict for a reader who does not speak the vocabulary of the
// full one: whether the change passes, what it changed, what got worse, what
// got better, what is counted as neither, what the run could not see, and what
// to do next.
//
// It is another writer over a finished Verdict and grades nothing: the exit
// code is the verdict's, whichever writer printed it. Every section is filled,
// with a sentence saying so when it has nothing, so a reader cannot mistake a
// section enola had nothing for with one it never looked at.
type Summary struct {
	Repo    string       `json:"repo,omitempty"`
	Result  Result       `json:"result"`
	Changed []plain.Item `json:"changed"`
	// Worse is what the change introduced, what fails the build first, at most
	// plain.MaxItems of it; MoreWorse counts the rest.
	Worse     []plain.Item `json:"worse"`
	MoreWorse int          `json:"more_worse,omitempty"`
	Better    []plain.Item `json:"better"`
	// Neither is what went away or appeared without the code getting better or
	// worse: excused, exempted, or no longer asked about. The full verdict keeps
	// these out of Resolved for the same reason, and so does this.
	Neither     []plain.Item `json:"neither"`
	CouldNotSee []plain.Item `json:"could_not_see"`
	Next        []plain.Item `json:"next"`
}

// Result is the one-word outcome and the sentences behind it.
type Result struct {
	Outcome string       `json:"outcome"`
	Items   []plain.Item `json:"items"`
}

// Outcomes.
const (
	OutcomePasses      = "passes"
	OutcomeFails       = "fails"
	OutcomeNotGraded   = "not graded"
	OutcomeCouldNotRun = "could not run"
)

// Summarize derives the summary of a verdict.
func (v Verdict) Summarize() *Summary {
	s := &Summary{}
	if v.Diff != nil {
		s.Repo = v.Diff.CurrentRepo
	}
	s.Result = v.result()
	s.Changed = v.changed()
	s.Worse, s.MoreWorse = v.worse()
	s.Better = v.better()
	s.Neither = v.neither()
	s.CouldNotSee = v.couldNotSee()
	s.Next = v.next()
	return s
}

func (v Verdict) result() Result {
	fact := func(text string) plain.Item { return plain.Item{Text: text, Certainty: plain.Certain} }
	var items []plain.Item
	partial := v.Status == StatusPartialClean || v.Status == StatusPartialRegression

	var outcome string
	switch v.Status {
	case StatusClean, StatusPartialClean:
		outcome = OutcomePasses
		switch {
		case len(v.Failures) > 0:
			// --warn-only: say what the policy would have done.
			items = append(items, fact("Passes only because --warn-only is set: "+
				plain.Plural(len(v.Failures), "problem", "problems")+" below would otherwise fail the build."))
		case v.changedNothing():
			items = append(items, fact("Passes. The change did not alter the structure of the code."))
		case !v.Policy.Enforcing():
			items = append(items, fact("Passes. Nothing is enforced yet, so this run could not fail the build."))
		default:
			items = append(items, fact("Passes. Nothing this change did breaks what the build enforces"+v.enforcedClause()+"."))
		}
	case StatusRegression, StatusPartialRegression:
		outcome = OutcomeFails
		items = append(items, fact("Fails the build"+v.enforcedClause()+"."))
		for _, b := range v.Breaches {
			if b.Fatal {
				items = append(items, fact(fmt.Sprintf("Over a limit the build enforces: %d %s.", b.Measurement.Count, b.Measurement.Label)))
			}
		}
	case StatusIncomparable:
		outcome = OutcomeNotGraded
		items = append(items, fact("Not graded. The before and after were not produced the same way, "+
			"so comparing them would describe that, not your change. This says nothing about your change."))
		if len(v.ComparabilityWarnings) > 0 {
			text := "Why: " + v.ComparabilityWarnings[0]
			if n := len(v.ComparabilityWarnings) - 1; n > 0 {
				text += " (" + plain.Plural(n, "more reason", "more reasons") + " in the full verdict.)"
			}
			items = append(items, fact(text))
		}
	default:
		outcome = OutcomeCouldNotRun
		items = append(items, fact("The check could not run, so it says nothing about your change."))
	}
	if partial {
		items = append(items, fact("Only the code both snapshots could read was compared: "+
			"enola read some languages differently before and after."))
	}
	return Result{Outcome: outcome, Items: items}
}

// enforcedClause names what the policy enforces, as the end of a sentence.
func (v Verdict) enforcedClause() string {
	ex := v.Policy.failExplainers()
	if len(ex) == 0 {
		return ""
	}
	return " (" + plain.AndList(ex) + ")"
}

func (v Verdict) changedNothing() bool {
	return v.FactsAdded == 0 && v.FactsRemoved == 0 && v.FactsChanged == 0 && v.EdgesAdded == 0 && v.EdgesRemoved == 0
}

// kindWords spells the architectural kinds a change adds or removes as a reader
// would. Dependencies are left out: what they mean, one module using another,
// is said from the import edges instead.
var kindWords = []struct{ kind, one, many string }{
	{facts.KindModule, "folder of code", "folders of code"},
	{facts.KindSymbol, "function or type", "functions and types"},
	{facts.KindRoute, "HTTP endpoint", "HTTP endpoints"},
	{facts.KindStorage, "database use", "database uses"},
	{facts.KindService, "service", "services"},
}

func (v Verdict) changed() []plain.Item {
	if v.Status == StatusIncomparable || v.Status == StatusUsageError {
		return []plain.Item{{Text: "Not shown: without a fair comparison, a list of changes would describe how the two snapshots were made.", Certainty: plain.Certain}}
	}
	var out []plain.Item
	tally := func(verb string, by map[string]int, kind string) {
		var parts []string
		for _, k := range kindWords {
			if n := by[k.kind]; n > 0 {
				parts = append(parts, plain.Plural(n, k.one, k.many))
			}
		}
		if len(parts) == 0 {
			return
		}
		text := verb + " " + plain.AndList(parts)
		if names := v.addedNames(kind); verb == "Added" && len(names) > 0 {
			text += " (" + strings.Join(names, ", ") + ")"
		}
		out = append(out, plain.Item{Text: text + ".", Certainty: plain.Certain})
	}
	tally("Added", v.AddedByKind, facts.KindModule)
	tally("Removed", v.RemovedByKind, "")
	if v.FactsChanged > 0 {
		out = append(out, plain.Item{
			Text:      "Changed the shape of " + plain.Plural(v.FactsChanged, "existing piece of code", "existing pieces of code") + ", such as a signature.",
			Certainty: plain.Certain,
		})
	}
	if v.Diff != nil {
		uses := importPairs(v.Diff.EdgesAdded, v.Diff.FactsAdded)
		drops := importPairs(v.Diff.EdgesRemoved, v.Diff.FactsRemoved)
		for i, p := range uses {
			if i == plain.MaxItems {
				out = append(out, plain.Item{Text: plain.Plural(len(uses)-i, "more new use", "more new uses") + " between modules in the full verdict.", Certainty: plain.Certain})
				break
			}
			out = append(out, plain.Item{Text: p[0] + " now uses " + p[1] + ".", Certainty: plain.Certain})
		}
		for i, p := range drops {
			if i == plain.MaxItems {
				out = append(out, plain.Item{Text: plain.Plural(len(drops)-i, "more removed use", "more removed uses") + " between modules in the full verdict.", Certainty: plain.Certain})
				break
			}
			out = append(out, plain.Item{Text: p[0] + " no longer uses " + p[1] + ".", Certainty: plain.Certain})
		}
	}
	if len(out) == 0 {
		out = append(out, plain.Item{Text: "Nothing in the structure of the code changed.", Certainty: plain.Certain})
	}
	return out
}

// addedNames lists up to plain.MaxItems added facts of one kind by name.
func (v Verdict) addedNames(kind string) []string {
	if v.Diff == nil || kind == "" {
		return nil
	}
	var names []string
	for _, f := range v.Diff.FactsAdded {
		if f.Kind == kind {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	if len(names) > plain.MaxItems {
		names = append(names[:plain.MaxItems], "…")
	}
	return names
}

// importPairs is the distinct module-to-module imports among edges, sorted.
// Only imports between two modules of this code count: a declares edge is
// bookkeeping, one per new symbol, and an import of the standard library or of
// another project is not one part of this code using another.
//
// An import edge belongs to a dependency fact named "<module> -> <import path>",
// and that fact's source prop says whether the import is internal. The facts
// come from the same side of the diff as the edges; an edge whose fact is not
// there is left out rather than guessed at, because whether a target is a
// module of this code is not something its spelling says in every language.
func importPairs(edges []diff.Edge, deps []facts.Fact) [][2]string {
	internal := map[string]bool{}
	for _, f := range deps {
		if f.Kind == facts.KindDependency && f.PropString("source") == "internal" {
			internal[f.Name] = true
		}
	}
	seen := map[[2]string]bool{}
	var out [][2]string
	for _, e := range edges {
		if e.Kind != facts.RelImports || !internal[e.Source] {
			continue
		}
		module, _, ok := strings.Cut(e.Source, " -> ")
		if !ok || module == e.Target {
			continue
		}
		p := [2]string{module, e.Target}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

func (v Verdict) worse() ([]plain.Item, int) {
	var out []plain.Item
	for _, in := range v.Failures {
		it := plainFinding(in)
		if v.Status == StatusRegression || v.Status == StatusPartialRegression {
			it.Text = "Fails the build. " + it.Text
		}
		out = append(out, it)
	}
	advisories := append([]facts.Insight(nil), v.Advisories...)
	sort.SliceStable(advisories, func(i, j int) bool { return advisories[i].Confidence > advisories[j].Confidence })
	for _, in := range advisories {
		out = append(out, plainFinding(in))
	}
	if len(out) == 0 {
		if v.Status == StatusIncomparable || v.Status == StatusUsageError {
			return []plain.Item{{Text: "Not graded.", Certainty: plain.Certain}}, 0
		}
		return []plain.Item{{Text: "Nothing.", Certainty: plain.Certain}}, 0
	}
	if len(out) > plain.MaxItems {
		return out[:plain.MaxItems], len(out) - plain.MaxItems
	}
	return out, 0
}

func (v Verdict) better() []plain.Item {
	if len(v.Resolved) == 0 {
		return []plain.Item{{Text: "Nothing.", Certainty: plain.Certain}}
	}
	counts := map[string]int{}
	var order []string
	for _, in := range v.Resolved {
		key := in.Source
		if _, ok := counts[key]; !ok {
			order = append(order, key)
		}
		counts[key]++
	}
	var out []plain.Item
	for _, k := range order {
		w := nounFor(k)
		out = append(out, plain.Item{Text: "Clears " + plain.Plural(counts[k], w.one, w.many) + ".", Certainty: plain.Certain})
	}
	return out
}

func (v Verdict) neither() []plain.Item {
	var out []plain.Item
	add := func(n int, text string) {
		if n > 0 {
			out = append(out, plain.Item{Text: text, Certainty: plain.Certain})
		}
	}
	add(len(v.Suppressed), plain.Plural(len(v.Suppressed), "problem is", "problems are")+
		" excused in "+SuppressionsFileName+", so they are reported and not counted.")
	add(len(v.Exempted), plain.Plural(len(v.Exempted), "problem is", "problems are")+
		" exempted by the project's own rules.")
	add(len(v.Silenced), plain.Plural(len(v.Silenced), "rule break stopped", "rule breaks stopped")+
		" being reported because the code moved out of what the rule covers. Nothing was fixed.")
	add(len(v.Undeclared), plain.Plural(len(v.Undeclared), "rule break stopped", "rule breaks stopped")+
		" being reported because the rule was changed or removed. The code is the same.")
	add(len(v.Unattributed), plain.Plural(len(v.Unattributed), "rule break", "rule breaks")+
		" could not be compared: the code they name is not in this snapshot.")
	add(len(v.Declared), "A rule this change adds already finds "+
		plain.Plural(len(v.Declared), "break", "breaks")+" in code you did not touch. That is where the rule starts, not something you broke.")
	if len(out) == 0 {
		out = append(out, plain.Item{Text: "Nothing: everything that appeared or went away is counted above.", Certainty: plain.Certain})
	}
	return out
}

func (v Verdict) couldNotSee() []plain.Item {
	var out []plain.Item
	c := v.Census
	switch {
	case c == nil:
	case !c.Recorded:
		out = append(out, plain.Item{Text: "Not recorded: the snapshot is older than this kind of check.", Certainty: plain.Certain})
	default:
		if c.FilesExcludedByIgnore > 0 || c.DirsExcludedByIgnore > 0 {
			var what []string
			if c.FilesExcludedByIgnore > 0 {
				what = append(what, plain.Plural(c.FilesExcludedByIgnore, "file", "files"))
			}
			if c.DirsExcludedByIgnore > 0 {
				what = append(what, plain.Plural(c.DirsExcludedByIgnore, "folder", "folders"))
			}
			out = append(out, plain.Item{
				Text:      plain.AndList(what) + " left out by your ignore settings, so a change there is not seen.",
				Certainty: plain.Certain,
			})
		}
		if c.DeadExemptions > 0 {
			out = append(out, plain.Item{
				Text:      plain.Plural(c.DeadExemptions, "exemption in your rules matches", "exemptions in your rules match") + " nothing, so it excuses nothing.",
				Certainty: plain.Certain,
			})
		}
		if c.UnusedSuppressions > 0 {
			out = append(out, plain.Item{
				Text:      plain.Plural(c.UnusedSuppressions, "excuse in "+SuppressionsFileName+" matches", "excuses in "+SuppressionsFileName+" match") + " no finding any more.",
				Certainty: plain.Certain,
			})
		}
		if n := len(c.ProviderSkips); n > 0 {
			out = append(out, plain.Item{
				Text:      plain.Plural(n, "extra source of facts was", "extra sources of facts were") + " skipped this run; the full verdict says why.",
				Certainty: plain.Certain,
			})
		}
	}
	out = append(out, plain.Item{Text: "enola compares the structure of the code. It does not run it or its tests.", Certainty: plain.Certain})
	return out
}

func (v Verdict) next() []plain.Item {
	full := plain.Item{Subject: "enola check", Text: "The full verdict, with every finding and every change."}
	switch v.Status {
	case StatusIncomparable:
		return []plain.Item{
			{Subject: "enola baseline pin", Text: "Take a new \"before\" with this enola and these settings, then make your next change against it."},
			full,
		}
	case StatusRegression, StatusPartialRegression:
		return []plain.Item{{Subject: "enola check", Text: "Run it again once the problems above are fixed. Without --summary it shows every finding and every change."}}
	}
	if ex := v.unenforced(); len(ex) > 0 && len(v.Policy.failExplainers()) == 0 {
		return []plain.Item{
			{Subject: "enola check --fail-on " + strings.Join(ex, ","), Text: "Make findings like these fail the build."},
			full,
		}
	}
	return []plain.Item{full}
}

// unenforced is the explainers the advisories came from, sorted.
func (v Verdict) unenforced() []string {
	seen := map[string]bool{}
	var out []string
	for _, in := range v.Advisories {
		if in.Source != "" && !seen[in.Source] {
			seen[in.Source] = true
			out = append(out, in.Source)
		}
	}
	sort.Strings(out)
	return out
}

// noun is how a finding of one explainer is counted: "1 dependency loop".
type noun struct{ one, many string }

var findingNouns = map[string]noun{
	"cycles":              {"dependency loop", "dependency loops"},
	"layers":              {"layer rule break", "layer rule breaks"},
	"constraints":         {"rule break", "rule breaks"},
	"dead-code":           {"piece of code that looked unused", "pieces of code that looked unused"},
	"god-class":           {"heavily used hotspot", "heavily used hotspots"},
	"complexity-outliers": {"overly branched function", "overly branched functions"},
	"hotspots":            {"call hotspot", "call hotspots"},
	"dependency-depth":    {"long dependency chain", "long dependency chains"},
	"exported-surface":    {"oversized public surface", "oversized public surfaces"},
	"performance":         {"repeated-work risk", "repeated-work risks"},
	"unused-routes":       {"endpoint nobody called", "endpoints nobody called"},
}

func nounFor(source string) noun {
	if n, ok := findingNouns[source]; ok {
		return n
	}
	if source == "" {
		return noun{"finding", "findings"}
	}
	return noun{source + " finding", source + " findings"}
}

// plainFinding says one finding in plain words. Explainers the summary knows
// get a sentence of their own; any other is its title, which is already a
// sentence, only a technical one.
func plainFinding(in facts.Insight) plain.Item {
	it := plain.Item{Text: in.Title, Certainty: plain.CertaintyOf(in.Confidence)}
	if len(in.Evidence) > 0 && in.Evidence[0].File != "" {
		it.Location = in.Evidence[0].File
		if in.Evidence[0].Line > 0 {
			it.Location += ":" + strconv.Itoa(in.Evidence[0].Line)
		}
	}
	t := in.Title
	switch in.Source {
	case "cycles":
		it.Text = fmt.Sprintf("Creates a loop: %d modules now depend on each other, so none of them can be changed or tested on its own.", firstNumber(t))
	case "layers":
		if from, to, ok := strings.Cut(between(t, "Layer violation:", " ("), " -> "); ok {
			it.Text = fmt.Sprintf("Code in the %q layer now depends on the %q layer, which the layer rules forbid.", from, to)
		}
	case "constraints":
		if rule := between(t, "Constraint ", " violated"); rule != "" && strings.HasPrefix(t, "Constraint ") {
			it.Text = fmt.Sprintf("Breaks the project's rule %q.", rule)
		}
	case "dead-code":
		switch {
		case strings.HasPrefix(t, "Dead code candidate:"):
			kind, name, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(t, "Dead code candidate:")), " ")
			it.Subject, it.Text = name, fmt.Sprintf("This %s looks unused. Check it before deleting it: something may call it by name at runtime.", kind)
		case strings.HasPrefix(t, "Test-only code candidate:"):
			kind, name, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(t, "Test-only code candidate:")), " ")
			it.Subject, it.Text = name, fmt.Sprintf("Only tests use this %s.", kind)
		case strings.HasPrefix(t, "Additional dead-code candidates:"):
			it.Text = plain.Plural(firstNumber(t), "more piece of code looks", "more pieces of code look") + " unused."
		}
	case "god-class":
		it.Subject = between(t, "High fan-in symbol:", " (")
		it.Text = fmt.Sprintf("Now used from %s. A bug or a change here shows up in many places at once.", plain.Plural(firstNumber(t), "place", "places"))
	case "complexity-outliers":
		it.Subject = between(t, "High cyclomatic complexity:", " (")
		it.Text = fmt.Sprintf("Has %d separate paths through it, so it is hard to test every one.", firstNumber(t))
	case "hotspots":
		it.Subject = between(t, "Call-graph hotspot:", " (")
		it.Text = "Many functions call it and it calls many, so a change to it ripples both ways."
	case "dependency-depth":
		it.Subject = between(t, "Deep dependency chain:", " (")
		it.Text = fmt.Sprintf("Sits at the end of a chain of %d modules, each depending on the next.", firstNumber(t))
	case "exported-surface":
		it.Subject = between(t, "Large public surface:", " exports")
		it.Text = "Exposes most of its code to the rest of the program, so it is hard to change without breaking a caller."
	case "performance":
		if i := strings.Index(t, "): "); i >= 0 && strings.HasPrefix(t, "Performance risk") {
			it.Subject = t[i+3:]
			it.Text = "Repeats work inside a loop. enola reads the code and does not measure it, so this only matters if the input grows large."
		}
	case "unused-routes":
		if strings.HasPrefix(t, "Unused endpoint candidates:") {
			it.Text = "Some endpoints have no caller among the clients analysed: " + strings.TrimSpace(strings.TrimPrefix(t, "Unused endpoint candidates:")) + "."
		}
	}
	return it
}

// between is the text after prefix up to stop, trimmed; "" when prefix is absent.
func between(s, prefix, stop string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	s = s[i+len(prefix):]
	if j := strings.Index(s, stop); j >= 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// firstNumber is the first integer in a title's parentheses, where explainers
// put their metric and where a digit in a name ("Base64") cannot be mistaken
// for it, or else the first after its colon, or 0.
func firstNumber(s string) int {
	if i := strings.IndexByte(s, '('); i >= 0 {
		if n, ok := numberIn(s[i:]); ok {
			return n
		}
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		if n, ok := numberIn(s[i:]); ok {
			return n
		}
	}
	return 0
}

// numberIn is the first run of digits in s.
func numberIn(s string) (int, bool) {
	start := -1
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if start < 0 {
				start = i
			}
		case start >= 0:
			n, _ := strconv.Atoi(s[start:i])
			return n, true
		}
	}
	if start >= 0 {
		n, _ := strconv.Atoi(s[start:])
		return n, true
	}
	return 0, false
}

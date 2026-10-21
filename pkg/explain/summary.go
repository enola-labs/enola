package explain

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/flows"
)

// Summary is the report for a reader who does not speak the vocabulary of the
// full one: what the repository is, whether its structure holds, where to start
// reading, what is risky to change, what is worth a look, and how far to trust
// all of it.
//
// It is derived by fixed rules, so the same repository always reads the same.
// Every section is filled, with a sentence saying so when nothing was found,
// because a reader cannot tell a missing section from a section enola never
// looked at. The wording lives here rather than in the renderer so the JSON form
// says exactly what the text form says.
type Summary struct {
	RepoPath string `json:"repo_path"`
	About    []Item `json:"about"`
	Health   Health `json:"health"`
	// StartHere is where the program begins and what most of it builds on.
	StartHere []Item `json:"start_here"`
	// Careful names the modules a change travels furthest from.
	Careful []Item `json:"careful"`
	// WorthALook is the most certain findings first, at most maxItems of them.
	WorthALook []Item `json:"worth_a_look"`
	// MoreFindings counts the kinds of finding WorthALook had no room for.
	MoreFindings int `json:"more_findings,omitempty"`
	// Caveats is what the reader should not take at face value.
	Caveats []Item `json:"caveats"`
	Next    []Item `json:"next"`
}

// Health is the one-word verdict on the structure and the reasons for it.
type Health struct {
	Verdict string `json:"verdict"`
	Items   []Item `json:"items"`
}

// Item is one plain-language statement, optionally about a named module or
// symbol and with the command that shows the detail behind it.
type Item struct {
	Subject   string    `json:"subject,omitempty"`
	Location  string    `json:"location,omitempty"`
	Text      string    `json:"text"`
	Certainty Certainty `json:"certainty,omitempty"`
	Next      string    `json:"next,omitempty"`
}

// Certainty is how sure enola is of a statement, in words rather than a score.
type Certainty string

const (
	Certain Certainty = "certain"
	Likely  Certainty = "likely"
	Guess   Certainty = "a guess"
)

// Health verdicts.
const (
	VerdictHealthy   = "healthy"
	VerdictAttention = "needs attention"
	VerdictUnknown   = "unknown"
)

// maxItems caps every list in the summary. Three is what a reader new to the
// repository can act on; the full report holds the rest.
const maxItems = 3

// maxEntries is how many of the program's entry points StartHere names, leaving
// room for the module the code builds on.
const maxEntries = 2

// careFloor and careShare set when a module's reach makes it risky to change:
// it must reach at least careFloor modules and at least careShare of all of
// them. A fixed bar alone flags half of a large repository and nothing in a
// small one.
const (
	careFloor = 5
	careShare = 0.10
)

// Summarize derives the summary of a report. The store is read once more for
// the program's entry points, which the full report does not carry.
func Summarize(src Source, r *Report) *Summary {
	return summarize(r, entryPoints(flows.Analyze(src.Store(), flows.Options{})))
}

// entryPoint is one way into the program, read off a flow.
type entryPoint struct {
	name, file, mechanism, route string
	line, reach, modules         int
}

// entryPoints keeps the flows that are the program itself: not a side entry such
// as an example or a build script, and not one nested inside a larger flow,
// which would name the same code twice.
func entryPoints(a *flows.Analysis) []entryPoint {
	var out []entryPoint
	for _, f := range a.Flows {
		if f.Side || len(f.NestedIn) > 0 {
			continue
		}
		e := entryPoint{
			name: f.Entry.Name, file: f.Entry.File, line: f.Entry.Line,
			mechanism: f.Mechanism, reach: f.Metrics.Reach, modules: f.Metrics.Modules,
		}
		if f.Mechanism == flows.MechanismRoute {
			e.route = f.Name
		}
		out = append(out, e)
		if len(out) == maxEntries {
			break
		}
	}
	return out
}

func summarize(r *Report, entries []entryPoint) *Summary {
	s := &Summary{RepoPath: r.RepoPath}
	s.About = about(r)
	s.Health = health(r)
	s.StartHere = startHere(r, entries)
	s.Careful = careful(r)
	s.WorthALook, s.MoreFindings = worthALook(r)
	s.Caveats = caveats(r)
	s.Next = next(r)
	return s
}

func about(r *Report) []Item {
	var out []Item
	langs := r.Languages
	if len(langs) == 0 {
		langs = r.Extractors
	}
	if len(langs) > 0 {
		text := "Written in " + langName(langs[0]) + "."
		if len(langs) > 1 {
			rest := make([]string, 0, len(langs)-1)
			for _, l := range langs[1:] {
				rest = append(rest, langName(l))
			}
			text = "Written mainly in " + langName(langs[0]) + ", also " + andList(rest) + "."
		}
		out = append(out, Item{Text: text, Certainty: Certain})
	}

	out = append(out, Item{
		Text: fmt.Sprintf("%s, holding %s such as functions and types.",
			plural(r.Modules, "module (a folder of code)", "modules (folders of code)"),
			plural(codeSymbols(r), "named piece of code", "named pieces of code")),
		Certainty: Certain,
	})

	var surface []string
	switch {
	case r.RoutesServed > 0:
		surface = append(surface, "Serves "+plural(r.RoutesServed, "HTTP endpoint", "HTTP endpoints"))
	default:
		surface = append(surface, "Serves no HTTP endpoint enola could find")
	}
	if r.RoutesOutbound > 0 {
		surface = append(surface, "calls "+plural(r.RoutesOutbound, "outside one", "outside ones"))
	}
	text := strings.Join(surface, " and ") + "."
	if r.Storage > 0 {
		text += " Uses " + plural(r.Storage, "database table or store", "database tables or stores") + "."
	} else {
		text += " enola found no database tables."
	}
	out = append(out, Item{Text: text, Certainty: Certain})

	out = append(out, layout(r))

	if r.CrossRepoEdges > 0 {
		out = append(out, Item{
			Text:      "Linked to the other repositories analysed with it by " + plural(r.CrossRepoEdges, "call or import", "calls or imports") + ".",
			Certainty: Certain,
		})
	}
	return out
}

// layout says how the code is organised. A declared layer order is stated as
// such: it is the project's own rule, not enola's reading of its folders.
func layout(r *Report) Item {
	if declared(r) {
		return Item{Text: "The project declares its own layer order, and enola checks the code against it.", Certainty: Certain}
	}
	best, conf := r.Architecture, r.ArchConfidence
	if best == "" {
		return Item{Text: "enola recognised no common way of organising the folders.", Certainty: Likely}
	}
	return Item{
		Text:      fmt.Sprintf("The folders follow the common %q layout.", best),
		Certainty: certaintyOf(conf),
	}
}

func declared(r *Report) bool {
	for _, a := range r.Architectures {
		if strings.HasPrefix(a.Pattern, "declared") {
			return true
		}
	}
	return strings.HasPrefix(r.Architecture, "declared")
}

func health(r *Report) Health {
	var items []Item
	if r.Cycles > 0 {
		items = append(items, Item{
			Text: plural(r.Cycles, "circular dependency", "circular dependencies") +
				": modules that depend on each other in a loop, so none of them can be changed or tested on its own.",
			Certainty: Certain,
		})
	}
	if r.LayerViolations > 0 {
		c := Likely
		if declared(r) {
			c = Certain
		}
		items = append(items, Item{
			Text: plural(r.LayerViolations, "place where", "places where") +
				" code depends on a layer it should not, for example low-level code reaching up into the parts that use it.",
			Certainty: c,
		})
	}
	if len(items) > 0 {
		return Health{Verdict: VerdictAttention, Items: items}
	}

	// Nothing found is only good news when enola could see the dependencies.
	if r.CouplingUnresolved {
		return Health{Verdict: VerdictUnknown, Items: []Item{{
			Text:      "enola could not follow the imports between modules here, so it cannot judge the structure.",
			Certainty: Certain,
		}}}
	}
	text := "No circular dependencies, and no layer rule broken."
	if r.Architecture == "" && len(r.Architectures) == 0 {
		text = "No circular dependencies. No layer order was recognised, so layer rules could not be checked."
	}
	return Health{Verdict: VerdictHealthy, Items: []Item{{Text: text, Certainty: Certain}}}
}

func startHere(r *Report, entries []entryPoint) []Item {
	var out []Item
	for _, e := range entries {
		it := Item{
			Subject:   e.name,
			Text:      entryText(e),
			Certainty: Certain,
		}
		if e.file != "" {
			it.Location = e.file
			if e.line > 0 {
				it.Location = fmt.Sprintf("%s:%d", e.file, e.line)
			}
		}
		out = append(out, it)
	}
	if len(out) == 0 {
		out = append(out, Item{
			Text:      "enola found no clear starting point: no main function, served endpoint or command.",
			Certainty: Likely,
		})
	}

	// The module the most others import is what the rest of the code builds on.
	var core *Hotspot
	for i := range r.Hotspots {
		h := &r.Hotspots[i]
		if h.Generated || h.FanIn == 0 {
			continue
		}
		if core == nil || h.FanIn > core.FanIn || (h.FanIn == core.FanIn && h.Module < core.Module) {
			core = h
		}
	}
	if core != nil && len(out) < maxItems {
		out = append(out, Item{
			Subject:   core.Module,
			Text:      fmt.Sprintf("The core: imported %s across the code, so most of it builds on this.", plural(core.FanIn, "time", "times")),
			Certainty: Certain,
		})
	}
	return out
}

func entryText(e entryPoint) string {
	var what string
	switch e.mechanism {
	case flows.MechanismMain:
		what = "The program starts here"
	case flows.MechanismRoute:
		what = "Handles the HTTP request " + e.route
	case flows.MechanismCommand:
		what = "Runs one of the program's commands"
	case flows.MechanismTool:
		what = "Answers one of the tools the program offers to AI agents"
	case flows.MechanismApp:
		what = "The platform starts the app here"
	default:
		what = fmt.Sprintf("The framework calls this (%s)", e.mechanism)
	}
	return fmt.Sprintf("%s. From here the code reaches %s in %s.",
		what, plural(e.reach, "function", "functions"), plural(e.modules, "module", "modules"))
}

func careful(r *Report) []Item {
	bar := int(float64(r.Modules) * careShare)
	if bar < careFloor {
		bar = careFloor
	}
	var risky []Hotspot
	for _, h := range r.Hotspots {
		if !h.Generated && h.BlastRadius >= bar {
			risky = append(risky, h)
		}
	}
	sort.SliceStable(risky, func(i, j int) bool {
		if risky[i].BlastRadius != risky[j].BlastRadius {
			return risky[i].BlastRadius > risky[j].BlastRadius
		}
		return risky[i].Module < risky[j].Module
	})
	var out []Item
	for _, h := range risky {
		if len(out) == maxItems {
			break
		}
		out = append(out, Item{
			Subject: h.Module,
			Text: fmt.Sprintf("A change here can reach %d of the %d modules within three import steps. Have tests in place before changing it.",
				h.BlastRadius, r.Modules),
			Certainty: Certain,
			Next:      "enola impact " + h.Module,
		})
	}
	if len(out) == 0 {
		out = append(out, Item{Text: "No module stands out: a change in one part of the code stays fairly local.", Certainty: Likely})
	}
	return out
}

// worthALook ranks the findings a newcomer can act on, most certain first, and
// returns how many did not fit.
func worthALook(r *Report) ([]Item, int) {
	var cands []Item
	if d := r.DeadCode; d != nil && d.High > 0 {
		cands = append(cands, Item{
			Text: plural(d.High, "function looks", "functions look") +
				" unused. Check each one before deleting it: code outside this repository, or code called by name at runtime, may still use it.",
			Certainty: Likely,
		})
	}
	if g, it := firstSource(r, "god-class"); it != nil {
		cands = append(cands, Item{
			Subject:   it.Name,
			Text:      fmt.Sprintf("Used from %s. A bug or a change here shows up in many places at once.", plural(firstInt(it.Detail), "place", "places")),
			Certainty: certaintyOf(g.Confidence),
		})
	}
	if g, it := firstSource(r, "complexity-outliers"); it != nil {
		cands = append(cands, Item{
			Subject:   it.Name,
			Text:      fmt.Sprintf("Has %d separate paths through it, far more than the rest of the code, so it is hard to test every one.", firstInt(it.Detail)),
			Certainty: certaintyOf(g.Confidence),
		})
	}
	if p := r.Performance; p != nil && p.High > 0 {
		cands = append(cands, Item{
			Text: plural(p.High, "place repeats", "places repeat") +
				" work inside a loop. enola reads the code and does not measure it, so these only matter if the input they loop over grows large.",
			Certainty: Guess,
		})
	}

	sort.SliceStable(cands, func(i, j int) bool { return certaintyRank(cands[i].Certainty) < certaintyRank(cands[j].Certainty) })
	if len(cands) == 0 {
		return []Item{{Text: "Nothing stands out.", Certainty: Likely}}, 0
	}
	if len(cands) > maxItems {
		return cands[:maxItems], len(cands) - maxItems
	}
	return cands, 0
}

// firstSource returns a code-health group and its highest-ranked offender that
// is not generated code, or nil when there is none.
func firstSource(r *Report, source string) (*FindingGroup, *RankedItem) {
	for i := range r.CodeHealth {
		g := &r.CodeHealth[i]
		if g.Source != source {
			continue
		}
		for j := range g.Top {
			if !g.Top[j].Generated {
				return g, &g.Top[j]
			}
		}
	}
	return nil, nil
}

func caveats(r *Report) []Item {
	var out []Item
	if n := generatedRanked(r); n > 0 {
		out = append(out, Item{
			Text: plural(n, "top-ranked finding is", "top-ranked findings are") +
				" in generated or vendored code, which nobody edits by hand, so this summary leaves them out.",
			Certainty: Certain,
		})
	}
	if v := r.Vendored; v != nil && v.Count > 0 {
		out = append(out, Item{
			Text: plural(v.Count, "directory looks", "directories look") +
				" like a copy of another project. Findings there are not this project's to fix; list them under ignore: in the enola config.",
			Certainty: Guess,
		})
	}
	if r.CouplingUnresolved {
		out = append(out, Item{Text: "Imports could not be matched to modules, so the dependency numbers are missing, not zero.", Certainty: Certain})
	}
	if r.RouteGapFramework != "" {
		out = append(out, Item{
			Text:      r.RouteGapFramework + " is a dependency, but no endpoint was found. enola may not read the way this project registers them.",
			Certainty: Likely,
		})
	}
	if d := r.DeadCode; d != nil && d.High > 0 {
		out = append(out, Item{
			Text:      "Unused code is found by matching names. Code reached through reflection, configuration or another repository can look unused when it is not.",
			Certainty: Certain,
		})
	}
	out = append(out, Item{Text: "enola reads the code and never runs it: it reports structure, not behaviour or speed.", Certainty: Certain})
	return out
}

// generatedRanked counts the generated or vendored entries in the rankings the
// summary reads from, the ones it skipped.
func generatedRanked(r *Report) int {
	n := 0
	for _, h := range r.Hotspots {
		if h.Generated {
			n++
		}
	}
	for _, g := range r.CodeHealth {
		for _, it := range g.Top {
			if it.Generated {
				n++
			}
		}
	}
	return n
}

func next(r *Report) []Item {
	repo := r.RepoPath
	if repo == "" {
		repo = "."
	}
	return []Item{
		{Subject: "enola --explain " + repo, Text: "Every number behind this summary."},
		{Subject: "enola flows " + repo, Text: "All the ways into the program, and how much of it each one reaches."},
	}
}

func certaintyOf(confidence float64) Certainty {
	switch {
	case confidence >= 0.9:
		return Certain
	case confidence >= 0.6:
		return Likely
	default:
		return Guess
	}
}

func certaintyRank(c Certainty) int {
	switch c {
	case Certain:
		return 0
	case Likely:
		return 1
	default:
		return 2
	}
}

// codeSymbols counts the named pieces of code, leaving out the sections and
// documents the markdown extractor reads, which are prose.
func codeSymbols(r *Report) int {
	n := 0
	for _, sk := range r.SymbolKinds {
		if sk.Label == facts.SymbolSection || sk.Label == facts.SymbolDocument {
			continue
		}
		n += sk.Count
	}
	return n
}

// firstInt is the first integer in s, or 0.
func firstInt(s string) int {
	if ints := allInts(s); len(ints) > 0 {
		return ints[0]
	}
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// andList joins words as English does: "a", "a and b", "a, b and c".
func andList(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// langNames spells the language keys the extractors write as people write them.
// A key not listed is printed as it is.
var langNames = map[string]string{
	"go": "Go", "c": "C", "cpp": "C++", "typescript": "TypeScript", "javascript": "JavaScript",
	"ruby": "Ruby", "python": "Python", "java": "Java", "kotlin": "Kotlin", "scala": "Scala",
	"dart": "Dart", "php": "PHP", "swift": "Swift", "rust": "Rust", "csharp": "C#",
	"vbnet": "VB.NET", "fsharp": "F#", "hcl": "Terraform", "proto": "Protobuf", "graphql": "GraphQL",
}

func langName(key string) string {
	if n, ok := langNames[key]; ok {
		return n
	}
	return key
}

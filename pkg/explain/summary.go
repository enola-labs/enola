package explain

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/flows"
	"github.com/enola-labs/enola/pkg/plain"
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
	RepoPath string       `json:"repo_path"`
	About    []plain.Item `json:"about"`
	Health   Health       `json:"health"`
	// StartHere is where the program begins and what most of it builds on.
	StartHere []plain.Item `json:"start_here"`
	// Careful names the modules a change travels furthest from.
	Careful []plain.Item `json:"careful"`
	// WorthALook is the most certain findings first, at most plain.MaxItems of them.
	WorthALook []plain.Item `json:"worth_a_look"`
	// MoreFindings counts the kinds of finding WorthALook had no room for.
	MoreFindings int `json:"more_findings,omitempty"`
	// Caveats is what the reader should not take at face value.
	Caveats []plain.Item `json:"caveats"`
	Next    []plain.Item `json:"next"`
}

// Health is the one-word verdict on the structure and the reasons for it.
type Health struct {
	Verdict string       `json:"verdict"`
	Items   []plain.Item `json:"items"`
}

// Health verdicts.
const (
	VerdictHealthy   = "healthy"
	VerdictAttention = "needs attention"
	VerdictUnknown   = "unknown"
)

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
			// A symbol of the root package is named after module ".", "..main";
			// the file beside it says where it is.
			name: strings.TrimPrefix(f.Entry.Name, ".."), file: f.Entry.File, line: f.Entry.Line,
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

func about(r *Report) []plain.Item {
	var out []plain.Item
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
			text = "Written mainly in " + langName(langs[0]) + ", also " + plain.AndList(rest) + "."
		}
		out = append(out, plain.Item{Text: text, Certainty: plain.Certain})
	}

	out = append(out, plain.Item{
		Text: fmt.Sprintf("%s, holding %s such as functions and types.",
			plain.Plural(r.Modules, "module (a folder of code)", "modules (folders of code)"),
			plain.Plural(codeSymbols(r), "named piece of code", "named pieces of code")),
		Certainty: plain.Certain,
	})

	var surface []string
	switch {
	case r.RoutesServed > 0:
		surface = append(surface, "Serves "+plain.Plural(r.RoutesServed, "HTTP endpoint", "HTTP endpoints"))
	default:
		surface = append(surface, "Serves no HTTP endpoint enola could find")
	}
	if r.RoutesOutbound > 0 {
		surface = append(surface, "calls "+plain.Plural(r.RoutesOutbound, "outside one", "outside ones"))
	}
	text := strings.Join(surface, " and ") + "."
	switch {
	case r.StorageDistinct > 0:
		text += " Works with " + plain.Plural(r.StorageDistinct, "data store", "distinct data stores") + ", such as database tables."
	case r.StorageReferences > 0:
		text += " Queries a database whose tables are defined somewhere else."
	default:
		text += " enola found no database or other data store."
	}
	out = append(out, plain.Item{Text: text, Certainty: plain.Certain})

	out = append(out, layout(r))

	if r.CrossRepoEdges > 0 {
		out = append(out, plain.Item{
			Text:      "Linked to the other repositories analysed with it by " + plain.Plural(r.CrossRepoEdges, "call or import", "calls or imports") + ".",
			Certainty: plain.Certain,
		})
	}
	return out
}

// layout says how the code is organised. A declared layer order is stated as
// such: it is the project's own rule, not enola's reading of its folders.
func layout(r *Report) plain.Item {
	if declared(r) {
		where := ""
		if len(r.LayerFiles) > 0 {
			where = ", in " + plain.AndList(r.LayerFiles)
		}
		layers := ""
		if r.DeclaredLayers > 0 {
			layers = " (" + plain.Plural(r.DeclaredLayers, "layer", "layers") + where + ")"
		} else if where != "" {
			layers = " (" + strings.TrimPrefix(where, ", ") + ")"
		}
		return plain.Item{
			Text:      "The project writes down which parts of its code may use which" + layers + ", and enola checks every import against it.",
			Certainty: plain.Certain,
		}
	}
	best, conf := r.Architecture, r.ArchConfidence
	if best == "" {
		return plain.Item{Text: "enola recognised no common way of organising the folders.", Certainty: plain.Likely}
	}
	return plain.Item{
		Text:      fmt.Sprintf("The folders follow the common %q layout.", best),
		Certainty: plain.CertaintyOf(conf),
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
	var items []plain.Item
	if r.Cycles > 0 {
		items = append(items, plain.Item{
			Text: plain.Plural(r.Cycles, "circular dependency", "circular dependencies") +
				": modules that depend on each other in a loop, so none of them can be changed or tested on its own.",
			Certainty: plain.Certain,
		})
	}
	if r.LayerViolations > 0 {
		c := plain.Likely
		if declared(r) {
			c = plain.Certain
		}
		items = append(items, plain.Item{
			Text: plain.Plural(r.LayerViolations, "place where", "places where") +
				" code depends on a layer it should not, for example low-level code reaching up into the parts that use it.",
			Certainty: c,
		})
	}
	if len(items) > 0 {
		return Health{Verdict: VerdictAttention, Items: items}
	}

	// Nothing found is only good news when enola could see the dependencies.
	if r.CouplingUnresolved {
		return Health{Verdict: VerdictUnknown, Items: []plain.Item{{
			Text:      "enola could not follow the imports between modules here, so it cannot judge the structure.",
			Certainty: plain.Certain,
		}}}
	}
	text := "No circular dependencies, and no layer rule broken."
	if r.Architecture == "" && len(r.Architectures) == 0 {
		text = "No circular dependencies. No layer order was recognised, so layer rules could not be checked."
	}
	return Health{Verdict: VerdictHealthy, Items: []plain.Item{{Text: text, Certainty: plain.Certain}}}
}

func startHere(r *Report, entries []entryPoint) []plain.Item {
	var out []plain.Item
	for _, e := range entries {
		it := plain.Item{
			Subject:   e.name,
			Text:      entryText(e),
			Certainty: plain.Certain,
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
		out = append(out, plain.Item{
			Text:      "enola found no clear starting point: no main function, served endpoint or command.",
			Certainty: plain.Likely,
		})
	}

	// The module the most others import is what the rest of the code builds on,
	// once it is imported often enough for that to be true.
	var core *Hotspot
	for i := range r.Hotspots {
		h := &r.Hotspots[i]
		if h.Generated || h.FanIn < careFloor {
			continue
		}
		if core == nil || h.FanIn > core.FanIn || (h.FanIn == core.FanIn && h.Module < core.Module) {
			core = h
		}
	}
	if core != nil && len(out) < plain.MaxItems {
		out = append(out, plain.Item{
			Subject:   core.Module,
			Text:      fmt.Sprintf("The most imported module: imported %s, so a lot of the code builds on it.", plain.Plural(core.FanIn, "time", "times")),
			Certainty: plain.Certain,
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
		what, plain.Plural(e.reach, "function", "functions"), plain.Plural(e.modules, "module", "modules"))
}

func careful(r *Report) []plain.Item {
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
	var out []plain.Item
	for _, h := range risky {
		if len(out) == plain.MaxItems {
			break
		}
		// No "of N modules": the reach counts every folder an import comes from,
		// and not every such folder is a module, so the two are not one set.
		out = append(out, plain.Item{
			Subject: h.Module,
			Text: fmt.Sprintf("A change here can reach %s within three import steps. Have tests in place before changing it.",
				plain.Plural(h.BlastRadius, "other folder of code", "other folders of code")),
			Certainty: plain.Certain,
			Next:      "enola impact " + h.Module,
		})
	}
	if len(out) == 0 {
		out = append(out, plain.Item{Text: "No module stands out: a change in one part of the code stays fairly local.", Certainty: plain.Likely})
	}
	return out
}

// worthALook ranks the findings a newcomer can act on, most certain first, and
// returns how many did not fit.
func worthALook(r *Report) ([]plain.Item, int) {
	var cands []plain.Item
	if d := r.DeadCode; d != nil && d.High > 0 {
		check := "Check each one before deleting it"
		if d.High == 1 {
			check = "Check it before deleting it"
		}
		cands = append(cands, plain.Item{
			Text: plain.Plural(d.High, "function looks", "functions look") + " unused. " + check +
				": code outside this repository, or code called by name at runtime, may still use it.",
			Certainty: plain.Likely,
		})
	}
	if g, it := firstSource(r, "god-class"); it != nil {
		cands = append(cands, plain.Item{
			Subject:   it.Name,
			Text:      fmt.Sprintf("Used from %s. A bug or a change here shows up in many places at once.", plain.Plural(firstInt(it.Detail), "place", "places")),
			Certainty: plain.CertaintyOf(g.Confidence),
		})
	}
	if g, it := firstSource(r, "complexity-outliers"); it != nil {
		cands = append(cands, plain.Item{
			Subject:   it.Name,
			Text:      fmt.Sprintf("Has %d separate paths through it, far more than the rest of the code, so it is hard to test every one.", firstInt(it.Detail)),
			Certainty: plain.CertaintyOf(g.Confidence),
		})
	}
	if p := r.Performance; p != nil && p.High > 0 {
		cands = append(cands, plain.Item{
			Text: plain.Plural(p.High, "place repeats", "places repeat") +
				" work inside a loop. enola reads the code and does not measure it, so these only matter if the input they loop over grows large.",
			Certainty: plain.Guess,
		})
	}

	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Certainty.Rank() < cands[j].Certainty.Rank() })
	if len(cands) == 0 {
		return []plain.Item{{Text: "Nothing stands out.", Certainty: plain.Likely}}, 0
	}
	if len(cands) > plain.MaxItems {
		return cands[:plain.MaxItems], len(cands) - plain.MaxItems
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

func caveats(r *Report) []plain.Item {
	var out []plain.Item
	if n := generatedRanked(r); n > 0 {
		out = append(out, plain.Item{
			Text: plain.Plural(n, "top-ranked finding is", "top-ranked findings are") +
				" in generated or vendored code, which nobody edits by hand, so this summary leaves them out.",
			Certainty: plain.Certain,
		})
	}
	if v := r.Vendored; v != nil && v.Count > 0 {
		out = append(out, plain.Item{
			Text: plain.Plural(v.Count, "directory looks", "directories look") +
				" like a copy of another project. Findings there are not this project's to fix; list them under ignore: in the enola config.",
			Certainty: plain.Guess,
		})
	}
	if r.CouplingUnresolved {
		out = append(out, plain.Item{Text: "Imports could not be matched to modules, so the dependency numbers are missing, not zero.", Certainty: plain.Certain})
	}
	if r.RouteGapFramework != "" {
		out = append(out, plain.Item{
			Text:      r.RouteGapFramework + " is a dependency, but no endpoint was found. enola may not read the way this project registers them.",
			Certainty: plain.Likely,
		})
	}
	if d := r.DeadCode; d != nil && d.High > 0 {
		out = append(out, plain.Item{
			Text:      "Unused code is found by matching names. Code reached through reflection, configuration or another repository can look unused when it is not.",
			Certainty: plain.Certain,
		})
	}
	out = append(out, plain.Item{Text: "enola reads the code and never runs it: it reports structure, not behaviour or speed.", Certainty: plain.Certain})
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

func next(r *Report) []plain.Item {
	repo := r.RepoPath
	if repo == "" {
		repo = "."
	}
	return []plain.Item{
		{Subject: "enola --explain " + repo, Text: "Every number behind this summary."},
		{Subject: "enola flows " + repo, Text: "All the ways into the program, and how much of it each one reaches."},
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

package constraints

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// IncomingSampleCap bounds the sources named per incoming group; the count is
// exact, the names are the first three in sorted order.
const IncomingSampleCap = 3

// NoPart is the component name an incoming edge is grouped under when its
// source belongs to no declared component.
const NoPart = "(no part)"

// FileMembership is one component a file's facts belong to, with the selector
// that admitted them and the members it holds there.
type FileMembership struct {
	Component string   `json:"component"`
	Selector  string   `json:"selector"`
	Source    string   `json:"source"`
	Members   []string `json:"members"`
}

// OutgoingEdge is one relation a fact in the file makes.
type OutgoingEdge struct {
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Line   int    `json:"line"`
}

// IncomingGroup is every relation of one kind landing on the file's facts from
// one part: the count, and the first sources by name.
type IncomingGroup struct {
	Kind      string   `json:"kind"`
	Component string   `json:"component"`
	Count     int      `json:"count"`
	Sources   []string `json:"sources"`
}

// Origin names the snapshot an explanation was read from, so a stale answer
// is never read as current.
type Origin struct {
	Commit      string `json:"commit,omitempty"`
	Dirty       bool   `json:"dirty"`
	GeneratedAt string `json:"generated_at,omitempty"`
}

// FileExplanation answers "why does this file belong where it belongs": the
// components whose selectors admit a fact in the file, with the selector
// stated, the edges those facts make, the edges that land on them grouped by
// kind and by the part the source belongs to, and, when asked, the verdicts
// that would change if the file left every part. It reads the same membership
// the evaluator reads, so the sentence and the verdict cannot disagree.
type FileExplanation struct {
	File        string           `json:"file"`
	Origin      *Origin          `json:"origin,omitempty"`
	Facts       []string         `json:"facts"`
	Memberships []FileMembership `json:"memberships"`
	Outgoing    []OutgoingEdge   `json:"outgoing"`
	Incoming    []IncomingGroup  `json:"incoming"`
	Radius      *BlastRadius     `json:"radius,omitempty"`
}

// ExplainFile reads a file's membership and edges off the snapshot.
func ExplainFile(store *facts.Store, file string) FileExplanation {
	out := FileExplanation{File: file, Incoming: []IncomingGroup{}}
	file = strings.TrimPrefix(file, "./")
	inFile := func(f facts.Fact) bool { return factInFile(f, file) }
	defined := map[string]bool{}
	for _, f := range store.All() {
		if f.Kind == facts.KindIntent || !inFile(f) {
			continue
		}
		out.Facts = append(out.Facts, f.Kind+" "+f.Name)
		defined[f.Name] = true
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDeclares {
				continue
			}
			out.Outgoing = append(out.Outgoing, OutgoingEdge{From: f.Name, Kind: rel.Kind, Target: rel.Target, Line: f.Line})
		}
	}
	sort.Strings(out.Facts)
	sort.Slice(out.Outgoing, func(i, j int) bool {
		if out.Outgoing[i].Line != out.Outgoing[j].Line {
			return out.Outgoing[i].Line < out.Outgoing[j].Line
		}
		return out.Outgoing[i].Target < out.Outgoing[j].Target
	})
	components, _ := declarations(store)
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	partsOf := map[string][]string{}
	for _, name := range names {
		c := components[name]
		_, members := resolveMembership(store, c)
		var here []string
		for _, m := range members {
			partsOf[m.Name] = append(partsOf[m.Name], name)
			if inFile(m) {
				here = append(here, m.Name)
			}
		}
		if len(here) == 0 {
			continue
		}
		sort.Strings(here)
		out.Memberships = append(out.Memberships, FileMembership{Component: name, Selector: selectorSummary(c), Source: c.source, Members: here})
	}
	out.Incoming = incomingGroups(store, defined, inFile, partsOf)
	return out
}

// incomingGroups walks every relation in the store once and keeps those that
// land on a name the file defines, grouped by the relation's kind and by each
// part the source belongs to, or by no part.
func incomingGroups(store *facts.Store, defined map[string]bool, inFile func(facts.Fact) bool, partsOf map[string][]string) []IncomingGroup {
	type key struct{ kind, component string }
	counts := map[key]int{}
	sources := map[key]map[string]bool{}
	for _, f := range store.All() {
		if f.Kind == facts.KindIntent || inFile(f) {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDeclares || !defined[rel.Target] {
				continue
			}
			parts := partsOf[f.Name]
			if len(parts) == 0 {
				parts = []string{NoPart}
			}
			for _, part := range parts {
				k := key{rel.Kind, part}
				counts[k]++
				if sources[k] == nil {
					sources[k] = map[string]bool{}
				}
				sources[k][f.Name] = true
			}
		}
	}
	groups := make([]IncomingGroup, 0, len(counts))
	for k, n := range counts {
		names := sortedMemberNames(sources[k])
		if len(names) > IncomingSampleCap {
			names = names[:IncomingSampleCap]
		}
		groups = append(groups, IncomingGroup{Kind: k.kind, Component: k.component, Count: n, Sources: names})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Kind != groups[j].Kind {
			return groups[i].Kind < groups[j].Kind
		}
		return groups[i].Component < groups[j].Component
	})
	return groups
}

func factInFile(f facts.Fact, file string) bool {
	return f.File != "" && (f.File == file || f.IdentityFile() == file)
}

// PartFile is one file a part holds members in, with the member names.
type PartFile struct {
	File    string   `json:"file"`
	Members []string `json:"members"`
}

// PartEdges is the count of edges between the part and one other part.
type PartEdges struct {
	Component string `json:"component"`
	Count     int    `json:"count"`
}

// PartLaw is one declared rule naming the part.
type PartLaw struct {
	Rule      string `json:"rule"`
	Mode      string `json:"mode"`
	Statement string `json:"statement"`
	Because   string `json:"because"`
}

// PartExplanation answers "what is this part, who reaches it and whom does it
// reach": its members by file, the public face its public globs admit, the
// edges in and out by part, and the laws naming it.
type PartExplanation struct {
	Part       string      `json:"part"`
	Origin     *Origin     `json:"origin,omitempty"`
	Selector   string      `json:"selector"`
	Source     string      `json:"source"`
	Files      []PartFile  `json:"files"`
	PublicFace []string    `json:"public_face"`
	FanIn      []PartEdges `json:"fan_in"`
	FanOut     []PartEdges `json:"fan_out"`
	Laws       []PartLaw   `json:"laws"`
}

// ExplainPart reads one declared component off the snapshot. The second
// result is false when no component carries the name.
func ExplainPart(store *facts.Store, part string) (PartExplanation, bool) {
	components, rules := declarations(store)
	c, ok := components[part]
	if !ok {
		return PartExplanation{}, false
	}
	out := PartExplanation{Part: part, Selector: selectorSummary(c), Source: c.source,
		Files: []PartFile{}, PublicFace: []string{}, FanIn: []PartEdges{}, FanOut: []PartEdges{}, Laws: []PartLaw{}}
	names, members := resolveMembership(store, c)
	byFile := map[string][]string{}
	for _, m := range members {
		byFile[m.File] = append(byFile[m.File], m.Name)
		if len(c.public) > 0 && matchConstraintFile(m, c.public) {
			out.PublicFace = append(out.PublicFace, m.Name)
		}
	}
	for file, list := range byFile {
		sort.Strings(list)
		out.Files = append(out.Files, PartFile{File: file, Members: list})
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].File < out.Files[j].File })
	sort.Strings(out.PublicFace)

	partsOf := map[string][]string{}
	for name, other := range components {
		if name == part {
			continue
		}
		_, otherMembers := resolveMembership(store, other)
		for _, m := range otherMembers {
			partsOf[m.Name] = append(partsOf[m.Name], name)
		}
	}
	in, outCounts := map[string]int{}, map[string]int{}
	for _, f := range store.All() {
		if f.Kind == facts.KindIntent {
			continue
		}
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelDeclares {
				continue
			}
			switch {
			case names[f.Name] && !names[rel.Target]:
				for _, p := range partsOrNone(partsOf[rel.Target]) {
					outCounts[p]++
				}
			case !names[f.Name] && names[rel.Target]:
				for _, p := range partsOrNone(partsOf[f.Name]) {
					in[p]++
				}
			}
		}
	}
	out.FanIn = partEdges(in)
	out.FanOut = partEdges(outCounts)
	for _, r := range rules {
		if !r.names(part) {
			continue
		}
		mode := r.mode
		if mode == "" {
			mode = "ratchet"
		}
		out.Laws = append(out.Laws, PartLaw{Rule: r.id, Mode: mode, Statement: r.statement(), Because: r.because})
	}
	sort.Slice(out.Laws, func(i, j int) bool { return out.Laws[i].Rule < out.Laws[j].Rule })
	return out, true
}

func partsOrNone(parts []string) []string {
	if len(parts) == 0 {
		return []string{NoPart}
	}
	return parts
}

func partEdges(counts map[string]int) []PartEdges {
	edges := make([]PartEdges, 0, len(counts))
	for name, n := range counts {
		edges = append(edges, PartEdges{Component: name, Count: n})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Count != edges[j].Count {
			return edges[i].Count > edges[j].Count
		}
		return edges[i].Component < edges[j].Component
	})
	return edges
}

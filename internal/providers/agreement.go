package providers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// PropResolutionAgreement is stamped by the seam on a call relation two
// producers emitted identically at the same file, line and callee. It sits
// beside the surviving producer's own resolution_level, which the seam never
// rewrites: how a producer resolved a name is its evidence, that another
// producer read the same thing is the seam's.
const PropResolutionAgreement = "resolution_agreement"

const AgreementLevel = "agreement"

// Difference causes the seam counts when two producers resolve the same call
// site to different receivers. Nothing is voted on or removed: both relations
// stay exactly as emitted and the count is the record.
const (
	DifferenceAliasResolved     = "alias-resolved"
	DifferenceDiffering         = "differing"
	DifferenceSingletonSpelling = "singleton-spelling"
)

const differenceExamples = 10

// PropResolutionCause is the cause a producer carries on a dependency fact it
// could not resolve, or resolved through an alias; the seam reads the alias
// case to classify a differing receiver.
const PropResolutionCause = "resolution_cause"

// callee splits a call target into the receiver and the method, keeping the
// separator so a singleton call (`Board.find`) is told from an instance call
// (`Board#find`). A target with no receiver is not a pairable call site.
type callee struct {
	receiver, sep, method string
}

func splitCallee(target string) (callee, bool) {
	i := strings.LastIndexAny(target, "#.")
	if i <= 0 || i == len(target)-1 {
		return callee{}, false
	}
	return callee{receiver: target[:i], sep: target[i : i+1], method: target[i+1:]}, true
}

func (c callee) String() string { return c.receiver + c.sep + c.method }

// normaliseReceiver is the one spelling table the seam owns. The engine
// behind one producer spells a singleton class as `<Owner>` and a bare
// built-in the same way; the other producer spells both as the plain constant.
// Nothing about the receiver is guessed: only the notation is unified.
func normaliseReceiver(receiver string) string {
	out := receiver
	for {
		i := strings.Index(out, "<")
		if i < 0 {
			break
		}
		j := strings.Index(out[i:], ">")
		if j < 0 {
			break
		}
		out = out[:i] + out[i+1:i+j] + out[i+j+1:]
	}
	return strings.TrimPrefix(out, "::")
}

type callRef struct {
	provider int
	index    int
	relation int
	raw      callee
	norm     callee
}

// pairAcrossProviders runs after every producer's facts are validated and
// before merge. It pairs call relations by file, line, receiver and method
// across producers, keeps one relation per pair under the first producer in
// name order with its callee spelled in the form that carries the scope, and
// counts the sites where producers differ. A fact that loses every relation
// to pairing is dropped; a fact with other relations keeps them.
func pairAcrossProviders(names []string, kept [][]facts.Fact, records []facts.ProviderRecord) [][]facts.Fact {
	if len(kept) < 2 {
		return kept
	}
	type site struct {
		file, method string
		line         int
	}
	byKey := map[string][]callRef{}
	bySite := map[site][]callRef{}
	aliasAt := map[string]bool{}
	for p, ff := range kept {
		for i, f := range ff {
			if f.Kind == facts.KindDependency && len(f.Relations) == 0 && f.PropAny(PropResolutionCause) == "alias" {
				aliasAt[fmt.Sprintf("%s\x00%d", f.File, f.Line)] = true
			}
			for r, rel := range f.Relations {
				if rel.Kind != facts.RelCalls {
					continue
				}
				raw, ok := splitCallee(rel.Target)
				if !ok {
					continue
				}
				norm := callee{receiver: normaliseReceiver(raw.receiver), sep: raw.sep, method: raw.method}
				ref := callRef{provider: p, index: i, relation: r, raw: raw, norm: norm}
				key := fmt.Sprintf("%s\x00%d\x00%s\x00%s", f.File, f.Line, norm.receiver, norm.method)
				byKey[key] = append(byKey[key], ref)
				s := site{file: f.File, method: norm.method, line: f.Line}
				bySite[s] = append(bySite[s], ref)
			}
		}
	}

	dropped := map[[2]int]map[int]bool{}
	drop := func(ref callRef) {
		k := [2]int{ref.provider, ref.index}
		if dropped[k] == nil {
			dropped[k] = map[int]bool{}
		}
		dropped[k][ref.relation] = true
	}
	paired := map[callRef]bool{}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		refs := byKey[k]
		byProvider := map[int][]callRef{}
		for _, ref := range refs {
			byProvider[ref.provider] = append(byProvider[ref.provider], ref)
		}
		if len(byProvider) < 2 {
			continue
		}
		providers := make([]int, 0, len(byProvider))
		for p := range byProvider {
			providers = append(providers, p)
		}
		sort.Ints(providers)
		// One-to-one in producer order: a line with two calls of one method
		// pairs at most as many times as the smallest producer read.
		rounds := len(byProvider[providers[0]])
		for _, p := range providers {
			if n := len(byProvider[p]); n < rounds {
				rounds = n
			}
		}
		for round := 0; round < rounds; round++ {
			survivor := byProvider[providers[0]][round]
			sep := survivor.norm.sep
			for _, p := range providers[1:] {
				if byProvider[p][round].norm.sep == "." {
					sep = "."
				}
			}
			canonical := callee{receiver: survivor.norm.receiver, sep: sep, method: survivor.norm.method}
			f := &kept[survivor.provider][survivor.index]
			old := f.Relations[survivor.relation].Target
			f.Relations[survivor.relation].Target = canonical.String()
			f.Name = strings.TrimSuffix(f.Name, old) + canonical.String()
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp(PropResolutionAgreement, AgreementLevel)
			for _, p := range providers {
				ref := byProvider[p][round]
				paired[ref] = true
				records[p].Agreed++
				if p != providers[0] {
					drop(ref)
				}
			}
		}
	}

	differences := make([]map[string]*facts.ProviderDifference, len(kept))
	for p := range differences {
		differences[p] = map[string]*facts.ProviderDifference{}
	}
	note := func(p int, cause, example string) {
		d := differences[p][cause]
		if d == nil {
			d = &facts.ProviderDifference{Cause: cause}
			differences[p][cause] = d
		}
		d.Count++
		if len(d.Examples) < differenceExamples {
			d.Examples = append(d.Examples, example)
		}
	}
	sites := make([]site, 0, len(bySite))
	for s := range bySite {
		sites = append(sites, s)
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		if sites[i].line != sites[j].line {
			return sites[i].line < sites[j].line
		}
		return sites[i].method < sites[j].method
	})
	for _, s := range sites {
		var unpaired []callRef
		seen := map[int]bool{}
		for _, ref := range bySite[s] {
			seen[ref.provider] = true
			if !paired[ref] {
				unpaired = append(unpaired, ref)
			}
		}
		if len(seen) < 2 {
			for _, ref := range unpaired {
				records[ref.provider].OneSided++
			}
			continue
		}
		receivers := map[string]bool{}
		for _, ref := range unpaired {
			receivers[ref.norm.receiver] = true
		}
		if len(receivers) < 2 {
			for _, ref := range unpaired {
				records[ref.provider].OneSided++
			}
			continue
		}
		cause := DifferenceDiffering
		if aliasAt[fmt.Sprintf("%s\x00%d", s.file, s.line)] {
			cause = DifferenceAliasResolved
		} else if sameButForMarkers(receivers) {
			cause = DifferenceSingletonSpelling
		}
		example := s.file + ":" + fmt.Sprint(s.line) + " " + describe(names, unpaired)
		for _, ref := range unpaired {
			records[ref.provider].Differing++
			note(ref.provider, cause, example)
		}
	}
	for p := range records {
		causes := make([]string, 0, len(differences[p]))
		for c := range differences[p] {
			causes = append(causes, c)
		}
		sort.Strings(causes)
		for _, c := range causes {
			records[p].Differences = append(records[p].Differences, *differences[p][c])
		}
	}

	for p := range kept {
		out := kept[p][:0]
		for i, f := range kept[p] {
			gone := dropped[[2]int{p, i}]
			if len(gone) == 0 {
				out = append(out, f)
				continue
			}
			var rels []facts.Relation
			for r, rel := range f.Relations {
				if !gone[r] {
					rels = append(rels, rel)
				}
			}
			if len(rels) == 0 {
				continue
			}
			f.Relations = rels
			out = append(out, f)
		}
		kept[p] = out
	}
	return kept
}

// sameButForMarkers reports whether the differing receivers are one spelling
// once every angle bracket is removed: a singleton notation the table did not
// cover, which is the regression this cause exists to make visible.
func sameButForMarkers(receivers map[string]bool) bool {
	var bare string
	for r := range receivers {
		stripped := strings.NewReplacer("<", "", ">", "").Replace(r)
		if bare == "" {
			bare = stripped
			continue
		}
		if stripped != bare {
			return false
		}
	}
	return true
}

func describe(names []string, refs []callRef) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, names[ref.provider]+"="+ref.raw.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

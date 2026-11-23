package swiftextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// A protocol's associated type is satisfied by a type of its name nested in a
// conforming type (`typealias Parameters = NullParameters`, `enum Parameters`). The
// protocol's users reach it through the conformance (`E.Parameters`), so no source
// names it, and it read as unused. For a protocol the repository declares, the
// conformance is recorded as naming its witness: the protocol is the one the
// conformance's name binds to through scope (typeuses.go), and the witness is the
// type of the associated type's name nested in the conforming type. A protocol the
// repository does not declare is read from a table in the dead-code analysis.

// propAssociatedTypes lists, comma-separated and sorted, the associated types a
// Swift protocol declares.
const propAssociatedTypes = "associated_types"

// associatedTypeNames returns the sorted associatedtype names a protocol body declares.
func associatedTypeNames(body *sitter.Node, src []byte) []string {
	if body == nil {
		return nil
	}
	var names []string
	for i := uint(0); i < body.ChildCount(); i++ {
		c := body.Child(i)
		if kindOf(c) != "associatedtype_declaration" {
			continue
		}
		if id := findChildByKind(c, "type_identifier"); id != nil {
			names = append(names, strings.Trim(nodeText(id, src), "`"))
		}
	}
	sort.Strings(names)
	return names
}

// nameAssociatedTypeWitnesses adds a names edge from each conformance (a type, or an
// extension fact) to the nested types that witness the associated types of a
// protocol it conforms to.
func nameAssociatedTypeWitnesses(allFacts []facts.Fact, vis *swiftTypeVisibility, named *swiftNamedTypes) {
	assoc := map[string][]string{} // protocol fact name -> its associated types
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if s, _ := f.PropAny(propAssociatedTypes).(string); s != "" {
			assoc[f.Name] = strings.Split(s, ",")
		}
	}
	if len(assoc) == 0 {
		return
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		sk, _ := f.PropAny("symbol_kind").(string)
		if sk != facts.SymbolStruct && sk != facts.SymbolClass && sk != facts.SymbolExtension {
			continue
		}
		var adds []string
		for _, r := range f.Relations {
			if r.Kind != facts.RelImplements {
				continue
			}
			proto := named.resolve(f, r.Target, vis)
			for _, a := range assoc[proto] {
				if a == "CodingKeys" {
					continue // markSynthesizedCodingKeys
				}
				if w := witnessOf(f, sk, a, named.forFile(f.File)); w != "" {
					adds = append(adds, w)
				}
			}
		}
		for _, w := range adds {
			if !f.HasRelation(facts.RelNames, w) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelNames, Target: w})
			}
		}
	}
}

// witnessOf returns the declared type named assoc nested in the conforming type of
// f, or "". An extension fact (`<dir>.<Base>+<Proto>`) names its type by the last
// component only, so a nested extended type is found by that tail, when one
// declaration of the module ends so.
func witnessOf(f *facts.Fact, kind, assoc string, set *swiftNamedTypeSet) string {
	if kind != facts.SymbolExtension {
		if set.full[f.Name+"."+assoc] {
			return f.Name + "." + assoc
		}
		return ""
	}
	base, _, ok := strings.Cut(f.Name, "+")
	if !ok {
		return ""
	}
	if set.full[base+"."+assoc] {
		return base + "." + assoc
	}
	dir := ""
	for _, r := range f.Relations {
		if r.Kind == facts.RelDeclares {
			dir = r.Target
		}
	}
	short, found := strings.CutPrefix(base, dir+".")
	if dir == "" || !found {
		return ""
	}
	var hits []string
	for _, c := range set.tails["."+short+"."+assoc] {
		if strings.HasPrefix(c, dir+".") {
			hits = append(hits, c)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

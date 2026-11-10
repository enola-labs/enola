// Package hierarchy answers what a type inherits: its bases, its subclasses, and
// which declaration a member name reaches from it.
//
// Extractors see one file at a time, so a call is spelled by what the file shows:
// `c.run()` on a Concrete is `Concrete.run` even when run is declared on Base, in
// another file, and a subclass in a third file says only that it extends `Base`.
// Every question that follows a call through a class tree (which method runs, which
// override a base method reaches) needs the tree, and this is it, built once over
// the assembled facts.
//
// A base is read from a type's implements relations and its superclass prop (an
// extractor writes one or the other). It resolves to a type of the same repository:
// by its exact name, else by its qualified tail, else, for a bare name, by its last
// segment when exactly one type nearest the subclass carries it. A base that names
// no type of the repository (a framework class, a standard library interface) is no
// edge: the tree stops where the repository's own code does.
package hierarchy

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// typeKinds are the symbol kinds that own members and take part in inheritance.
var typeKinds = map[string]bool{
	facts.SymbolClass: true, facts.SymbolStruct: true, facts.SymbolInterface: true,
	facts.SymbolType: true, facts.SymbolEnum: true,
}

// memberKinds are the symbol kinds a type declares as members a call can reach.
var memberKinds = map[string]bool{
	facts.SymbolMethod: true, facts.SymbolGetter: true, facts.SymbolFunc: true,
}

type key = [2]string // (repo, type name)

// Index is the type tree of every repository in a fact set.
type Index struct {
	types    map[key]bool
	abstract map[key]bool
	byShort  map[key][]string // (repo, last segment) -> type names, sorted
	members  map[key]map[string]string
	parents  map[key][]string
	children map[key][]string
	// ancestors memoizes Ancestors, which a walk asks for on every call it follows.
	ancestors map[key][]string
}

// Build reads the type tree out of a fact set.
func Build(fs []facts.Fact) *Index {
	ix := &Index{
		types: map[key]bool{}, abstract: map[key]bool{}, byShort: map[key][]string{},
		members: map[key]map[string]string{}, parents: map[key][]string{}, children: map[key][]string{},
		ancestors: map[key][]string{},
	}
	rawBases := map[key][]string{}
	for i := range fs {
		f := &fs[i]
		if f.Kind != facts.KindSymbol || !typeKinds[f.PropString("symbol_kind")] {
			continue
		}
		k := key{f.Repo, f.Name}
		if !ix.types[k] {
			ix.types[k] = true
			sk := key{f.Repo, lastSegment(f.Name)}
			ix.byShort[sk] = append(ix.byShort[sk], f.Name)
		}
		if f.PropString("symbol_kind") == facts.SymbolInterface || f.PropBool("abstract") {
			ix.abstract[k] = true
		}
		if s := f.PropString("superclass"); s != "" {
			rawBases[k] = appendUnique(rawBases[k], s)
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				rawBases[k] = appendUnique(rawBases[k], r.Target)
			}
		}
	}
	for k := range ix.byShort {
		sort.Strings(ix.byShort[k])
	}
	for i := range fs {
		f := &fs[i]
		if f.Kind != facts.KindSymbol || !memberKinds[f.PropString("symbol_kind")] {
			continue
		}
		owner, member := SplitMember(f.Name)
		k := key{f.Repo, owner}
		if owner == "" || !ix.types[k] {
			continue
		}
		if ix.members[k] == nil {
			ix.members[k] = map[string]string{}
		}
		ix.members[k][member] = f.Name
	}
	for k, raws := range rawBases {
		for _, raw := range raws {
			b := ix.resolveBase(k[0], k[1], raw)
			if b == "" || b == k[1] {
				continue
			}
			ix.parents[k] = appendUnique(ix.parents[k], b)
			bk := key{k[0], b}
			ix.children[bk] = appendUnique(ix.children[bk], k[1])
		}
	}
	for k := range ix.children {
		sort.Strings(ix.children[k])
	}
	return ix
}

// SplitMember splits a qualified member name at its rightmost member separator
// (`::`, `#` or `.`) into its owner and the member's own name. A name with no
// separator, or whose tail is a path, has no owner.
//
// A Ruby instance member keeps its `#` (`Base#run` is Base and "#run"), so it and a
// class method of the same name (`Base.run`) stay two members: `Mailer.notify`
// does not reach `Mailer#notify`. The member is a key to pass back to Lookup,
// Declares and Overriders, not a name to print.
func SplitMember(name string) (owner, member string) {
	at, width := -1, 0
	for _, sep := range [...]string{"::", "#", "."} {
		if i := strings.LastIndex(name, sep); i > at {
			at, width = i, len(sep)
		}
	}
	if at <= 0 || at+width >= len(name) {
		return "", name
	}
	member = name[at+width:]
	if strings.ContainsAny(member, `/\`) {
		return "", name
	}
	if name[at] == '#' {
		member = "#" + member
	}
	return name[:at], member
}

// IsType reports whether a name is a type of the repository.
func (ix *Index) IsType(repo, name string) bool { return ix.types[key{repo, name}] }

// IsAbstract reports whether a type is an interface or an abstract class.
func (ix *Index) IsAbstract(repo, name string) bool { return ix.abstract[key{repo, name}] }

// Declares returns the member a type declares itself under a name.
func (ix *Index) Declares(repo, typ, member string) (string, bool) {
	name, ok := ix.members[key{repo, typ}][member]
	return name, ok
}

// Lookup returns the declaration a member name reaches from a type: the type's own,
// else the nearest ancestor's, breadth first in declared base order.
func (ix *Index) Lookup(repo, typ, member string) (string, bool) {
	if name, ok := ix.Declares(repo, typ, member); ok {
		return name, true
	}
	for _, a := range ix.Ancestors(repo, typ) {
		if name, ok := ix.Declares(repo, a, member); ok {
			return name, true
		}
	}
	return "", false
}

// Members returns the members a type declares itself, sorted.
func (ix *Index) Members(repo, typ string) []string {
	m := ix.members[key{repo, typ}]
	out := make([]string, 0, len(m))
	for _, name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Overriders returns the topmost declarations of a member below a type: down each
// branch of its subtypes, the first that declares the member itself. A subtype of an
// overrider is not listed; it overrides the overrider.
func (ix *Index) Overriders(repo, typ, member string) []string {
	var out []string
	seen := map[string]bool{typ: true}
	queue := []string{typ}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		for _, c := range ix.children[key{repo, t}] {
			if seen[c] {
				continue
			}
			seen[c] = true
			if name, ok := ix.Declares(repo, c, member); ok {
				out = append(out, name)
				continue
			}
			queue = append(queue, c)
		}
	}
	return out
}

// Ancestors returns a type's bases, transitively, nearest first. The result is
// shared: callers must not modify it. An Index is not safe for concurrent use.
func (ix *Index) Ancestors(repo, typ string) []string {
	k := key{repo, typ}
	if a, ok := ix.ancestors[k]; ok {
		return a
	}
	a := ix.closure(repo, typ, ix.parents)
	ix.ancestors[k] = a
	return a
}

// Subsumes reports whether anc is typ or one of its ancestors.
func (ix *Index) Subsumes(repo, anc, typ string) bool {
	if anc == typ {
		return true
	}
	for _, a := range ix.Ancestors(repo, typ) {
		if a == anc {
			return true
		}
	}
	return false
}

func (ix *Index) closure(repo, typ string, edges map[key][]string) []string {
	var out []string
	seen := map[string]bool{typ: true}
	queue := []string{typ}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		for _, n := range edges[key{repo, t}] {
			if seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
			queue = append(queue, n)
		}
	}
	return out
}

// ResolveType names the repository type a spelling refers to, as written in the
// type from: exactly, by its qualified tail, or, bare, by its last segment when one
// type nearest from carries it. "" when no type of the repository does.
func (ix *Index) ResolveType(repo, from, raw string) string {
	return ix.resolveName(repo, from, raw, false)
}

// resolveBase names the repository type a base spelling refers to, or "". A type is
// never its own base.
func (ix *Index) resolveBase(repo, sub, raw string) string {
	return ix.resolveName(repo, sub, raw, true)
}

func (ix *Index) resolveName(repo, sub, raw string, notSelf bool) string {
	name := normalizeBase(raw)
	if name == "" {
		return ""
	}
	if ix.types[key{repo, name}] {
		return name
	}
	cands := ix.byShort[key{repo, lastSegment(name)}]
	if len(cands) == 0 {
		return ""
	}
	if lastSegment(name) != name {
		// A qualified spelling: the type whose name ends in it, when one does.
		var tail []string
		for _, c := range cands {
			if hasQualifiedSuffix(c, name) {
				tail = append(tail, c)
			}
		}
		if len(tail) == 1 {
			return tail[0]
		}
		return ""
	}
	// A bare name: the candidate sharing the longest prefix with the subclass, so a
	// type of the subclass's own package wins over a namesake elsewhere.
	best, bestLen, tied := "", -1, false
	for _, c := range cands {
		if notSelf && c == sub {
			continue
		}
		n := commonPrefix(c, sub)
		switch {
		case n > bestLen:
			best, bestLen, tied = c, n, false
		case n == bestLen:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

// normalizeBase strips what a base spelling carries beyond its name: type arguments,
// a constructor call, a leading root qualifier.
func normalizeBase(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "<[("); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "::")
	s = strings.TrimPrefix(s, `\`)
	return strings.TrimSpace(s)
}

func lastSegment(name string) string {
	at := -1
	for _, sep := range [...]string{"::", "#", ".", "/", `\`} {
		if i := strings.LastIndex(name, sep); i >= 0 && i+len(sep) > at {
			at = i + len(sep)
		}
	}
	if at < 0 {
		return name
	}
	return name[at:]
}

func hasQualifiedSuffix(name, suffix string) bool {
	// A path and a dotted import name spell the same qualifier: app/lib.Helper is
	// what Python's app.lib.Helper names.
	name, suffix = strings.ReplaceAll(name, "/", "."), strings.ReplaceAll(suffix, "/", ".")
	if !strings.HasSuffix(name, suffix) {
		return false
	}
	if len(name) == len(suffix) {
		return true
	}
	switch name[len(name)-len(suffix)-1] {
	case '.', ':', '#', '/', '\\':
		return true
	}
	return false
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

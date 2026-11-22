package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// An annotation names its type the way any type reference does, and an annotation
// type used only as `@Menu(...)` has no other use: without an edge from what it
// annotates it reads as dead, or as test-only when a test happens to apply it.
//
// The walker cannot resolve the name alone. It writes the candidate FQNs, in
// javac's lookup order, into one relation of kind relTypeRef, and
// canonicalizeTargets keeps the first candidate the repository declares as a
// RelNames edge, or drops the relation. An annotation no candidate of which the
// repository declares (@Override, @Test, Spring's stereotypes) records nothing.
// RelNames, not a call or an import: a use of the type that keeps it out of the
// dead-code report and stays out of coupling metrics.
//
// Class literals, static member reads and declared parameter and field types
// (typerefs.go) record their type the same way.
const relTypeRef = "java_type_ref"

// javaLangAnnotations are the annotations java.lang declares. They are never looked
// up in the repository's packages: binding @Override to a repository type that
// happens to share the name would make it look used by every override.
var javaLangAnnotations = map[string]bool{
	"Override": true, "Deprecated": true, "SuppressWarnings": true,
	"FunctionalInterface": true, "SafeVarargs": true,
}

// relateAnnotations records every annotation in n's subtree, nested annotations
// inside another's arguments included, as a use by owner.
func (w *astWalker) relateAnnotations(n *sitter.Node, owner int) {
	if n == nil || owner == noOwner {
		return
	}
	switch kindOf(n) {
	case "annotation", "marker_annotation":
		w.relateAnnotation(n, owner)
	case "class_literal":
		// `@Import(Config.class)`: an annotation argument naming a type.
		w.relateClassLiteral(n, owner)
	case "field_access":
		w.relateStaticRead(n, owner)
	case "class_body", "block", "lambda_expression":
		return
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		w.relateAnnotations(n.NamedChild(i), owner)
	}
}

// relateAnnotation records one annotation's candidate types as a use by owner.
func (w *astWalker) relateAnnotation(n *sitter.Node, owner int) {
	if owner == noOwner {
		return
	}
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	w.relateCandidates(owner, typeCandidates(nodeText(nameNode, w.src), w.pkg, w.typeStack, w.importMap, w.wildcards))
}

// relateCandidates records one type use by owner as its candidate FQNs, once.
func (w *astWalker) relateCandidates(owner int, cands []string) {
	if owner == noOwner || len(cands) == 0 {
		return
	}
	target := strings.Join(cands, "|")
	if w.out[owner].HasRelation(relTypeRef, target) {
		return
	}
	w.relate(owner, relTypeRef, target)
}

// typeCandidates returns the FQNs a type name may denote, in the order
// javac looks them up: a member type of an enclosing class, a single-type import,
// the file's own package, then each on-demand import. A single-type import ends the
// search, since it shadows the package and the on-demand imports. A qualified name
// is a package-qualified FQN when it starts lower case, and otherwise a member type
// reached through a type resolved the same way.
func typeCandidates(name, pkg string, enclosing []string, imports map[string]string, wildcards []string) []string {
	name = strings.Join(strings.Fields(name), "")
	if name == "" {
		return nil
	}
	head, rest := name, ""
	if i := strings.IndexByte(name, '.'); i >= 0 {
		head, rest = name[:i], name[i:]
		if c := head[0]; c >= 'a' && c <= 'z' {
			return []string{name}
		}
	}
	var out []string
	for k := len(enclosing); k > 0; k-- {
		out = append(out, qualifyIn(pkg, strings.Join(enclosing[:k], ".")+"."+head)+rest)
	}
	if fqn, ok := imports[head]; ok {
		return append(out, fqn+rest)
	}
	if javaLangAnnotations[head] && rest == "" {
		return out
	}
	out = append(out, qualifyIn(pkg, head)+rest)
	for _, wc := range wildcards {
		out = append(out, wc+"."+head+rest)
	}
	return out
}

func qualifyIn(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// resolveTypeRefs rewrites a fact's relTypeRef relations to a RelNames
// edge at the first candidate typeIndex declares, and drops the rest: an annotation
// or type from outside the repository, a type naming itself, or a type the
// fact already names.
func resolveTypeRefs(f *facts.Fact, typeIndex map[string]string) {
	kept := f.Relations[:0]
	for _, r := range f.Relations {
		if r.Kind != relTypeRef {
			kept = append(kept, r)
			continue
		}
		for _, cand := range strings.Split(r.Target, "|") {
			canon, ok := typeIndex[cand]
			if !ok {
				continue
			}
			if canon != f.Name && !hasRel(kept, facts.RelNames, canon) {
				kept = append(kept, facts.Relation{Kind: facts.RelNames, Target: canon})
			}
			break
		}
	}
	f.Relations = kept
}

func hasRel(rels []facts.Relation, kind, target string) bool {
	for _, r := range rels {
		if r.Kind == kind && r.Target == target {
			return true
		}
	}
	return false
}

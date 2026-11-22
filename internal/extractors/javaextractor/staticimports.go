package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// A static import names a type: `import static T.member;` and `import static T.*;`
// both require the canonical type name T (JLS 7.5.3, 7.5.4), and the file does not
// compile without it. A constant holder or a static utility class reached only this
// way has no other reference, since the code then reads the bare member.
//
// The use belongs to the file, not to a declaration: an import has file scope, and
// binding each bare identifier back to the import it came from would need the
// imported type's members (unknown for `T.*` until every file is read) and javac's
// shadowing rules for inherited and local names. So the walker records T as a
// relTypeRef on one file reference fact (facts.KindFileRef, named by the file),
// the way a Scala file-scope object import does, and canonicalizeTargets keeps a
// RelNames edge only when the repository declares T. A JDK or library type binds
// to nothing, and a fact left with no edge is dropped (dropEmptyFileRefs).
//
// A static import of a type the file declares itself (`import static
// Self.Mode.IN;` in Self.java) is not a use from outside and records nothing.

// noteStaticImport records the type a static import names. path is the import as
// written, without the `.*` of an on-demand import.
func (w *astWalker) noteStaticImport(path string, onDemand bool) {
	typeName := path
	if !onDemand {
		typeName = parentName(path)
	}
	if typeName == "" {
		return
	}
	if w.fileRefIdx < 0 {
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindFileRef,
			Name:  w.relFile,
			File:  w.relFile,
			Line:  1,
			Props: map[string]any{"language": "java"},
		})
		w.fileRefIdx = len(w.out) - 1
	}
	w.relateCandidates(w.fileRefIdx, []string{typeName})
}

// dropOwnStaticImports removes, once the file is walked, the static imports that
// name a type the file declares, and the file reference fact if none is left.
func (w *astWalker) dropOwnStaticImports() {
	if w.fileRefIdx < 0 {
		return
	}
	var own []string
	for _, f := range w.out {
		if fqn, _ := f.PropAny("fqn").(string); fqn != "" && f.Kind == facts.KindSymbol {
			own = append(own, fqn)
		}
	}
	ref := &w.out[w.fileRefIdx]
	kept := ref.Relations[:0]
	for _, r := range ref.Relations {
		if !declaredIn(r.Target, own) {
			kept = append(kept, r)
		}
	}
	ref.Relations = kept
	if len(kept) == 0 {
		w.out = append(w.out[:w.fileRefIdx], w.out[w.fileRefIdx+1:]...)
		w.fileRefIdx = -1
	}
}

// declaredIn reports whether typeName is one of the types own lists, or a member
// type of one.
func declaredIn(typeName string, own []string) bool {
	for _, d := range own {
		if typeName == d || strings.HasPrefix(typeName, d+".") {
			return true
		}
	}
	return false
}

// dropEmptyFileRefs removes a file reference fact whose every static import
// resolved to nothing the repository declares.
func dropEmptyFileRefs(all []facts.Fact) []facts.Fact {
	out := all[:0]
	for _, f := range all {
		if f.Kind == facts.KindFileRef && len(f.Relations) == 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}

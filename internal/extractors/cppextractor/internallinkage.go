package cppextractor

import (
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Internal linkage: one file-qualified identity.
//
// Symbols are named by directory and name, and dedupeSymbols folds same-named
// facts (it exists to fuse a header prototype with its definition). A declaration
// visible only in its own translation unit is not one function across files: two
// drivers' `static int to_state(...)` in one directory are two functions. So a
// declaration with internal linkage is named `<dir>.<name>@<file>`, where <file> is
// the file's base name with every `.` replaced by `_` (`adv7180.c` gives
// `@adv7180_c`), the convention the Dart extractor uses for unnamed extensions.
//
// Internal linkage here is:
//   - a free function (definition or prototype) or a file-scope initialized variable
//     declared `static`; in C, a `static` prototype gives a later definition of the
//     name in the same file internal linkage too, so the rule is per name and file;
//   - a free function or variable declared inside an anonymous namespace.
//
// Types declared in an anonymous namespace keep their name: their members are named
// through the type and the type index resolves them by it.
//
// A `static` defined in a header (`static inline` in a .h) is internal to every
// translation unit that includes it. Its one definition is in the header, so the
// header is its qualifier (`include/linux.foo@bar_h`), and the files that include
// it resolve to that one fact (resolveInternal).
//
// Several programs in one directory (`main` defined in more than one file of it)
// get the same qualifier for their `main`: separate programs, never one function.

// internalQualifier is the `@<file>` suffix for relFile.
func internalQualifier(relFile string) string {
	base := path.Base(strings.ReplaceAll(relFile, "\\", "/"))
	if base == "." || base == "/" || base == "" {
		return ""
	}
	return "@" + strings.ReplaceAll(base, ".", "_")
}

// splitInternal splits a file-qualified name into the name it qualifies and its
// qualifier. C and C++ identifiers cannot contain `@`, so one marks the identity.
func splitInternal(name string) (base string, ok bool) {
	i := strings.LastIndexByte(name, '@')
	if i < 0 {
		return name, false
	}
	return name[:i], true
}

// qualifyInternals renames, within one file's facts, every symbol named in
// internal (the names the walker saw declared with internal linkage) to its
// file-qualified identity, and points every reference the file makes to one of
// them at the new name. A static is callable only from its own translation unit,
// so binding the file's own references to it is exact.
func qualifyInternals(out []facts.Fact, internal map[string]bool, relFile, dir string) {
	if len(internal) == 0 {
		return
	}
	q := internalQualifier(relFile)
	if q == "" {
		return
	}
	rename := make(map[string]string, len(internal))
	for old := range internal {
		rename[old] = old + q
	}
	// Leaf name → the file's internal declarations with that leaf, for a call
	// written with less of the namespace than the declaration has: bare inside the
	// namespace (`dir.helper` written, `dir.ns::helper` declared), qualified by an
	// inner namespace only (`inner::helper`), or bare under a using-directive.
	byLeaf := map[string][]string{}
	prefix := dir + "."
	for old := range rename {
		rest := strings.TrimPrefix(old, prefix)
		leaf := rest
		if i := strings.LastIndex(rest, "::"); i >= 0 {
			leaf = rest[i+2:]
		}
		byLeaf[leaf] = append(byLeaf[leaf], old)
	}
	for _, olds := range byLeaf {
		sort.Strings(olds)
	}
	retarget := func(caller *facts.Fact, target string) (string, bool) {
		if n, ok := rename[target]; ok {
			return n, true
		}
		written := strings.TrimPrefix(target, prefix)
		if written == target && !strings.Contains(target, "::") {
			return "", false // another directory's name
		}
		leaf := written
		if i := strings.LastIndex(written, "::"); i >= 0 {
			leaf = written[i+2:]
		}
		candidates := byLeaf[leaf]
		if len(candidates) == 0 {
			return "", false
		}
		if strings.Contains(written, "::") {
			// Qualified: the declaration's scope must end with the written one.
			var hit string
			for _, old := range candidates {
				rest := strings.TrimPrefix(old, prefix)
				if rest != written && !strings.HasSuffix(rest, "::"+written) {
					continue
				}
				if hit != "" {
					return "", false
				}
				hit = old
			}
			if hit == "" {
				return "", false
			}
			return rename[hit], true
		}
		// Bare: a declaration in a namespace enclosing the caller first, else the
		// one declaration of that leaf the file has (reached by a using-directive).
		scope := ""
		if caller.Kind == facts.KindSymbol {
			scope = strings.TrimPrefix(caller.Name, prefix)
		}
		var hit string
		for _, old := range candidates {
			ns := strings.TrimSuffix(strings.TrimPrefix(old, prefix), leaf)
			if ns != "" && !strings.HasPrefix(scope, ns) {
				continue
			}
			if hit != "" {
				return "", false
			}
			hit = old
		}
		if hit == "" && len(candidates) == 1 {
			hit = candidates[0]
		}
		if hit == "" {
			return "", false
		}
		return rename[hit], true
	}
	for i := range out {
		f := &out[i]
		if f.Kind == facts.KindSymbol {
			if n, ok := rename[f.Name]; ok {
				f.Name = n
			}
		}
		for j := range f.Relations {
			r := &f.Relations[j]
			switch r.Kind {
			case facts.RelDeclares, facts.RelImports:
				continue
			case relCapitalizedCallCandidate:
				// A bare capitalized callee; a file's own static by that name is the call.
				if n, ok := retarget(f, prefix+r.Target); ok {
					r.Kind = facts.RelCalls
					r.Target = n
				}
				continue
			case relReceiverCallCandidate:
				// `helper()::method`: the callee whose return type types the receiver.
				if i := strings.Index(r.Target, callMark+"::"); i > 0 {
					callee := r.Target[:i]
					n, ok := retarget(f, callee)
					if !ok {
						n, ok = retarget(f, prefix+callee)
					}
					if ok {
						r.Target = strings.TrimPrefix(n, prefix) + r.Target[i:]
					}
				}
				continue
			}
			if n, ok := retarget(f, r.Target); ok {
				r.Target = n
			}
		}
		for _, key := range [...]string{"calls_in_loop", "calls_in_scaling_loop"} {
			calls, ok := f.Props[key].([]string)
			if !ok {
				continue
			}
			for j, c := range calls {
				if n, ok := retarget(f, c); ok {
					calls[j] = n
				}
			}
		}
	}
	dedupeRelations(out)
}

// dedupeRelations drops a relation repeated on one fact after retargeting.
func dedupeRelations(out []facts.Fact) {
	for i := range out {
		rels := out[i].Relations
		if len(rels) < 2 {
			continue
		}
		seen := make(map[string]bool, len(rels))
		kept := rels[:0]
		for _, r := range rels {
			k := r.Kind + "\x00" + r.Target
			if seen[k] {
				continue
			}
			seen[k] = true
			kept = append(kept, r)
		}
		out[i].Relations = kept
	}
}

// qualifyMains gives each `main` its file's qualifier when several files of one
// directory define one: separate programs, which dedupeSymbols would fold.
func qualifyMains(perFileFacts [][]facts.Fact, files []string) {
	byDir := map[string][]int{}
	for i, fileFacts := range perFileFacts {
		dir := factpath.Dir(files[i])
		for _, f := range fileFacts {
			if f.Kind == facts.KindSymbol && f.Name == dir+".main" && f.PropBool("has_body") {
				byDir[dir] = append(byDir[dir], i)
				break
			}
		}
	}
	for dir, idx := range byDir {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			qualifyInternals(perFileFacts[i], map[string]bool{dir + ".main": true}, files[i], dir)
		}
	}
}

// sharesTranslationUnit reports whether file's internal declarations may be seen
// from another file: a header, or a source file some file includes by name
// (`#include "foo.c"`, a unit test included at the end of the file it tests).
func sharesTranslationUnit(file string, includedSources map[string]bool) bool {
	return isHeaderExt(file) || includedSources[path.Base(file)]
}

// includedSourceNames is the base names of non-header files named by a quoted
// include anywhere in the snapshot.
func includedSourceNames(allFacts []facts.Fact) map[string]bool {
	out := map[string]bool{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindDependency || f.PropString("include_style") != "quoted" {
			continue
		}
		inc := path.Base(strings.ReplaceAll(f.PropString("include"), "\\", "/"))
		if inc != "" && !isHeaderExt(inc) {
			out[inc] = true
		}
	}
	return out
}

// internalIndex maps a name as a call in the declaring directory writes it
// (`dir.name`) to the one internal declaration another file may mean by it.
type internalIndex struct {
	// shared holds the declarations of headers and included sources, keyed by the
	// name qualified (`dir.ns::name`) and, second, by its leaf alone (`dir.name`),
	// as a call made inside the namespace writes it.
	shared, sharedLeaf map[string]string
	// sources holds the other declarations by the same two keys, each with the
	// file that declares it: a static of a source file, which only a source file
	// it includes (`#include "impl.cxx"`, a unit test included at its end) shares.
	sources, sourcesLeaf map[string][]internalDecl
	// includes maps a file to the base names of the files it includes.
	includes map[string]map[string]bool
}

type internalDecl struct{ name, file string }

func newInternalIndex(allFacts []facts.Fact, includedSources map[string]bool) internalIndex {
	idx := internalIndex{shared: map[string]string{}, sharedLeaf: map[string]string{},
		sources: map[string][]internalDecl{}, sourcesLeaf: map[string][]internalDecl{},
		includes: map[string]map[string]bool{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindDependency && f.PropString("include_style") == "quoted" {
			inc := path.Base(strings.ReplaceAll(f.PropString("include"), "\\", "/"))
			if idx.includes[f.File] == nil {
				idx.includes[f.File] = map[string]bool{}
			}
			idx.includes[f.File][inc] = true
			continue
		}
		if f.Kind != facts.KindSymbol {
			continue
		}
		base, ok := splitInternal(f.Name)
		if !ok {
			continue
		}
		leaf := ""
		if dot := strings.IndexByte(base, '.'); dot >= 0 {
			if i := strings.LastIndex(base, "::"); i > dot {
				leaf = base[:dot+1] + base[i+2:]
			}
		}
		if sharesTranslationUnit(f.File, includedSources) {
			indexUniqueName(idx.shared, base, f.Name)
			if leaf != "" {
				indexUniqueName(idx.sharedLeaf, leaf, f.Name)
			}
			continue
		}
		d := internalDecl{name: f.Name, file: f.File}
		idx.sources[base] = append(idx.sources[base], d)
		if leaf != "" {
			idx.sourcesLeaf[leaf] = append(idx.sourcesLeaf[leaf], d)
		}
	}
	return idx
}

// resolve binds target, a call written in caller's file to a name its own file
// does not declare internally, to the one internal declaration of that name in the
// caller's directory it can see: a header's (or an included source's), or, from a
// source file another source includes, a static of the including file.
func (idx internalIndex) resolve(caller *facts.Fact, target string, includedSources map[string]bool) string {
	if n, ok := idx.shared[target]; ok {
		return n // "" when the directory's headers declare several
	}
	if n := idx.sharedLeaf[target]; n != "" {
		return n
	}
	if caller.Kind != facts.KindSymbol || isHeaderExt(caller.File) || !includedSources[path.Base(caller.File)] {
		return ""
	}
	if n := idx.includer(idx.sources[target], caller.File); n != "" {
		return n
	}
	return idx.includer(idx.sourcesLeaf[target], caller.File)
}

// includer is the one declaration among decls whose file includes file.
func (idx internalIndex) includer(decls []internalDecl, file string) string {
	base, hit := path.Base(file), ""
	for _, d := range decls {
		if !idx.includes[d.file][base] {
			continue
		}
		if hit != "" && hit != d.name {
			return ""
		}
		hit = d.name
	}
	return hit
}

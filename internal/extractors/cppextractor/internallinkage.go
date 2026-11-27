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
// header is its qualifier (`include/linux.foo@bar_h`), and the files that see it
// resolve to that one fact: by name in the header's directory, and by its short
// name from elsewhere only while no source file declares a static of that name
// too (internalIndex, indexFunctionNames). A static several headers of one
// directory define (alternatives a configuration picks) stays the directory's one
// declaration, unqualified, as base folded it (foldHeaderAlternatives). A source file's static is seen only by
// a source sharing its translation unit through a quoted include, either way
// round (`#include "impl.c"`, a unit test included at the end of its file).
// A prototype of a static that a source sharing its translation unit defines
// takes the definition's name, so the two fuse (fuseStaticPrototypes).
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

// internalIndex finds the internal declaration a call written outside the
// declaring file means, where the language lets one file see another's.
type internalIndex struct {
	// headers holds the statics of headers, keyed by the name qualified
	// (`dir.ns::name`) and, second, by its leaf alone (`dir.name`), as a call made
	// inside the namespace writes it: what a file of the header's directory sees.
	headers, headersLeaf map[string]string
	// sources holds the statics of source files by leaf, each with its file: one
	// another source shares only by including it (`#include "impl.c"`), or by being
	// included by it (a unit test included at the end of the file it tests).
	sources map[string][]internalDecl
	// headerDecls holds the statics of headers by leaf, each with its header, for
	// a name several headers define (alternatives a configuration picks between):
	// the one a caller's file includes itself is the one it calls.
	headerDecls map[string][]internalDecl
	// includes maps a file to what its quoted includes name: each include's base
	// name, and its path resolved against the including file's directory.
	includes map[string]map[string]bool
	// written holds each file's includes as written, quoted or angle, and the
	// quoted ones resolved against the file's directory too.
	written map[string][]string
}

type internalDecl struct{ name, file string }

func newInternalIndex(allFacts []facts.Fact, includedSources map[string]bool) internalIndex {
	idx := internalIndex{headers: map[string]string{}, headersLeaf: map[string]string{},
		sources: map[string][]internalDecl{}, headerDecls: map[string][]internalDecl{},
		includes: map[string]map[string]bool{}, written: map[string][]string{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindDependency {
			inc := path.Clean(strings.ReplaceAll(f.PropString("include"), "\\", "/"))
			if inc == "." || inc == "" {
				continue
			}
			idx.written[f.File] = append(idx.written[f.File], inc)
			if f.PropString("include_style") != "quoted" {
				continue
			}
			joined := path.Join(path.Dir(f.File), inc)
			idx.written[f.File] = append(idx.written[f.File], joined)
			if idx.includes[f.File] == nil {
				idx.includes[f.File] = map[string]bool{}
			}
			idx.includes[f.File][path.Base(inc)] = true
			idx.includes[f.File][joined] = true
			continue
		}
		if f.Kind != facts.KindSymbol {
			continue
		}
		base, ok := splitInternal(f.Name)
		if !ok {
			continue
		}
		if !isHeaderExt(f.File) {
			// Only a source some file includes, or one including another, can share.
			if len(includedSources) > 0 {
				short := lastScopeComponent(base)
				idx.sources[short] = append(idx.sources[short], internalDecl{name: f.Name, file: f.File})
			}
			continue
		}
		indexUniqueName(idx.headers, base, f.Name)
		short := lastScopeComponent(base)
		idx.headerDecls[short] = append(idx.headerDecls[short], internalDecl{name: f.Name, file: f.File})
		if dot := strings.IndexByte(base, '.'); dot >= 0 {
			if i := strings.LastIndex(base, "::"); i > dot {
				indexUniqueName(idx.headersLeaf, base[:dot+1]+base[i+2:], f.Name)
			}
		}
	}
	return idx
}

// resolve binds target, a call written in caller's file to a name its own file
// does not declare internally, to the one internal declaration the caller sees:
// a static of a header in the caller's directory, or of a source file that shares
// the caller's translation unit through a quoted include, either way round.
func (idx internalIndex) resolve(caller *facts.Fact, target string, includedSources map[string]bool) string {
	if n, ok := idx.headers[target]; ok {
		if n == "" { // the directory's headers declare several
			return idx.includedHeader(caller, lastScopeComponent(target))
		}
		return n
	}
	if n := idx.headersLeaf[target]; n != "" {
		return n
	}
	if caller.Kind != facts.KindSymbol || isHeaderExt(caller.File) || len(idx.sources) == 0 {
		return ""
	}
	decls := idx.sources[lastScopeComponent(target)]
	// The include as written resolves to the file first; a base name alone, which
	// another directory's file of the same name shares, only when none does.
	if hit, found := idx.sharer(decls, caller.File, includedSources, func(f string) string { return f }); found {
		return hit
	}
	hit, _ := idx.sharer(decls, caller.File, includedSources, path.Base)
	return hit
}

// includedHeader is the one static named leaf of a header that caller's file
// includes itself, an include naming the header's path or a suffix of it
// (`<asm/msr.h>` for arch/x86/include/asm/msr.h), or "".
func (idx internalIndex) includedHeader(caller *facts.Fact, leaf string) string {
	if caller.Kind != facts.KindSymbol {
		return ""
	}
	written, hit := idx.written[caller.File], ""
	for _, d := range idx.headerDecls[leaf] {
		if !namesFile(written, d.file) {
			continue
		}
		if hit != "" && hit != d.name {
			return ""
		}
		hit = d.name
	}
	return hit
}

// namesFile reports whether one of includes names file: its path, or a trailing
// run of its path components.
func namesFile(includes []string, file string) bool {
	for _, inc := range includes {
		if file == inc || strings.HasSuffix(file, "/"+inc) {
			return true
		}
	}
	return false
}

// sharer is the one declaration of decls whose file shares file's translation
// unit through a quoted include, either way round, naming files by key; found
// reports whether any did.
func (idx internalIndex) sharer(decls []internalDecl, file string, includedSources map[string]bool, key func(string) string) (hit string, found bool) {
	for _, d := range decls {
		if d.file == file {
			continue
		}
		callerIncludes := idx.includes[file][key(d.file)]
		includesCaller := includedSources[path.Base(file)] && idx.includes[d.file][key(file)]
		if !callerIncludes && !includesCaller {
			continue
		}
		if found && hit != d.name {
			return "", true
		}
		hit, found = d.name, true
	}
	return hit, found
}

// fuseStaticPrototypes renames a prototype of a static, with no body, to the
// definition it declares: the one static of that name, with a body, in a source
// file that shares the prototype's translation unit through quoted includes (the
// source includes the prototype's file, the other way round, or a third file
// includes both, as a driver including its core `.c` and that core's header), in
// the prototype's directory.
// References to the prototype follow it, and dedupeSymbols then fuses the two, as
// it fuses an extern prototype with its definition.
func fuseStaticPrototypes(allFacts []facts.Fact) {
	includes := map[string]map[string]bool{}
	includers := map[string][]string{} // base name -> files including it
	defs := map[string][]int{}         // leaf -> definitions
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindDependency && f.PropString("include_style") == "quoted" {
			inc := path.Base(strings.ReplaceAll(f.PropString("include"), "\\", "/"))
			if includes[f.File] == nil {
				includes[f.File] = map[string]bool{}
			}
			if !includes[f.File][inc] {
				includes[f.File][inc] = true
				includers[inc] = append(includers[inc], f.File)
			}
			continue
		}
		if f.Kind != facts.KindSymbol || !f.PropBool("has_body") {
			continue
		}
		if base, ok := splitInternal(f.Name); ok {
			defs[lastScopeComponent(base)] = append(defs[lastScopeComponent(base)], i)
		}
	}
	shareUnit := func(a, b string) bool {
		ba, bb := path.Base(a), path.Base(b)
		if ba == bb {
			return false // a file name cannot tell the two apart
		}
		if includes[a][bb] || includes[b][ba] {
			return true
		}
		for _, c := range includers[ba] {
			if includes[c][bb] {
				return true
			}
		}
		return false
	}
	// A prototype in the file that defines it is that definition already.
	defined := map[string]bool{}
	for _, list := range defs {
		for _, j := range list {
			defined[allFacts[j].Name] = true
		}
	}
	rename := map[string]string{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || f.PropBool("has_body") {
			continue
		}
		base, ok := splitInternal(f.Name)
		if !ok || defined[f.Name] {
			continue
		}
		def := ""
		for _, j := range defs[lastScopeComponent(base)] {
			d := &allFacts[j]
			if factpath.Dir(d.File) != factpath.Dir(f.File) || !sameUnqualified(d.Name, base) || !shareUnit(f.File, d.File) {
				continue
			}
			if def != "" && def != d.Name {
				def = ""
				break
			}
			def = d.Name
		}
		if def != "" {
			rename[f.Name] = def
			f.Name = def
		}
	}
	if len(rename) == 0 {
		return
	}
	for i := range allFacts {
		for j := range allFacts[i].Relations {
			if n, ok := rename[allFacts[i].Relations[j].Target]; ok {
				allFacts[i].Relations[j].Target = n
			}
		}
	}
}

// sameUnqualified reports whether name, less its file qualifier and directory,
// is base less its directory: one declaration's scope and name in two files.
func sameUnqualified(name, base string) bool {
	n, _ := splitInternal(name)
	_, a, _ := strings.Cut(n, ".")
	_, b, _ := strings.Cut(base, ".")
	return a == b
}

// foldHeaderAlternatives names a static that several headers of one directory
// define as the directory's one declaration, without a file qualifier, as an
// extern is: such headers are mostly alternatives a configuration picks between
// (`asm/msr.h` and `asm/paravirt.h`), the case one declaration across files keeps
// folded. A static one header of its directory defines keeps its qualifier.
func foldHeaderAlternatives(allFacts []facts.Fact) {
	defined := map[string]map[string]bool{} // unqualified name -> headers defining it
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || !isHeaderExt(f.File) || !f.PropBool("has_body") {
			continue
		}
		if base, ok := splitInternal(f.Name); ok {
			if defined[base] == nil {
				defined[base] = map[string]bool{}
			}
			defined[base][f.File] = true
		}
	}
	rename := map[string]string{}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol || !isHeaderExt(f.File) {
			continue
		}
		if base, ok := splitInternal(f.Name); ok && len(defined[base]) > 1 {
			rename[f.Name] = base
			f.Name = base
		}
	}
	if len(rename) == 0 {
		return
	}
	for i := range allFacts {
		for j := range allFacts[i].Relations {
			if n, ok := rename[allFacts[i].Relations[j].Target]; ok {
				allFacts[i].Relations[j].Target = n
			}
		}
		for _, key := range [...]string{"calls_in_loop", "calls_in_scaling_loop"} {
			if calls, ok := allFacts[i].Props[key].([]string); ok {
				for j, c := range calls {
					if n, ok := rename[c]; ok {
						calls[j] = n
					}
				}
			}
		}
	}
}

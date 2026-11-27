package cppextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// enclosingLookup finds the function an unqualified call names the way C++
// unqualified lookup does: in the caller's enclosing namespaces, innermost first.
// The walker scopes such a call to the caller's directory ("<dir>.name"), and a
// function declared in a namespace is named "<dir>.ns::name", so without this the
// call binds only when the short name is unique in the repository. A same-named
// function in a directory the caller's file cannot include (a test helper, a
// vendored copy) then leaves it dangling.
//
// Only the declarations the file may see count, as for types (typeIndex.resolve).
// A call made inside a member function is found in the class first, or in a base;
// those are left to the member passes, so a namespace function never takes a call
// the class or a base answers.
type enclosingLookup struct {
	// declared maps a symbol's name without its directory ("db::Type::field",
	// "db::Compute") to every directory that declares it, in fact order.
	declared map[string][]string
	// callable is declared restricted to functions and methods.
	callable map[string][]string
	// byShort maps a function's short name to its callable keys.
	byShort map[string][]string
	types   typeIndex
	bases   baseIndex
	known   map[string]bool
	vis     *includeVisibility
	// internal maps "<dir>.<qualified>" to the internal declaration of a header (or
	// included source) it names, "" when that directory has several.
	internal map[string]string
}

func newEnclosingLookup(allFacts []facts.Fact, types typeIndex, bases baseIndex, known map[string]bool, vis *includeVisibility, includedSources map[string]bool) *enclosingLookup {
	l := &enclosingLookup{declared: map[string][]string{}, callable: map[string][]string{},
		byShort: map[string][]string{}, types: types, bases: bases, known: known, vis: vis,
		internal: map[string]string{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		dir := sourceDir(f)
		// An internal declaration is looked up by the name it qualifies, and only
		// when another file can see it (internallinkage.go).
		name, internal := splitInternal(f.Name)
		if internal {
			if !sharesTranslationUnit(f.File, includedSources) {
				continue
			}
			indexUniqueName(l.internal, name, f.Name)
		}
		qualified, ok := strings.CutPrefix(name, dir+".")
		if !ok || !strings.Contains(qualified, "::") {
			continue
		}
		l.declared[qualified] = appendDir(l.declared[qualified], dir)
		if sk := f.PropString("symbol_kind"); sk == facts.SymbolFunc || sk == facts.SymbolMethod {
			if len(l.callable[qualified]) == 0 {
				short := qualified[strings.LastIndex(qualified, "::")+2:]
				l.byShort[short] = append(l.byShort[short], qualified)
			}
			l.callable[qualified] = appendDir(l.callable[qualified], dir)
		}
	}
	return l
}

func appendDir(dirs []string, dir string) []string {
	for _, d := range dirs {
		if d == dir {
			return dirs
		}
	}
	return append(dirs, dir)
}

// resolve returns the function the caller's unqualified call to name binds to, or
// "" when no enclosing namespace of the caller declares a function by that name in
// sight, or the call may name a member of the caller's class.
func (l *enclosingLookup) resolve(caller *facts.Fact, source, name string) string {
	qualified, ok := strings.CutPrefix(caller.Name, source+".")
	if !ok {
		return ""
	}
	scopes := strings.Split(qualified, "::")
	scopes = scopes[:len(scopes)-1]
	n := len(scopes)
	if receiver := caller.PropString("receiver"); receiver != "" {
		// The class scopes of a member function: its trailing components that name
		// a type. A member any of them declares (in any directory) is what the call
		// names, and so is one a base of the innermost class declares.
		for n > 0 && len(l.types.dirs[templateBase(scopes[n-1])]) > 0 {
			if len(l.declared[strings.Join(scopes[:n], "::")+"::"+name]) > 0 {
				return ""
			}
			n--
		}
		if n == len(scopes) {
			return "" // the receiver is no type the repository declares
		}
		class := templateBase(scopes[len(scopes)-1])
		dir := l.types.resolve(class, source, l.vis, nil)
		if dir == "" {
			return "" // which class it is, and so what it inherits, is not known
		}
		if l.bases.lookup(l.types.canonical(dir, class), name, l.known) != "" {
			return ""
		}
	}
	for ; n > 0; n-- {
		key := strings.Join(scopes[:n], "::") + "::" + name
		all := l.callable[key]
		var candidates []string
		for _, dir := range all {
			if l.vis.mayReach(source, dir) {
				candidates = append(candidates, dir)
			}
		}
		if len(candidates) == 0 {
			continue // nothing in sight here: C++ goes on to the enclosing namespace
		}
		if l.nestedInSight(scopes[:n], key, name, source) {
			return "" // a using-directive may bring that one in instead
		}
		if dir := pickInSight(candidates, len(all), source, l.vis); dir != "" {
			if canonical, ok := l.internal[dir+"."+key]; ok {
				return canonical // "" when the directory's headers declare several
			}
			return dir + "." + key
		}
		return "" // several in sight, none preferred: the name is found but ambiguous
	}
	return ""
}

// nestedInSight reports whether a namespace nested in scope declares a function
// named name in a directory source may see. A using-directive for it (which the
// walker does not record) would put that overload in the same lookup.
func (l *enclosingLookup) nestedInSight(scope []string, key, name, source string) bool {
	prefix := strings.Join(scope, "::") + "::"
	for _, other := range l.byShort[name] {
		rest, ok := strings.CutPrefix(other, prefix)
		if !ok || other == key {
			continue
		}
		if first, _, _ := strings.Cut(rest, "::"); len(l.types.dirs[templateBase(first)]) > 0 {
			continue // a class member, which no using-directive brings in
		}
		for _, dir := range l.callable[other] {
			if l.vis.mayReach(source, dir) {
				return true
			}
		}
	}
	return false
}

// pickInSight chooses among the directories in sight of source that declare a
// name, by the rule typeIndex.resolve applies: a name declared once resolves to
// its copy; otherwise the source's own directory, then the single directory its
// includes are known to reach.
func pickInSight(candidates []string, declaredIn int, source string, vis *includeVisibility) string {
	if len(candidates) == 1 && declaredIn == 1 {
		return candidates[0]
	}
	for _, dir := range candidates {
		if dir == source {
			return dir
		}
	}
	visible := ""
	for _, dir := range candidates {
		if !vis.reaches(source, dir) {
			continue
		}
		if visible != "" {
			return ""
		}
		visible = dir
	}
	return visible
}

// templateBase is a scope component without its template arguments: Chain<C> is
// Chain.
func templateBase(component string) string {
	base, _, _ := strings.Cut(component, "<")
	return base
}

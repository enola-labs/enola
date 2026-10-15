package rubyextractor

import (
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Call resolution.
//
// The walker records a call as the method's file writes it: `process_status!` for a
// call on self, `Admin::Report.build` for a class method, and "PostStatusService#call"
// for `PostStatusService.new.call`. A graph edge needs the name the callee is declared
// under, and three things that decide it are visible only with every class in hand:
//
//   - A call on self is a method of the caller's class, or of the ancestor that
//     declares it.
//   - A constant is looked up from the caller's namespace outward, so `Report` inside
//     `Admin::ReportsController` is `Admin::Report` when that is declared.
//   - A method a class inherits is declared on the ancestor.
//
// A call this cannot settle keeps the name the walker gave it. The one exception is a
// call on a new instance, which falls back to the spelling it had before this pass
// existed.

// resolveRubyCalls rewrites the calls of every method to the method they reach, where
// one declared method fits.
func resolveRubyCalls(allFacts []facts.Fact) {
	ix := buildHandlerIndex(allFacts)
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		// The walker gives a class method the function kind.
		if k := f.PropString("symbol_kind"); k != facts.SymbolMethod && k != facts.SymbolFunc {
			continue
		}
		owner, sep := methodOwner(f.Name)
		if owner == "" {
			continue
		}
		seen := map[string]bool{}
		var named []string
		out := f.Relations[:0]
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				var class string
				r.Target, class = ix.resolveCall(r.Target, owner, sep)
				if class != "" && !slices.Contains(named, class) {
					named = append(named, class)
				}
				if seen[r.Target] {
					continue
				}
				seen[r.Target] = true
			}
			out = append(out, r)
		}
		f.Relations = out
		if len(named) > 0 {
			f.SetProp(propNamedReceivers, named)
		}
	}
}

// propNamedReceivers lists, on a method, the classes it calls a singleton method
// on that an ancestor declares: `User::Action::SuspendAll.call` resolved to
// `Service::ActionBase.call`. The call edge names the ancestor, but the code names
// the class, and the class is what the method couples to. resolveImports reads it
// and removes it.
const propNamedReceivers = "named_receivers"

// methodOwner splits a method's name into the class or module that declares it and
// the separator of its kind: "#" for an instance method, "." for a singleton one. ""
// for a method no class declares, which the walker names after its directory.
func methodOwner(name string) (owner, sep string) {
	if i := strings.LastIndexByte(name, '#'); i > 0 {
		return name[:i], "#"
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 && !strings.Contains(name[:i], "/") {
		return name[:i], "."
	}
	return "", ""
}

// resolveCall returns the declared method a call target reaches from a method of
// owner, or the target as the walker wrote it. When a singleton method on a named
// class resolves to an ancestor's, it also returns the class.
func (ix *handlerIndex) resolveCall(target, owner, sep string) (resolved, named string) {
	switch {
	case strings.HasPrefix(target, "#"):
		// `new(...).call` inside the class: an instance method of the class itself.
		if m := ix.member(owner, "#", target[1:]); m != "" {
			return m, ""
		}
		return "new." + target[1:], ""

	case strings.Contains(target, "#"):
		// `Const.new.call`: an instance method of the class the constant names.
		recv, method, _ := strings.Cut(target, "#")
		if class := ix.lexical(recv, owner); class != "" {
			if m := ix.member(class, "#", method); m != "" {
				return m, ""
			}
		}
		return "new." + method, ""

	case isBareMethodName(target):
		// A call on self: a method of the caller's own kind first, then, for an
		// instance method calling `self.class.name`, a singleton one.
		if m := ix.member(owner, sep, target); m != "" {
			return m, ""
		}
		if sep == "#" {
			if m := ix.member(owner, ".", target); m != "" {
				return m, ""
			}
		}
		return target, ""
	}

	// `Const.method`: the constant from the caller's namespace outward, then the
	// singleton method on it or an ancestor.
	dot := strings.LastIndexByte(target, '.')
	if dot <= 0 || !startsUpper(target) {
		return target, ""
	}
	class := ix.lexical(target[:dot], owner)
	if class == "" {
		return target, ""
	}
	if m := ix.member(class, ".", target[dot+1:]); m != "" {
		if declaring, _ := methodOwner(m); declaring != class {
			return m, class
		}
		return m, ""
	}
	return target, ""
}

// isBareMethodName reports whether a call target is a method name with no receiver:
// lower-case or underscore first, and no namespace, receiver or path in it.
func isBareMethodName(target string) bool {
	if target == "" || strings.ContainsAny(target, ".#:/") {
		return false
	}
	c := target[0]
	return c == '_' || (c >= 'a' && c <= 'z')
}

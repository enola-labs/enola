package pythonextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Loops whose trip count is fixed where the code is written. A literal collection
// and range(<int>) were the only forms recognised. The rest of what is fixed is
// fixed by NAME:
//
//	for prefix in RESERVED_URL_PREFIXES: …    an ALL_CAPS module constant
//	for name, d in RESOURCE_MAP.items(): …    a view of one
//	for state in DagRunState: …               a class, which only an Enum lets you iterate
//	for i in range(MAX_RETRIES): …            a constant bound
//	attrs = ("a", "b"); for a in attrs: …     a local that is a literal and stays one
//
// Each is a syntactic proof and fails closed: a name that merely looks constant
// (lower case, a parameter, a local that is appended to) keeps counting.

// pyScreaming reports whether name is SCREAMING_SNAKE_CASE with at least two
// characters.
func pyScreaming(name string) bool {
	if len(name) < 2 {
		return false
	}
	hasLetter := false
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

// pyClassName reports whether name is CapWords: an initial capital and at least
// one lower-case letter, which sets it apart from a constant.
func pyClassName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	return strings.ContainsAny(name, "abcdefghijklmnopqrstuvwxyz")
}

// pyConstViews return a view of their argument or receiver, as many elements as
// it has.
var (
	pyConstViewFuncs   = map[string]bool{"sorted": true, "list": true, "tuple": true, "set": true, "reversed": true, "enumerate": true, "iter": true}
	pyConstViewMethods = map[string]bool{"items": true, "values": true, "keys": true}
)

// pyConstantIterable reports whether it is a collection with a fixed number of
// elements. consts are the local names proven to be literal collections.
func pyConstantIterable(it *sitter.Node, src []byte, consts map[string]bool) bool {
	if it == nil {
		return false
	}
	switch kindOf(it) {
	case "list", "tuple", "set", "dictionary":
		return true
	case "parenthesized_expression":
		return it.NamedChildCount() > 0 && pyConstantIterable(it.NamedChild(0), src, consts)
	case "identifier":
		name := pyText(it, src)
		return pyScreaming(name) || pyClassName(name) || consts[name]
	case "attribute":
		// `settings.STOP_WORDS`, `models.DagRunState`: the trailing name decides.
		if attr := it.ChildByFieldName("attribute"); attr != nil {
			name := pyText(attr, src)
			return pyScreaming(name) || pyClassName(name)
		}
	case "call":
		fn := it.ChildByFieldName("function")
		args := it.ChildByFieldName("arguments")
		if fn == nil || args == nil {
			return false
		}
		switch kindOf(fn) {
		case "identifier":
			name := pyText(fn, src)
			if name == "range" {
				return pyConstantRange(args, src)
			}
			return pyConstViewFuncs[name] && args.NamedChildCount() > 0 && pyConstantIterable(args.NamedChild(0), src, consts)
		case "attribute":
			attr := fn.ChildByFieldName("attribute")
			return attr != nil && pyConstViewMethods[pyText(attr, src)] && args.NamedChildCount() == 0 &&
				pyConstantIterable(fn.ChildByFieldName("object"), src, consts)
		}
	}
	return false
}

// pyConstantRange reports whether every argument of a range() call is an integer
// literal or an ALL_CAPS constant.
func pyConstantRange(args *sitter.Node, src []byte) bool {
	if args.NamedChildCount() == 0 {
		return false
	}
	for i := uint(0); i < uint(args.NamedChildCount()); i++ {
		a := args.NamedChild(i)
		switch kindOf(a) {
		case "integer":
		case "identifier":
			if !pyScreaming(pyText(a, src)) {
				return false
			}
		case "attribute":
			attr := a.ChildByFieldName("attribute")
			if attr == nil || !pyScreaming(pyText(attr, src)) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// pyMutators are the methods that change how many elements a list, set or dict
// holds.
var pyMutators = []string{".append(", ".extend(", ".insert(", ".add(", ".update(", ".pop(", ".remove(", ".discard(", ".setdefault(", ".clear("}

// pyConstLiteralLocals returns the names a function body assigns exactly once, to
// a list, tuple, set or dict literal, and never grows: no mutating call on the
// name, no element assignment, no augmented assignment.
//
// The mutation test is textual and so errs towards refusing.
func pyConstLiteralLocals(body *sitter.Node, src []byte) map[string]bool {
	if body == nil {
		return nil
	}
	assigned := map[string]int{}
	literal := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(n) {
		case "assignment", "augmented_assignment":
			if left := n.ChildByFieldName("left"); left != nil && kindOf(left) == "identifier" {
				name := pyText(left, src)
				assigned[name]++
				if right := n.ChildByFieldName("right"); kindOf(n) == "assignment" && right != nil {
					switch kindOf(right) {
					case "list", "tuple", "set", "dictionary":
						literal[name] = true
					}
				}
			}
		case "for_statement", "for_in_clause":
			// A loop target is rebound every iteration.
			var sc pyLoopScope
			pyBindTargetNames(n.ChildByFieldName("left"), src, &sc)
			for name := range sc.vars {
				assigned[name] += 2
			}
		}
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(body)
	var out map[string]bool
	text := string(src[body.StartByte():body.EndByte()])
	for name := range literal {
		if assigned[name] != 1 || strings.Contains(text, name+"[") {
			continue
		}
		grows := false
		for _, m := range pyMutators {
			grows = grows || strings.Contains(text, name+m)
		}
		if grows {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[name] = true
	}
	return out
}

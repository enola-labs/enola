package kotlinextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Calls on a receiver whose type the source declares.
//
//	class OfflineRepo(private val dao: TopicDao) { … }   // a constructor property
//	@Inject lateinit var network: NiaNetwork             // a property
//	fun sync(repo: TopicsRepository) { … }               // a parameter
//	val store: PrefsStore = open()                       // a typed local
//	val cache = MemoryCache()                            // a constructed local
//
// A call on a receiver has only ever been recorded by its bare member name, which
// is enough to say the member is used and says nothing about which one. Where the
// receiver's type is written down there is nothing to guess: `dao.insert(x)` is a
// call on a TopicDao, and an edge to TopicDao.insert is what lets anything that
// follows calls edges get from a repository to the Room or Retrofit method behind
// it.
//
// A type name is resolved the way a bare call is: through the file's imports, and
// otherwise as a declaration of the same package. Neither is checked here, since a
// file cannot see what the others declare; resolveTypedCalls keeps what names a
// type of this repository and drops the rest.

// The walker's notes to resolveTypedCalls. Neither leaves the extractor.
const (
	propTypedCalls = "typed_calls"
	// propSuperCandidates lists, for a type, the canonical name each of its
	// supertypes would have if this repository declares it.
	propSuperCandidates = "super_candidates"
)

// typedCall is one call whose receiver type is declared.
type typedCall struct {
	written string // as the in-loop metric records it: `dao.insert`
	typ     string // the canonical name the receiver's type would have
	method  string
}

// typeCandidate returns the canonical name a simple type name would have as a
// declaration of this repository, or "" when it is known to be another's.
func (w *astWalker) typeCandidate(simple string) string {
	if simple == "" || strings.Contains(simple, ".") {
		return ""
	}
	if target, ok := w.importMap[simple]; ok {
		return target // "" for an import from outside the repository
	}
	if kotlinBuiltins[simple] {
		return ""
	}
	return w.dir + "." + simple
}

// declaredType reads the type a declaration states: the written type, or with
// none written, the constructor it is initialised from (`val cache = Cache()`).
func (w *astWalker) declaredType(decl *sitter.Node) string {
	if t := lastTypeIdentifier(firstTypeChild(decl), w.src); t != "" {
		return w.typeCandidate(t)
	}
	return ""
}

// constructedType returns the type of a `Type(…)` initialiser among a
// declaration's children.
func (w *astWalker) constructedType(decl *sitter.Node) string {
	for i := uint(0); i < decl.NamedChildCount(); i++ {
		c := decl.NamedChild(i)
		if kindOf(c) != "call_expression" {
			continue
		}
		if callee := firstNamedChild(c); callee != nil && kotlinIsIdentifier(callee) {
			if name := nodeText(callee, w.src); isCapitalized(name) {
				return w.typeCandidate(name)
			}
		}
	}
	return ""
}

func firstIdentifierChild(n *sitter.Node) *sitter.Node {
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); kotlinIsIdentifier(c) {
			return c
		}
	}
	return nil
}

// bindProperty records what a property_declaration declares: `val x: T`,
// `val x = T(…)`.
func (w *astWalker) bindProperty(into map[string]string, prop *sitter.Node) {
	vd := findChildByKind(prop, "variable_declaration")
	if vd == nil {
		return
	}
	name := firstIdentifierChild(vd)
	if name == nil {
		return
	}
	t := w.declaredType(vd)
	if t == "" && firstTypeChild(vd) == nil {
		t = w.constructedType(prop)
	}
	if t != "" {
		into[nodeText(name, w.src)] = t
	} else {
		delete(into, nodeText(name, w.src)) // shadows a typed name with an untyped one
	}
}

// bindMembers fills the innermost type scope with the declared types of a class's
// constructor parameters and properties.
func (w *astWalker) bindMembers(class *sitter.Node) {
	if len(w.memberTypes) == 0 {
		return
	}
	into := w.memberTypes[len(w.memberTypes)-1]
	if pc := findChildByKind(class, "primary_constructor"); pc != nil {
		if cps := findChildByKind(pc, "class_parameters"); cps != nil {
			for i := uint(0); i < cps.NamedChildCount(); i++ {
				cp := cps.NamedChild(i)
				if kindOf(cp) != "class_parameter" {
					continue
				}
				if name := firstIdentifierChild(cp); name != nil {
					if t := w.declaredType(cp); t != "" {
						into[nodeText(name, w.src)] = t
					}
				}
			}
		}
	}
	body := findChildByKind(class, "class_body")
	if body == nil {
		return
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		if m := body.NamedChild(i); kindOf(m) == "property_declaration" {
			w.bindProperty(into, m)
		}
	}
}

// bindParameters records a function's typed parameters.
func (w *astWalker) bindParameters(fn *sitter.Node) {
	params := findChildByKind(fn, "function_value_parameters")
	if params == nil {
		return
	}
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if kindOf(p) != "parameter" {
			continue
		}
		if name := firstIdentifierChild(p); name != nil {
			if t := w.declaredType(p); t != "" {
				w.localTypes[nodeText(name, w.src)] = t
			}
		}
	}
}

// receiverType returns the canonical name the type of a call's receiver would
// have, or "".
func (w *astWalker) receiverType(recv *sitter.Node) string {
	if recv == nil {
		return ""
	}
	if kotlinIsIdentifier(recv) {
		name := nodeText(recv, w.src)
		if t, ok := w.localTypes[name]; ok {
			return t
		}
		for i := len(w.memberTypes) - 1; i >= 0; i-- {
			if t, ok := w.memberTypes[i][name]; ok {
				return t
			}
		}
		// `Analytics.log(…)`: a call on an object names its type.
		if isCapitalized(name) {
			return w.typeCandidate(name)
		}
		return ""
	}
	// `this.dao.insert(x)`
	if kindOf(recv) == "navigation_expression" && navReceiverIsThis(recv, w.src) && len(w.memberTypes) > 0 {
		if member, _ := calleeName(recv, w.src); member != "" {
			return w.memberTypes[len(w.memberTypes)-1][member]
		}
	}
	return ""
}

// noteTypedCall records a call on a receiver of declared type against the
// function being walked.
func (w *astWalker) noteTypedCall(written, typ, method string) {
	if w.metrics == nil || typ == "" {
		return
	}
	w.metrics.typedCalls = append(w.metrics.typedCalls, typedCall{written, typ, method})
}

// superCandidates returns the canonical names a type's supertypes would have.
func (w *astWalker) superCandidates(supertypes []string) []string {
	var out []string
	for _, st := range supertypes {
		if c := w.typeCandidate(st); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// resolveTypedCalls turns the walker's notes into edges, keeping what names a
// declaration of this repository.
//
// A supertype written by its simple name becomes an implements edge to the type
// it names, where this repository declares one: until now the target was the bare
// name, which matched no fact. A call on a receiver of declared type becomes a
// calls edge to the method the type, its companion, or one of its supertypes
// declares, and the same name replaces the written text in the in-loop lists.
func resolveTypedCalls(all []facts.Fact) {
	exists := make(map[string]bool, len(all))
	for i := range all {
		if all[i].Kind == facts.KindSymbol {
			exists[all[i].Name] = true
		}
	}
	supers := make(map[string][]string)
	for i := range all {
		f := &all[i]
		cands, ok := f.PropAny(propSuperCandidates).([]string)
		if !ok {
			continue
		}
		f.DelProp(propSuperCandidates)
		for _, c := range cands {
			if !exists[c] || c == f.Name {
				continue
			}
			supers[f.Name] = append(supers[f.Name], c)
			simple := c[strings.LastIndexByte(c, '.')+1:]
			for j := range f.Relations {
				if r := &f.Relations[j]; r.Kind == facts.RelImplements && r.Target == simple {
					r.Target = c
				}
			}
		}
	}
	declaring := func(typ, method string) string {
		seen := map[string]bool{}
		for queue := []string{typ}; len(queue) > 0; {
			t := queue[0]
			queue = queue[1:]
			if seen[t] {
				continue
			}
			seen[t] = true
			for _, owner := range []string{t, t + ".Companion"} {
				if exists[owner+"."+method] {
					return owner + "." + method
				}
			}
			queue = append(queue, supers[t]...)
		}
		return ""
	}

	for i := range all {
		f := &all[i]
		calls, ok := f.PropAny(propTypedCalls).([]typedCall)
		if !ok {
			continue
		}
		f.DelProp(propTypedCalls)
		rename := make(map[string]string)
		for _, c := range calls {
			if !exists[c.typ] {
				continue
			}
			target := declaring(c.typ, c.method)
			if target == "" || target == f.Name {
				continue
			}
			if !f.HasRelation(facts.RelCalls, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
			}
			rename[c.written] = target
		}
		for _, key := range []string{"calls_in_loop", "calls_in_scaling_loop"} {
			if list, ok := f.PropAny(key).([]string); ok {
				for j, c := range list {
					if to, ok := rename[c]; ok {
						list[j] = to
					}
				}
			}
		}
	}
}

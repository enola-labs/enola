package phpextractor

import (
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// An untyped parameter holds what its callers pass.
//
//	public function showGroupObject($group) { … $group->admin … }
//	$this->showGroupObject(Group::find($id));   // the only call of the name
//
// A relation read or a call on an untyped parameter named nothing: `$group->admin`
// may as well be a column of anything. The parameter is given the type of what its
// method's callers pass, and only when that is exact: every call site of the
// method's name in the repository has a receiver of known type, each one naming
// this method passes a value of the same declared class at that position, and no
// site could reach the method through a subclass's receiver. A call of the name on
// a receiver of unknown type, a callable array naming it, a `parent::` call, a
// spread or named argument, a route or hook handler, a parameter with a default,
// one passed by reference and one the body assigns leave it untyped.
//
// The walker writes such a parameter as paramMark, the method and its position
// (`$App\Http\C::show#0`), and notes every call site with arguments on one carrier
// fact per file; eloquentResult reads the mark once every file is read.

const (
	paramMark = "$"
	// kindCallSites is the carrier fact of a file's call sites. It never leaves the
	// extractor: dropCallSites removes it after binding.
	kindCallSites = "_php_call_sites"
	propCallSites = "_call_sites"
	// siteSep separates a site's method name, receiver type and argument types.
	siteSep = "\x1f"
)

// markUntypedParams types each untyped parameter of a method as its paramMark.
func (w *phpWalker) markUntypedParams(method string, params, body *sitter.Node) {
	if params == nil || strings.HasPrefix(method[strings.LastIndex(method, "::")+2:], "__") {
		return
	}
	var assigned map[string]bool
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if kindOf(p) != "simple_parameter" || p.ChildByFieldName("type") != nil ||
			p.ChildByFieldName("default_value") != nil || p.ChildByFieldName("reference_modifier") != nil {
			continue
		}
		name := variableName(p.ChildByFieldName("name"), w.src)
		if name == "" || w.paramTypes[name] != "" {
			continue
		}
		if assigned == nil {
			assigned = map[string]bool{}
			w.assignedNames(body, assigned)
		}
		if !assigned[name] {
			w.paramTypes[name] = paramMark + method + "#" + strconv.Itoa(int(i))
		}
	}
}

// assignedNames collects the variables a body assigns, binds in a foreach or a list,
// or takes by reference, closures included.
func (w *phpWalker) assignedNames(node *sitter.Node, out map[string]bool) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "assignment_expression", "reference_assignment_expression", "augmented_assignment_expression":
		w.variablesIn(node.ChildByFieldName("left"), out)
	case "foreach_statement":
		if v := node.NamedChild(1); v != nil && (node.ChildByFieldName("body") == nil || v.StartByte() != node.ChildByFieldName("body").StartByte()) {
			w.variablesIn(v, out)
		}
	case "by_ref", "unset_statement", "global_declaration", "static_variable_declaration":
		w.variablesIn(node, out)
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		w.assignedNames(node.NamedChild(i), out)
	}
}

// variablesIn collects the variable names a target names, a list's included; a
// property or element target (`$a->b = …`, `$a[] = …`) names none.
func (w *phpWalker) variablesIn(node *sitter.Node, out map[string]bool) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "variable_name":
		out[variableName(node, w.src)] = true
		return
	case "member_access_expression", "nullsafe_member_access_expression", "subscript_expression",
		"scoped_property_access_expression":
		return
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		w.variablesIn(node.NamedChild(i), out)
	}
}

// recordCallSite notes a call with arguments: the method's name, the receiver's type
// ("" when unknown) and each argument's, "" for one of unknown type. A spread, a
// named argument or a first-class callable makes every position unknown.
func (w *phpWalker) recordCallSite(call *sitter.Node, method, recv string) {
	args := call.ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() == 0 {
		return
	}
	site := []string{method, recv}
	for i := uint(0); i < args.NamedChildCount(); i++ {
		a := args.NamedChild(i)
		v := argValue(a)
		if kindOf(a) != "argument" || a.ChildByFieldName("name") != nil || a.ChildByFieldName("reference_modifier") != nil ||
			v == nil || kindOf(v) == "variadic_unpacking" {
			site = []string{method, ""}
			break
		}
		site = append(site, w.chainReceiver(v, w.localTypes))
	}
	w.callSites = append(w.callSites, strings.Join(site, siteSep))
}

// noteCallableArray notes `[$obj, 'method']` as a call of method on an unknown
// receiver: a callable array is called with arguments its site does not show.
func (w *phpWalker) noteCallableArray(node *sitter.Node) {
	var els []*sitter.Node
	for i := uint(0); i < node.NamedChildCount(); i++ {
		if el := node.NamedChild(i); kindOf(el) == "array_element_initializer" {
			els = append(els, el)
		}
	}
	if len(els) != 2 || els[1].NamedChildCount() != 1 {
		return
	}
	if name := stringLiteral(els[1].NamedChild(0), w.src); isIdentifier(name) {
		w.callSites = append(w.callSites, name+siteSep)
	}
}

// callSitesFact carries a file's call sites to the cross-file pass.
func (w *phpWalker) callSitesFact() []facts.Fact {
	if len(w.callSites) == 0 {
		return nil
	}
	return []facts.Fact{{Kind: kindCallSites, File: w.relFile, Props: map[string]any{propCallSites: w.callSites}}}
}

// dropCallSites removes the carrier facts once the call sites are read.
func dropCallSites(all []facts.Fact) []facts.Fact {
	out := all[:0]
	for i := range all {
		if all[i].Kind != kindCallSites {
			out = append(out, all[i])
		}
	}
	return out
}

// paramSites is what the parameter marks are read against.
type paramSites struct {
	// byName lists the call sites of each method name, each as receiver type then
	// argument types.
	byName map[string][][]string
	// handlers are the methods a route or a hook names.
	handlers map[string]bool
	memo     map[string]string
	busy     map[string]bool
}

func newParamSites() *paramSites {
	return &paramSites{byName: map[string][][]string{}, handlers: map[string]bool{}, memo: map[string]string{}, busy: map[string]bool{}}
}

// note reads a fact's call sites and the handlers it names.
func (p *paramSites) note(f *facts.Fact) {
	for _, rel := range f.Relations {
		if rel.Kind == facts.RelHandledBy {
			p.handlers[rel.Target] = true
		}
	}
	if f.Kind != kindCallSites {
		return
	}
	sites, _ := f.Props[propCallSites].([]string)
	for _, s := range sites {
		parts := strings.Split(s, siteSep)
		p.byName[parts[0]] = append(p.byName[parts[0]], parts[1:])
	}
}

// paramType is the class a parameter mark holds, "" when its callers do not say
// exactly.
func (r relationResolver) paramType(mark string, depth int) string {
	p := r.params
	if p == nil || r.ix == nil {
		return ""
	}
	if t, ok := p.memo[mark]; ok {
		return t
	}
	if p.busy[mark] {
		return ""
	}
	p.busy[mark] = true
	t := r.callersPass(mark, depth)
	delete(p.busy, mark)
	p.memo[mark] = t
	return t
}

func (r relationResolver) callersPass(mark string, depth int) string {
	hash := strings.LastIndexByte(mark, '#')
	if hash < 0 {
		return ""
	}
	decl := strings.TrimPrefix(mark[:hash], paramMark)
	idx, err := strconv.Atoi(mark[hash+1:])
	sep := strings.LastIndex(decl, "::")
	if err != nil || sep <= 0 || r.params.handlers[decl] {
		return ""
	}
	owner, name := decl[:sep], decl[sep+2:]
	trait := !r.ix.controllers[owner]
	found := ""
	for _, site := range r.params.byName[name] {
		recv := r.classOf(site[0], depth+1)
		if !r.ix.hasClass(recv) {
			return ""
		}
		if r.ix.declaringMethod(recv, name) != decl {
			// A receiver of an unrelated class runs another method. One the owner
			// descends from may be an instance of the owner.
			if trait || r.ix.descends(owner, recv) {
				return ""
			}
			continue
		}
		if idx+1 >= len(site) {
			return ""
		}
		arg := r.classOf(site[idx+1], depth+1)
		if !r.ix.hasClass(arg) || (found != "" && arg != found) {
			return ""
		}
		found = arg
	}
	return found
}

// classOf is the class or interface a receiver type names, through declared return
// types or as an Eloquent result; "" when it names none.
func (r relationResolver) classOf(t string, depth int) string {
	if t == "" || depth > maxMarkDepth {
		return ""
	}
	if typ := (returnMarks{ix: r.ix, returns: r.returns}).typeOf(t, depth); r.ix.hasClass(typ) || r.ix.interfaces[typ] {
		return typ
	}
	if class, kind := r.eloquentResult(t, depth); kind == ofModel && r.ix.hasClass(class) {
		return class
	}
	return ""
}

// descends reports whether class reaches ancestor through its parents.
func (ix *classIndex) descends(class, ancestor string) bool {
	seen := map[string]bool{class: true}
	frontier := []string{class}
	for len(frontier) > 0 {
		var next []string
		for _, c := range frontier {
			for _, ref := range ix.parents[c] {
				parent := ix.resolve(c, ref)
				if parent == ancestor {
					return true
				}
				if parent != "" && !seen[parent] {
					seen[parent] = true
					next = append(next, parent)
				}
			}
		}
		frontier = next
	}
	return false
}

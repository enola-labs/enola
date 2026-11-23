package phpextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// An Eloquent relation is declared as a method and used as a property
// (`$status->mentions`) or by name in an eager load (`with('mentions')`). Neither
// form calls the method in the source, so the relation read as dead. The symbol walk
// notes each property read and each eager-loaded name, and bindRelationReads turns
// the ones naming a relation method into calls edges once every file is read.
const (
	// propPropertyReads lists, on a function or method, the properties it reads
	// (`name`) and those whose receiver type is declared (`Type::name`).
	propPropertyReads = "property_reads"
	// propRelationMethod marks a method returning `$this->hasMany(...)` or another
	// relation builder.
	propRelationMethod = "relation_method"
)

// relationBuilders are the builders a relation method returns. morphTo names no
// related model, so eloquentRelations leaves it out; it is a relation all the same.
func isRelationBuilder(method string) bool {
	return eloquentRelations[method] || method == "morphTo"
}

// eagerLoadMethods are the query and model methods whose string arguments name
// relations.
var eagerLoadMethods = map[string]bool{
	"with": true, "without": true, "withOnly": true, "load": true, "loadMissing": true,
	"loadCount": true, "loadExists": true, "loadSum": true, "loadMax": true, "loadMin": true, "loadAvg": true,
	"has": true, "orHas": true, "doesntHave": true, "orDoesntHave": true,
	"whereHas": true, "orWhereHas": true, "whereDoesntHave": true, "orWhereDoesntHave": true, "withWhereHas": true,
	"whereRelation": true, "orWhereRelation": true,
	"withCount": true, "withExists": true, "withSum": true, "withMax": true, "withMin": true, "withAvg": true,
}

// recordRelationMethod marks the enclosing method as a relation when it calls a
// relation builder on `$this`.
func (w *phpWalker) recordRelationMethod(call *sitter.Node, ownerIdx int, method string) {
	if !isRelationBuilder(method) || ownerIdx < 0 || len(w.typeStack) == 0 {
		return
	}
	if obj := call.ChildByFieldName("object"); obj == nil || phpText(obj, w.src) != "$this" {
		return
	}
	if owner := &w.out[ownerIdx]; owner.Kind == facts.KindSymbol && owner.PropString("symbol_kind") == facts.SymbolMethod {
		owner.Props[propRelationMethod] = true
	}
}

// recordPropertyRead notes a `$obj->name` read. An assignment's target is a write
// and is left out.
func (w *phpWalker) recordPropertyRead(node *sitter.Node, ownerIdx int) {
	name := node.ChildByFieldName("name")
	if ownerIdx < 0 || name == nil || kindOf(name) != "name" {
		return
	}
	if parent := node.Parent(); parent != nil && kindOf(parent) == "assignment_expression" {
		if left := parent.ChildByFieldName("left"); left != nil && left.StartByte() == node.StartByte() && left.EndByte() == node.EndByte() {
			return
		}
	}
	w.addRead(ownerIdx, w.plainReceiver(node.ChildByFieldName("object")), phpText(name, w.src))
}

// recordEagerLoads notes the relations an eager load or relation query names:
// `with('author.profile')`, `withCount(['comments as total'])`,
// `with(['posts' => fn ($q) => ...])`. typ is the receiver's class when declared; it
// types the first segment of each path; without it nothing is recorded, so a view's
// or a redirect's `with('key', $value)` names no relation.
func (w *phpWalker) recordEagerLoads(call *sitter.Node, ownerIdx int, method, typ string) {
	if !eagerLoadMethods[method] || ownerIdx < 0 || typ == "" {
		return
	}
	var args []*sitter.Node
	if list := call.ChildByFieldName("arguments"); list != nil {
		for i := uint(0); i < list.NamedChildCount(); i++ {
			if a := list.NamedChild(i); kindOf(a) == "argument" && a.ChildByFieldName("name") == nil && a.NamedChildCount() > 0 {
				args = append(args, a.NamedChild(a.NamedChildCount()-1))
			}
		}
	}
	for _, arg := range args {
		switch kindOf(arg) {
		case "string", "encapsed_string":
			w.addRelationPath(ownerIdx, typ, stringLiteral(arg, w.src))
		case "array_creation_expression":
			for i := uint(0); i < arg.NamedChildCount(); i++ {
				el := arg.NamedChild(i)
				if kindOf(el) != "array_element_initializer" || el.NamedChildCount() == 0 {
					continue
				}
				// A keyed element names its relation in the key and constrains it with a
				// closure; a key with any other value is data (`with(['status' => 'Saved'])`).
				if el.NamedChildCount() > 1 && !isClosure(el.NamedChild(el.NamedChildCount()-1)) {
					continue
				}
				w.addRelationPath(ownerIdx, typ, stringLiteral(el.NamedChild(0), w.src))
			}
		}
	}
}

func isClosure(node *sitter.Node) bool {
	switch kindOf(node) {
	case "anonymous_function", "arrow_function":
		return true
	}
	return false
}

// addRelationPath notes a dotted relation path, without an `as alias` suffix or a
// `:columns` selection, on a receiver of declared type. bindRelationReads follows
// each segment through the class the previous relation names.
func (w *phpWalker) addRelationPath(ownerIdx int, typ, path string) {
	if i := strings.Index(strings.ToLower(path), " as "); i >= 0 {
		path = path[:i]
	}
	if i := strings.IndexByte(path, ':'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimSpace(path)
	for _, seg := range strings.Split(path, ".") {
		if !isIdentifier(seg) {
			return
		}
	}
	w.addRead(ownerIdx, typ, path)
}

// plainReceiver is receiverOf for a receiver whose class is known without a later
// return-type lookup, "" otherwise.
func (w *phpWalker) plainReceiver(obj *sitter.Node) string {
	if obj == nil {
		return ""
	}
	typ := w.receiverOf(obj, w.localTypes)
	if strings.HasSuffix(typ, callMark) {
		return ""
	}
	return typ
}

// addRead appends a read, `Type::name` or `Type::a.b` for a path, to the owner's
// property reads, each once. A read on a receiver of unknown type is not recorded:
// without the model's columns, `$x->status` may as well be a column as a relation.
func (w *phpWalker) addRead(ownerIdx int, typ, name string) {
	if typ == "" || name == "" {
		return
	}
	entry := typ + "::" + name
	reads, _ := w.out[ownerIdx].Props[propPropertyReads].([]string)
	for _, r := range reads {
		if r == entry {
			return
		}
	}
	w.out[ownerIdx].Props[propPropertyReads] = append(reads, entry)
}

func isIdentifier(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return s != ""
}

// bindRelationReads adds a calls edge for each property read or eager-loaded path
// that reaches a relation method, in place, and removes the carrier props.
//
// Every read has a receiver of declared type. It reaches that class's relation
// (declared on it, a parent or a trait) or nothing: a column of that model shares the
// name with no relation of its own. A path's later segments are looked up on the
// class the previous relation names (eloquent_target); a morphTo names none, and the
// path stops there.
func bindRelationReads(allFacts []facts.Fact) {
	relations := map[string]string{}
	reads := false
	for i := range allFacts {
		f := &allFacts[i]
		if f.Props == nil {
			continue
		}
		if f.PropBool(propRelationMethod) {
			delete(f.Props, propRelationMethod)
			relations[f.Name] = f.PropString(propEloquentTarget)
		}
		if _, ok := f.Props[propPropertyReads]; ok {
			reads = true
		}
	}
	if !reads {
		return
	}
	var ix *classIndex
	if len(relations) > 0 {
		ix = newClassIndex(allFacts)
	}
	for i := range allFacts {
		f := &allFacts[i]
		if (f.Kind != facts.KindSymbol && f.Kind != facts.KindFileRef) || f.Props == nil {
			continue
		}
		entries, _ := f.Props[propPropertyReads].([]string)
		delete(f.Props, propPropertyReads)
		if ix == nil {
			continue
		}
		for _, r := range entries {
			sep := strings.LastIndex(r, "::")
			if sep <= 0 {
				continue
			}
			class := r[:sep]
			for _, seg := range strings.Split(r[sep+2:], ".") {
				if class == "" || !ix.hasClass(class) {
					break
				}
				m := ix.declaringMethod(class, seg)
				target, ok := relations[m]
				if !ok {
					break
				}
				if m != f.Name && !f.HasRelation(facts.RelCalls, m) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: m})
				}
				class = target
			}
		}
	}
}

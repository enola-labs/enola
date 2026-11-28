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
	// propPropertyReads lists, on a function or method, the properties it reads on
	// a receiver of known type, `Type::name`, or `Type::a.b` for `$x->a->b`.
	propPropertyReads = "property_reads"
	// propEagerLoads lists the relation paths it names in an eager load or a relation
	// query, `Type::a.b`, where Type may be a query of the model as well as one.
	propEagerLoads = "eager_loads"
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
	// `$media->status->poll` reads poll on what status reads: the path is followed
	// through each relation's model once every file is read.
	path := []string{phpText(name, w.src)}
	obj := node.ChildByFieldName("object")
	for obj != nil {
		if typ := w.receiverOf(obj, w.localTypes); typ != "" {
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			w.addRead(ownerIdx, propPropertyReads, typ, strings.Join(path, "."))
			return
		}
		switch kindOf(obj) {
		case "member_access_expression", "nullsafe_member_access_expression":
		default:
			return
		}
		seg := obj.ChildByFieldName("name")
		if seg == nil || kindOf(seg) != "name" {
			return
		}
		path = append(path, phpText(seg, w.src))
		obj = obj.ChildByFieldName("object")
	}
}

// recordEagerLoads notes the relations an eager load or relation query names:
// `with('author.profile')`, `withCount(['comments as total'])`,
// `with(['posts' => fn ($q) => ...])`. typ is the receiver's class when declared; it
// types the first segment of each path, and may be a query's result, marked as in
// factoryreturns.go; without it nothing is recorded, so a view's or a redirect's
// `with('key', $value)` names no relation.
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
	w.addRead(ownerIdx, propEagerLoads, typ, path)
}

// addRead appends a read, `Type::name` or `Type::a.b` for a path, to the owner's
// prop, each once. A read on a receiver of unknown type is not recorded: without the
// model's columns, `$x->status` may as well be a column as a relation.
func (w *phpWalker) addRead(ownerIdx int, prop, typ, name string) {
	if typ == "" || name == "" {
		return
	}
	entry := typ + "::" + name
	reads, _ := w.out[ownerIdx].Props[prop].([]string)
	for _, r := range reads {
		if r == entry {
			return
		}
	}
	w.out[ownerIdx].Props[prop] = append(reads, entry)
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
// Every read has a receiver of known type. It reaches that class's relation
// (declared on it, a parent or a trait) or nothing: a column of that model shares the
// name with no relation of its own. A path's later segments are looked up on the
// class the previous relation names (eloquent_target); a morphTo names none, and the
// path stops there. A receiver that is a query's result is read by eloquentResult.
// The resolver is returned for bindReceiverCalls, which reads query results the same
// way; its class index is nil when no relation is declared.
func bindRelationReads(allFacts []facts.Fact) relationResolver {
	r := relationResolver{relations: map[string]string{}, toMany: map[string]bool{}, returns: map[string]string{}, models: map[string]bool{},
		params: newParamSites()}
	reads := false
	for i := range allFacts {
		f := &allFacts[i]
		r.params.note(f)
		if f.Props == nil {
			continue
		}
		if f.PropBool(propRelationMethod) {
			delete(f.Props, propRelationMethod)
			r.relations[f.Name] = f.PropString(propEloquentTarget)
			r.toMany[f.Name] = toManyRelations[f.PropString(propEloquentRelation)]
		}
		// Read here, removed by bindReceiverCalls.
		if t, ok := f.Props[propReturnType].(string); ok {
			r.returns[f.Name] = t
		}
		if _, ok := f.Props[propPropertyReads]; ok {
			reads = true
		}
		if _, ok := f.Props[propEagerLoads]; ok {
			reads = true
		}
	}
	if !reads {
		return r
	}
	if len(r.relations) > 0 {
		r.ix = newClassIndex(allFacts)
	}
	for i := range allFacts {
		f := &allFacts[i]
		if (f.Kind != facts.KindSymbol && f.Kind != facts.KindFileRef) || f.Props == nil {
			continue
		}
		props, _ := f.Props[propPropertyReads].([]string)
		loads, _ := f.Props[propEagerLoads].([]string)
		delete(f.Props, propPropertyReads)
		delete(f.Props, propEagerLoads)
		if r.ix == nil {
			continue
		}
		for n, entries := range [][]string{props, loads} {
			for _, e := range entries {
				sep := strings.LastIndex(e, "::")
				if sep <= 0 {
					continue
				}
				class, kind := r.eloquentResult(e[:sep], 0)
				// A property of a query or a collection is none of its model's relations;
				// either loads them.
				if kind != ofModel && n == 0 {
					continue
				}
				for _, seg := range strings.Split(e[sep+2:], ".") {
					if class == "" || !r.ix.hasClass(class) {
						break
					}
					m := r.ix.declaringMethod(class, seg)
					target, ok := r.relations[m]
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
	return r
}

// relationResolver holds what bindRelationReads looks receivers up in.
type relationResolver struct {
	ix *classIndex
	// relations maps each relation method to the model it names, "" for a morphTo.
	relations map[string]string
	// toMany marks the relation methods whose property is a collection.
	toMany map[string]bool
	// returns maps a method to the class its return type declares.
	returns map[string]string
	models  map[string]bool
	// params types an untyped parameter by its callers (paramcallers.go).
	params *paramSites
}

// toManyRelations are the relation builders whose property is a collection.
var toManyRelations = map[string]bool{
	"hasMany": true, "belongsToMany": true, "hasManyThrough": true,
	"morphMany": true, "morphToMany": true, "morphedByMany": true,
}

// What a receiver type names: one model of its class, a query of it, or a
// collection of its models.
type resultKind int

const (
	ofModel resultKind = iota
	ofQuery
	ofMany
)

// queryMethods are the Eloquent builder methods a model class, a model or a query
// forwards to a query of the same model.
var queryMethods = map[string]bool{
	"query": true, "newQuery": true, "on": true, "onWriteConnection": true,
	"select": true, "addSelect": true, "distinct": true,
	"latest": true, "oldest": true, "orderBy": true, "orderByDesc": true, "reorder": true, "inRandomOrder": true,
	"groupBy": true, "having": true, "limit": true, "take": true, "skip": true, "offset": true, "forPage": true,
	"join": true, "leftJoin": true, "rightJoin": true, "crossJoin": true,
	"withTrashed": true, "onlyTrashed": true, "withoutTrashed": true,
	"withoutGlobalScope": true, "withoutGlobalScopes": true,
	"lockForUpdate": true, "sharedLock": true, "when": true, "unless": true,
}

// resultMethods are those that fetch, create or reload one model.
var resultMethods = map[string]bool{
	"find": true, "findOrFail": true, "findOrNew": true, "findOr": true,
	"first": true, "firstOrFail": true, "firstOrNew": true, "firstOrCreate": true, "firstOr": true,
	"firstWhere": true, "sole": true, "create": true, "forceCreate": true, "updateOrCreate": true,
	"make": true, "fresh": true, "refresh": true, "replicate": true,
	"load": true, "loadMissing": true, "loadCount": true,
}

// manyMethods are those that fetch a collection of models.
var manyMethods = map[string]bool{
	"get": true, "all": true, "findMany": true, "paginate": true, "simplePaginate": true, "cursorPaginate": true,
	"cursor": true, "lazy": true, "lazyById": true,
}

// collectionMethods are the collection methods that keep a collection of the same
// models, and collectionItems those that take one of them.
var (
	collectionMethods = map[string]bool{
		"filter": true, "reject": true, "where": true, "whereIn": true, "whereNotIn": true, "whereNull": true,
		"whereNotNull": true, "sortBy": true, "sortByDesc": true, "sortKeys": true, "values": true, "unique": true,
		"take": true, "skip": true, "slice": true, "reverse": true, "keyBy": true, "shuffle": true,
		"load": true, "loadMissing": true, "loadCount": true, "fresh": true, "except": true, "only": true,
	}
	collectionItems = map[string]bool{
		"first": true, "last": true, "firstWhere": true, "firstOrFail": true, "sole": true, "find": true,
		"pop": true, "shift": true,
	}
)

// eloquentResult is the class a receiver type names, and what of it. A plain type
// is one model of its class. A marked type (factoryreturns.go) is read mark by mark:
//
//   - a method's result: a method the repository declares yields its declared return
//     type, or a query of its model when it is a relation; a method of the Eloquent
//     builder the model does not declare, called on a model class, a model or its
//     query, yields a query of the model (a `whereX` or a scope the model declares
//     as `scopeX` included), one of its models, or a collection of them; a collection
//     method keeps the collection or takes one model of it;
//   - a property read: a relation's model, or a collection of them for a to-many
//     relation;
//   - an element: one model of a collection or a query.
//
// Anything else is "".
func (r relationResolver) eloquentResult(t string, depth int) (string, resultKind) {
	if depth > maxMarkDepth {
		return "", ofModel
	}
	if inner, ok := strings.CutSuffix(t, elementMark); ok {
		if class, kind := r.eloquentResult(inner, depth+1); kind != ofModel {
			return class, ofModel
		}
		return "", ofModel
	}
	mark := ""
	switch {
	case strings.HasSuffix(t, callMark):
		mark = callMark
	case strings.HasSuffix(t, readMark):
		mark = readMark
	case strings.HasPrefix(t, paramMark):
		return r.paramType(t, depth+1), ofModel
	default:
		return t, ofModel
	}
	path := strings.TrimSuffix(t, mark)
	sep := strings.LastIndex(path, "::")
	if sep <= 0 {
		return "", ofModel
	}
	class, kind := r.eloquentResult(path[:sep], depth+1)
	method := path[sep+2:]
	if class == "" || !r.ix.hasClass(class) || method == "" {
		return "", ofModel
	}
	if kind == ofMany {
		switch {
		case mark != callMark:
		case collectionMethods[method]:
			return class, ofMany
		case collectionItems[method]:
			return class, ofModel
		}
		return "", ofModel
	}
	if mark == readMark {
		if kind != ofModel {
			return "", ofModel
		}
		decl := r.ix.declaringMethod(class, method)
		target, ok := r.relations[decl]
		if !ok || target == "" {
			return "", ofModel
		}
		if r.toMany[decl] {
			return target, ofMany
		}
		return target, ofModel
	}
	if decl := r.ix.declaringMethod(class, method); decl != "" {
		if target, ok := r.relations[decl]; ok {
			return target, ofQuery
		}
		return r.returns[decl], ofModel
	}
	if !r.isModel(class) {
		return "", ofModel
	}
	switch {
	case resultMethods[method]:
		return class, ofModel
	case manyMethods[method]:
		return class, ofMany
	case queryMethods[method], eagerLoadMethods[method], strings.HasPrefix(method, "where"), strings.HasPrefix(method, "orWhere"):
		return class, ofQuery
	case r.ix.declaringMethod(class, "scope"+strings.ToUpper(method[:1])+method[1:]) != "":
		return class, ofQuery
	}
	return "", ofModel
}

func (r relationResolver) isModel(class string) bool {
	known, seen := r.models[class]
	if !seen {
		known = r.ix.extendsEloquent(class, map[string]bool{})
		r.models[class] = known
	}
	return known
}

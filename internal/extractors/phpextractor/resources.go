package phpextractor

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// resourceAction is one route of a resource controller. member routes address one
// record and carry its parameter.
type resourceAction struct {
	name, method, suffix string
	member               bool
}

// resourceActions are Laravel's seven, in the order it registers them. update is one
// route answering to both PUT and PATCH, so it is listed once per verb.
var resourceActions = []resourceAction{
	{"index", "GET", "", false},
	{"create", "GET", "/create", false},
	{"store", "POST", "", false},
	{"show", "GET", "", true},
	{"edit", "GET", "/edit", true},
	{"update", "PUT", "", true},
	{"update", "PATCH", "", true},
	{"destroy", "DELETE", "", true},
}

// resourceOptions is what narrows or renames a resource's routes: the fluent
// ->only / ->except / ->parameters / ->shallow, and the same keys in the options
// array passed as the third argument.
type resourceOptions struct {
	only, except map[string]bool
	parameters   map[string]string
	shallow      bool
}

// serves reports whether the resource registers an action.
func (o resourceOptions) serves(action string) bool {
	if o.only != nil && !o.only[action] {
		return false
	}
	return !o.except[action]
}

// resourceOptionsOf reads the options of one resource registration from its chain
// and its options array.
func (w *laravelRouteWalker) resourceOptionsOf(calls []chainCall, optsArg *sitter.Node) resourceOptions {
	var o resourceOptions
	if optsArg != nil && kindOf(optsArg) == "array_creation_expression" {
		o.only = stringSet(w.stringList(arrayValue(optsArg, "only", w.src)))
		o.except = stringSet(w.stringList(arrayValue(optsArg, "except", w.src)))
		o.parameters = arrayKeyValues(arrayValue(optsArg, "parameters", w.src), w.src)
		o.shallow = phpText(arrayValue(optsArg, "shallow", w.src), w.src) == "true"
	}
	for _, c := range calls {
		switch c.method {
		case "only":
			o.only = stringSet(w.argStrings(c.args))
		case "except":
			o.except = stringSet(w.argStrings(c.args))
		case "parameters":
			o.parameters = arrayKeyValues(positionalArg(c.args, 0), w.src)
		case "parameter":
			if o.parameters == nil {
				o.parameters = map[string]string{}
			}
			o.parameters[positionalString(c.args, 0, w.src)] = positionalString(c.args, 1, w.src)
		case "shallow":
			o.shallow = true
		}
	}
	return o
}

// emitResources expands Route::resources / apiResources, whose first argument maps
// each resource name to its controller.
func (w *laravelRouteWalker) emitResources(node *sitter.Node, stack []phpRouteScope, calls []chainCall, args *sitter.Node, apiOnly bool) {
	list := positionalArg(args, 0)
	if list == nil || kindOf(list) != "array_creation_expression" {
		return
	}
	for i := uint(0); i < list.ChildCount(); i++ {
		el := list.Child(i)
		if kindOf(el) != "array_element_initializer" || el.NamedChildCount() < 2 {
			continue
		}
		name := stringLiteral(el.NamedChild(0), w.src)
		controller := w.classArg(el.NamedChild(el.NamedChildCount() - 1))
		w.emitResource(node, stack, calls, name, controller, positionalArg(args, 1), apiOnly)
	}
}

// emitResource expands a Route::resource / apiResource declaration into the routes
// Laravel registers for it.
//
// The name is a path: `admin/photos` serves photos under /admin, and
// `albums.songs` nests songs under one album, at /albums/{album}/songs/{song}.
// Each record is addressed by a parameter named after the singular of its segment.
// ->only and ->except decide which of the seven actions exist, and a route for an
// action the application never registered is a route it does not serve.
func (w *laravelRouteWalker) emitResource(node *sitter.Node, stack []phpRouteScope, calls []chainCall, name, controller string, optsArg *sitter.Node, apiOnly bool) {
	name = strings.Trim(name, "/")
	if name == "" {
		return
	}
	opts := w.resourceOptionsOf(calls, optsArg)

	prefix := ""
	if i := strings.LastIndex(name, "/"); i >= 0 {
		prefix, name = "/"+name[:i], name[i+1:]
	}
	segments := strings.Split(name, ".")
	last := segments[len(segments)-1]
	if last == "" {
		return
	}
	// collection is where the records live; member is where one of them does. A
	// shallow nested resource addresses a record without its parents.
	collection := prefix
	for _, seg := range segments[:len(segments)-1] {
		collection += "/" + seg + "/{" + opts.parameterFor(seg) + "}"
	}
	collection += "/" + last
	member := collection + "/{" + opts.parameterFor(last) + "}"
	if opts.shallow && len(segments) > 1 {
		member = prefix + "/" + last + "/{" + opts.parameterFor(last) + "}"
	}

	for _, a := range resourceActions {
		if apiOnly && (a.name == "create" || a.name == "edit") {
			continue // apiResource omits the HTML form endpoints
		}
		if !opts.serves(a.name) {
			continue
		}
		path := collection
		if a.member {
			path = member
		}
		handler := ""
		if controller != "" {
			handler = controller + "::" + a.name
		}
		w.emitRoute(node, stack, a.method, path+a.suffix, handler, "", a.name)
	}
}

// parameterFor names the route parameter of a resource segment: the one ->parameters
// gives it, else the singular of the segment with dashes as underscores.
func (o resourceOptions) parameterFor(segment string) string {
	if p := o.parameters[segment]; p != "" {
		return p
	}
	return strings.ReplaceAll(singular(segment), "-", "_")
}

// singular applies the regular English singular. Laravel's inflector also knows the
// irregular ones (people, media, children); those keep the name they were written
// with, which is the wrong parameter name on a path that is otherwise right.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "ches"),
		strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "ss"), strings.HasSuffix(s, "us"), strings.HasSuffix(s, "is"):
		return s
	case strings.HasSuffix(s, "s") && len(s) > 1:
		return s[:len(s)-1]
	}
	return s
}

// argStrings returns the strings a call was given, as one array or as separate
// arguments: ->only(['index', 'show']) and ->only('index', 'show').
func (w *laravelRouteWalker) argStrings(args *sitter.Node) []string {
	if first := positionalArg(args, 0); first != nil && kindOf(first) == "array_creation_expression" {
		return arrayStringElements(first, w.src)
	}
	var out []string
	for i := 0; ; i++ {
		arg := positionalArg(args, i)
		if arg == nil {
			return out
		}
		if s := stringLiteral(arg, w.src); s != "" {
			out = append(out, s)
		}
	}
}

// stringList returns the strings of an options value: an array of them, or one.
func (w *laravelRouteWalker) stringList(node *sitter.Node) []string {
	if node == nil {
		return nil
	}
	if kindOf(node) == "array_creation_expression" {
		return arrayStringElements(node, w.src)
	}
	if s := stringLiteral(node, w.src); s != "" {
		return []string{s}
	}
	return nil
}

// stringSet returns the set of a list, nil for an empty one so that an absent
// ->only reads as no restriction.
func stringSet(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set
}

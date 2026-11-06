package rubyextractor

import (
	"path/filepath"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// jsonapiResolver answers the questions a `jsonapi_resources` declaration cannot
// answer by itself: which resource class it names, what relationships that class
// declares, and which resource serves each related route.
//
// It is four hops, and every one of them may end in nothing:
//
//  1. the route scope's module path plus the declared name locate one resource file
//  2. that file's has_one/has_many declarations give the relationship routes
//  3. a relationship's target class comes from its own class_name:, or failing
//     that from the model association JSONAPI::RelationshipBuilder reflects on
//  4. the target class's declared route name is the controller that serves it
//
// Nothing is a legal answer at each hop. An unresolved declaration keeps its
// RESTful routes and stays counted; an unresolved relationship keeps its routes
// and loses only its handler. The value sits at hop 2 and the risk sits below it,
// which is what makes a chain this long affordable.
type jsonapiResolver struct {
	repoPath string
	// resourceFiles maps "<module path>/<singular name>" to a resource class file.
	// The module path is part of the key because seven of the monolith's resource
	// classes share a basename across api/v1, api/partner/v1 and api/channel/v1.
	resourceFiles map[string]string
	// modelFiles maps an underscored model class name — including any namespace,
	// as in "custom_field/picked_custom_field" — to its file.
	modelFiles map[string]string
	// declaredTypes maps "<module path>/<singularized declared name>" to the name
	// as declared. It exists so the plural is read off config/routes.rb rather
	// than computed: `scorecard_criterium` pluralizes to `scorecard_criteria` by
	// no rule a suffix table holds, and inventing an English pluralizer to serve
	// one irregular is how a handler becomes quietly wrong.
	declaredTypes map[string]string
	relCache      map[string]*jsonapiResourceClass
	modelCache    map[string]map[string]string
}

// jsonapiRelationship is one has_one/has_many on a resource class.
type jsonapiRelationship struct {
	name      string
	toMany    bool
	className string
}

// jsonapiResourceClass is what a resource file says that routing depends on.
type jsonapiResourceClass struct {
	relationships []jsonapiRelationship
	modelName     string
	immutable     bool
}

func newJsonapiResolver(repoPath string, files []string) *jsonapiResolver {
	r := &jsonapiResolver{
		repoPath:      repoPath,
		resourceFiles: map[string]string{},
		modelFiles:    map[string]string{},
		declaredTypes: map[string]string{},
		relCache:      map[string]*jsonapiResourceClass{},
		modelCache:    map[string]map[string]string{},
	}
	for _, relFile := range files {
		slash := filepath.ToSlash(relFile)
		switch {
		case strings.HasSuffix(slash, "_resource.rb"):
			if key, ok := resourceKey(slash); ok {
				r.resourceFiles[key] = relFile
			}
		case strings.HasPrefix(slash, "app/models/") && strings.HasSuffix(slash, ".rb"):
			name := strings.TrimSuffix(strings.TrimPrefix(slash, "app/models/"), ".rb")
			r.modelFiles[name] = relFile
		}
	}
	return r
}

// resourceKey turns app/resources/api/v1/activity_resource.rb into api/v1/activity.
func resourceKey(slash string) (string, bool) {
	idx := strings.Index(slash, "/resources/")
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSuffix(slash[idx+len("/resources/"):], "_resource.rb")
	if rest == "" {
		return "", false
	}
	return rest, true
}

// declare records a declaration so a later relationship can name it as a handler.
func (r *jsonapiResolver) declare(modulePath, name string) {
	r.declaredTypes[joinModule(modulePath, singularize(name))] = name
}

func joinModule(modulePath, name string) string {
	if modulePath == "" {
		return name
	}
	return modulePath + "/" + name
}

// resourceClass returns what the resource class a declaration names says, or nil
// when the name does not locate exactly one file.
func (r *jsonapiResolver) resourceClass(modulePath, declared string) *jsonapiResourceClass {
	key := joinModule(modulePath, singularize(declared))
	relFile, ok := r.resourceFiles[key]
	if !ok {
		return nil
	}
	if cached, seen := r.relCache[relFile]; seen {
		return cached
	}
	parsed := parseResourceClass(filepath.Join(r.repoPath, relFile))
	r.relCache[relFile] = parsed
	return parsed
}

// controllerFor names the controller serving a declaration, which is the type
// its plural declaration recorded — plural even when the declaration is
// singular, because JSONAPI derives the type from the class name either way.
func (r *jsonapiResolver) controllerFor(modulePath, declared string) string {
	plural, ok := r.declaredTypes[joinModule(modulePath, singularize(declared))]
	if !ok {
		return ""
	}
	return joinModule(modulePath, plural)
}

// handlerFor names the controller that serves a relationship's related route.
func (r *jsonapiResolver) handlerFor(modulePath string, rel jsonapiRelationship, owner *jsonapiResourceClass, ownerName string) string {
	className := rel.className
	if className == "" {
		named, stated := r.associationClassName(owner, ownerName, rel.name)
		if stated {
			className = named
		}
		if stated && named == "" {
			// The model states an association whose class only the intermediate
			// model knows — a `through:` with no class_name:. That is one hop past
			// what this resolver reaches, so it answers nothing rather than
			// falling back to a name ActiveRecord would not have derived.
			return ""
		}
	}
	if className == "" {
		// Neither the resource nor the model named a class, which is the ordinary
		// case rather than a failure: ActiveRecord derives the target from the
		// association name, and JSONAPI's own default is the same string. The
		// fail-closed gate is hop 4, where a name nothing declares resolves to
		// nothing.
		base := rel.name
		if rel.toMany {
			base = singularize(base)
		}
		className = camelizeClass(base)
	}
	declared, ok := r.declaredTypes[joinModule(modulePath, underscoreClass(className))]
	if !ok {
		return ""
	}
	return joinModule(modulePath, declared)
}

// associationClassName reads the target class off the model's association, which
// is what the gem does when the resource declares no class_name of its own.
// The second return distinguishes "the model said something about this
// association" from "the model said nothing", which are different answers: the
// first is authoritative even when it is empty, the second means the caller
// should use the name ActiveRecord would derive.
func (r *jsonapiResolver) associationClassName(owner *jsonapiResourceClass, ownerName, relName string) (string, bool) {
	model := ownerName
	if owner != nil && owner.modelName != "" {
		model = underscoreClass(owner.modelName)
	}
	relFile, ok := r.modelFiles[model]
	if !ok {
		return "", false
	}
	associations, cached := r.modelCache[relFile]
	if !cached {
		associations = parseModelAssociations(filepath.Join(r.repoPath, relFile))
		r.modelCache[relFile] = associations
	}
	className, stated := associations[relName]
	return className, stated
}

// writeActions are the actions an immutable resource class does not serve.
var writeActions = map[string]bool{"create": true, "update": true, "destroy": true}

func parseResourceClass(path string) *jsonapiResourceClass {
	out := &jsonapiResourceClass{}
	eachCall(path, func(method string, args *sitter.Node, src []byte) {
		switch {
		case method == "immutable" && args == nil:
			out.immutable = true
		case method == "model_name":
			if name := firstStringArg(args, src); name != "" {
				out.modelName = name
			}
		default:
			toMany, isRelationship := relationshipMacros[method]
			if !isRelationship {
				return
			}
			// `has_one :a, :b` declares two, which is why every symbol counts.
			for _, name := range positionalSymbols(args, src) {
				out.relationships = append(out.relationships, jsonapiRelationship{
					name: name, toMany: toMany, className: pairString(args, "class_name", src),
				})
			}
		}
	})
	return out
}

// parseModelAssociations records the associations whose target the relationship
// name does not already imply: the ones that declare a class_name:, and the
// `through:` ones, which are entered with an empty value meaning "stated, and
// not derivable here". An ordinary association is deliberately absent, so the
// caller falls back to the class ActiveRecord derives from its name.
func parseModelAssociations(path string) map[string]string {
	out := map[string]string{}
	eachCall(path, func(method string, args *sitter.Node, src []byte) {
		if !associationMacros[method] {
			return
		}
		className := pairString(args, "class_name", src)
		through := findPairValue(args, "through", src) != nil
		if className == "" && !through {
			return
		}
		if className == "" {
			// A `through:` association takes its class from the source association,
			// which `source:` names and which otherwise defaults to this
			// association's own name. Reading `source:` is reading a declared name,
			// not guessing one — and where it is absent the derived default is
			// already what the caller would have used.
			source := pairSymbol(args, "source", src)
			if source == "" {
				return
			}
			className = camelizeClass(singularize(source))
		}
		for _, name := range positionalSymbols(args, src) {
			out[name] = className
		}
	})
	return out
}

// jsonapiRelationshipRoutes describes the routes one relationship serves. The
// action names are the gem's own, so a reader who greps the gem finds them.
var jsonapiRelationshipRoutes = []restAction{
	{name: "show_relationship", method: "GET"},
	{name: "update_relationship", method: "PATCH"},
	{name: "update_relationship", method: "PUT"},
	{name: "destroy_relationship", method: "DELETE"},
}

// collectJsonapiDeclarations records every jsonapi declaration in a route file
// with the module path it sits under, so a relationship resolved later can name
// its controller by the route name the application actually wrote.
func collectJsonapiDeclarations(src []byte, resolver *jsonapiResolver) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	var walk func(n *sitter.Node, mod string)
	walk = func(n *sitter.Node, mod string) {
		for i := uint(0); i < n.ChildCount(); i++ {
			c := n.Child(i)
			if kindOf(c) != "call" {
				walk(c, mod)
				continue
			}
			method := rubyText(c.ChildByFieldName("method"), src)
			args := c.ChildByFieldName("arguments")
			inner := mod
			switch method {
			case "namespace":
				inner = joinModule(mod, firstSymbolArg(args, src))
			case "scope":
				if m := pairSymbol(args, "module", src); m != "" {
					inner = joinModule(mod, m)
				} else if m := pairString(args, "module", src); m != "" {
					inner = joinModule(mod, m)
				}
			case "jsonapi_resources":
				// Only the plural form names a type. A singular `jsonapi_resource
				// :company` is still served by the *plural* controller, so letting it
				// register "company" would clobber the "companies" the plural
				// declaration recorded and rename nine handlers.
				if name := firstSymbolArg(args, src); name != "" {
					resolver.declare(mod, name)
				}
			}
			walk(c, inner)
		}
	}
	walk(tree.RootNode(), "")
}

package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// javaAnnotation is a parsed annotation with its simple name and (string-valued)
// arguments. Non-string argument values (e.g. RequestMethod.GET) are kept as their
// trailing identifier.
type javaAnnotation struct {
	name       string            // simple name, e.g. "RequestMapping"
	positional []string          // positional argument values, e.g. @GetMapping("/x")
	named      map[string]string // key=value arguments, e.g. value="/x", method="GET"
}

// parseAnnotations extracts the annotations from a `modifiers` node.
func parseAnnotations(modifiers *sitter.Node, src []byte) []javaAnnotation {
	if modifiers == nil {
		return nil
	}
	var out []javaAnnotation
	for i := uint(0); i < uint(modifiers.ChildCount()); i++ {
		c := modifiers.Child(i)
		switch kindOf(c) {
		case "marker_annotation":
			if n := annotationSimpleName(c, src); n != "" {
				out = append(out, javaAnnotation{name: n})
			}
		case "annotation":
			ann := javaAnnotation{name: annotationSimpleName(c, src), named: map[string]string{}}
			if args := findChildByKind(c, "annotation_argument_list"); args != nil {
				parseAnnotationArgs(args, src, &ann)
			}
			if ann.name != "" {
				out = append(out, ann)
			}
		}
	}
	return out
}

func annotationSimpleName(node *sitter.Node, src []byte) string {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return lastTypeComponent(nodeText(nameNode, src))
}

func parseAnnotationArgs(args *sitter.Node, src []byte, ann *javaAnnotation) {
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		c := args.Child(i)
		switch kindOf(c) {
		case "element_value_pair":
			key := c.ChildByFieldName("key")
			val := c.ChildByFieldName("value")
			if key != nil && val != nil {
				ann.named[nodeText(key, src)] = annotationValue(val, src)
			}
		case "string_literal", "field_access", "identifier", "element_value_array_initializer", "scoped_identifier":
			ann.positional = append(ann.positional, annotationValue(c, src))
		}
	}
}

// annotationValue renders an annotation argument value as a string: string literals
// are unquoted; everything else keeps its trailing identifier (so RequestMethod.GET
// becomes "GET").
func annotationValue(node *sitter.Node, src []byte) string {
	switch kindOf(node) {
	case "string_literal":
		return strings.Trim(nodeText(node, src), `"`)
	case "field_access", "scoped_identifier":
		return lastTypeComponent(nodeText(node, src))
	case "element_value_array_initializer", "array_initializer":
		// Join the trailing identifiers of each element, e.g. {GET, POST}.
		var parts []string
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			c := node.Child(i)
			if c.IsNamed() {
				parts = append(parts, annotationValue(c, src))
			}
		}
		return strings.Join(parts, ",")
	default:
		return strings.TrimSpace(nodeText(node, src))
	}
}

func hasAnnotation(anns []javaAnnotation, names ...string) bool {
	for _, a := range anns {
		for _, n := range names {
			if a.name == n {
				return true
			}
		}
	}
	return false
}

func findAnnotation(anns []javaAnnotation, name string) *javaAnnotation {
	for i := range anns {
		if anns[i].name == name {
			return &anns[i]
		}
	}
	return nil
}

// --- component classification ---

// classifyComponent tags a type symbol fact with framework/component props for
// Spring stereotypes and Dubbo SPI. It mutates f.Props in place.
func classifyComponent(f *facts.Fact, name string, annotations []javaAnnotation, supertypes []string) {
	// Dagger/Hilt DI infrastructure. Dagger @Component/@Subcomponent are declared
	// on INTERFACES, whereas Spring stereotypes are always concrete classes — so a
	// @Component on an interface is Dagger, not Spring. This disambiguates the
	// simple-name collision between dagger.Component and springframework…Component
	// (annotations are matched by simple name) and keeps DI wiring out of the
	// domain-architecture metrics. @Module classes are DI wiring regardless of kind.
	if hasAnnotation(annotations, "Module") {
		f.SetProp("di_module", true)
	}
	if f.PropAny("symbol_kind") == facts.SymbolInterface &&
		(hasAnnotation(annotations, "Component") || hasAnnotation(annotations, "Subcomponent")) {
		f.SetProp("di_component", true)
		return // do NOT fall through to the Spring stereotype switch (avoids mislabel)
	}

	switch {
	case hasAnnotation(annotations, "RestController"):
		f.SetProp("framework", "spring")
		f.SetProp("component", "controller")
	case hasAnnotation(annotations, "Controller"):
		f.SetProp("framework", "spring")
		f.SetProp("component", "controller")
	case hasAnnotation(annotations, "Service"):
		f.SetProp("framework", "spring")
		f.SetProp("component", "service")
	case hasAnnotation(annotations, "Repository"):
		f.SetProp("framework", "spring")
		f.SetProp("component", "repository")
	case hasAnnotation(annotations, "Configuration"):
		f.SetProp("framework", "spring")
		f.SetProp("component", "configuration")
	case hasAnnotation(annotations, "Component"):
		f.SetProp("framework", "spring")
		f.SetProp("component", "component")
	}

	// Dubbo SPI extension mechanism.
	if hasAnnotation(annotations, "SPI") {
		f.SetProp("framework", "dubbo")
		f.SetProp("dubbo_spi", true)
	}
	if hasAnnotation(annotations, "Activate") {
		f.SetProp("dubbo_activate", true)
		if f.PropAny("framework") == nil {
			f.SetProp("framework", "dubbo")
		}
	}
	if hasAnnotation(annotations, "DubboService") {
		f.SetProp("framework", "dubbo")
		f.SetProp("component", "service")
	}

	// Runtime classpath-scanned plugin annotations: the class is discovered and
	// instantiated by the framework scanning for the annotation, never referenced by
	// its own name in code — an entry point, not dead code. Currently ThingsBoard's
	// @RuleNode (rule-engine nodes); the `scanned_plugin` prop is generic so other
	// scanned-plugin annotations can join it.
	if hasAnnotation(annotations, "RuleNode") {
		f.SetProp("scanned_plugin", true)
	}

	// Spring Data repository interface (extends JpaRepository/CrudRepository/...).
	if isSpringDataRepository(supertypes) {
		f.SetProp("framework", "spring")
		f.SetProp("component", "repository")
	}
}

func isSpringDataRepository(supertypes []string) bool {
	for _, s := range supertypes {
		switch s {
		case "JpaRepository", "CrudRepository", "PagingAndSortingRepository",
			"ReactiveCrudRepository", "MongoRepository", "Repository":
			return true
		}
	}
	return false
}

// --- JPA / storage ---

// detectJpaStorage emits a KindStorage fact for JPA entities, Spring @Repository
// classes, and Spring Data repository interfaces.
func detectJpaStorage(name string, annotations []javaAnnotation, relFile string, line int, dir string) *facts.Fact {
	var storageKind, framework string
	switch {
	case hasAnnotation(annotations, "Entity"):
		storageKind, framework = "entity", "jpa"
	case hasAnnotation(annotations, "Repository"):
		storageKind, framework = "repository", "spring-data"
	default:
		return nil
	}

	f := &facts.Fact{
		Kind: facts.KindStorage,
		Name: dir + "." + name,
		File: relFile,
		Line: line,
		Props: map[string]any{
			"storage_kind": storageKind,
			"language":     "java",
			"framework":    framework,
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: dir},
		},
	}
	// @Table(name="...") or @Entity(name="...") → table name.
	if t := findAnnotation(annotations, "Table"); t != nil {
		if tn := t.named["name"]; tn != "" {
			f.SetProp("table", tn)
		} else if len(t.positional) > 0 {
			f.SetProp("table", t.positional[0])
		}
	}
	return f
}

// --- Spring MVC routes ---

func isSpringController(annotations []javaAnnotation) bool {
	return hasAnnotation(annotations, "RestController", "Controller")
}

// requestMappingPath returns the base path declared by a class-level @RequestMapping
// (its value/path argument), or "" when absent.
func requestMappingPath(annotations []javaAnnotation) string {
	rm := findAnnotation(annotations, "RequestMapping")
	if rm == nil {
		return ""
	}
	return mappingPath(rm)
}

// mappingMethods maps a method-level mapping annotation to its HTTP verb(s).
var mappingMethods = map[string]string{
	"GetMapping":    "GET",
	"PostMapping":   "POST",
	"PutMapping":    "PUT",
	"DeleteMapping": "DELETE",
	"PatchMapping":  "PATCH",
}

// springRouteFacts emits a KindRoute fact for each HTTP method a controller method
// handles, combining the class base path with the method-level mapping path.
func springRouteFacts(basePath string, methodAnns []javaAnnotation, relFile string, line int, dir, handler string) []facts.Fact {
	var out []facts.Fact
	for _, a := range methodAnns {
		var methods []string
		var sub string
		if verb, ok := mappingMethods[a.name]; ok {
			methods = []string{verb}
			sub = mappingPath(&a)
		} else if a.name == "RequestMapping" {
			methods = requestMappingVerbs(&a)
			sub = mappingPath(&a)
		} else {
			continue
		}
		full := facts.JoinRoutePath(basePath, sub)
		for _, m := range methods {
			out = append(out, facts.Fact{
				Kind: facts.KindRoute,
				Name: full,
				File: relFile,
				Line: line,
				Props: map[string]any{
					"method":    m,
					"framework": "spring",
					"language":  "java",
					"handler":   handler,
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
				},
			})
		}
	}
	return out
}

// mappingPath extracts the path from a mapping annotation: a positional value,
// value=, or path= argument.
func mappingPath(a *javaAnnotation) string {
	if len(a.positional) > 0 {
		return firstPath(a.positional[0])
	}
	if v := a.named["value"]; v != "" {
		return firstPath(v)
	}
	if v := a.named["path"]; v != "" {
		return firstPath(v)
	}
	return ""
}

// firstPath returns the first entry of a possibly comma-joined path list.
func firstPath(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		return s[:i]
	}
	return s
}

// requestMappingVerbs returns the HTTP methods named by a @RequestMapping's
// method= argument, defaulting to "ALL" when unspecified.
func requestMappingVerbs(a *javaAnnotation) []string {
	m := a.named["method"]
	if m == "" {
		return []string{"ALL"}
	}
	var out []string
	for _, part := range strings.Split(m, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return []string{"ALL"}
	}
	return out
}

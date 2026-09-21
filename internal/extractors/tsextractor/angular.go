// Angular support — the fourth framework dialect the TypeScript extractor handles,
// beside Vue, Svelte and Ember.
//
// Angular's architecture is declared almost entirely in decorators and in the
// dependency-injection graph, and neither was visible: measured across ten public
// Angular repositories, 15,648 classes carry @Component, 3,314 carry @Injectable and
// 3,425 carry @NgModule, while 18,194 constructor parameters and 7,224 inject() calls
// name the collaborator they receive. Every one of them extracted as an ordinary
// class with no framework role and no outgoing edge.
//
// Two rules shape what this file does and does not do.
//
// Everything is gated on the repository actually being an Angular one. A class
// decorated @Component in a repo with no @angular/core dependency models nothing —
// the same gate detectORMs applies to a class decorated @Entity, and for the same
// reason: a decorator name is not a framework.
//
// An injects edge is DERIVED, never guessed. The target is resolved through the
// file's own import table (the local name a named import binds, mapped to the module
// that declares it) or through a class the same file declares. A parameter whose type
// comes from a default import, a namespace import, an external package or an
// injection token resolves to nothing and is COUNTED instead — a missing edge beats a
// wrong one, and 21,418 injection sites is far too many to guess at.
package tsextractor

import (
	"path"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// AngularFramework is the framework prop value stamped on Angular facts.
const AngularFramework = "angular"

// angularRoles maps a class-level decorator to the role the container gives the
// class. Every one of them is instantiated by Angular rather than by application
// code, which is what framework_registered records.
var angularRoles = map[string]string{
	"Component":  "component",
	"Directive":  "directive",
	"Pipe":       "pipe",
	"Injectable": "service",
	"NgModule":   "ng_module",
}

// angularSearchDepth is how deep a nested Angular application may sit. One level
// covers `frontend/` and `apps/<app>/`; two covers an Nx workspace whose only
// @angular/core declaration is in a project manifest. Deeper than that and the
// directory is a fixture or a vendored copy, not the repository's frontend.
const angularSearchDepth = 2

// detectAngular reports whether this repository contains an Angular application.
//
// It searches nested manifests as well as the TypeScript root, which is the shape
// that matters most here: two of the corpus's Angular applications live inside a
// repository whose root package.json is a backend's (a Rails monolith's `frontend/`
// and an Nx workspace's `apps/<app>/`). Checking only the root would leave the flag
// false and every gate below silently producing nothing — the failure detectEmber
// records having made once already.
func detectAngular(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
	if hasPkgDependency(tsRoot, "@angular/core") ||
		(tsRoot != repoPath && hasPkgDependency(repoPath, "@angular/core")) {
		return true
	}
	return nestedPkgDeclares(repoPath, "@angular/core", angularSearchDepth)
}

// angularCounts accounts for the injection sites one file declared: those whose
// target this pass could name, and those it could not, by cause.
type angularCounts struct {
	resolved   int
	unresolved map[string]int
}

func (c *angularCounts) miss(cause string) {
	if c.unresolved == nil {
		c.unresolved = map[string]int{}
	}
	c.unresolved[cause]++
}

func (c *angularCounts) merge(o angularCounts) {
	c.resolved += o.resolved
	for k, v := range o.unresolved {
		if c.unresolved == nil {
			c.unresolved = map[string]int{}
		}
		c.unresolved[k] += v
	}
}

// total reports whether this account saw anything at all. An extractor that
// examined nothing must not report a confident zero.
func (c angularCounts) total() int {
	n := c.resolved
	for _, v := range c.unresolved {
		n += v
	}
	return n
}

// angularEnrich classifies the Angular classes in one file's facts and attaches
// their injection edges. It runs as a post-pass over the declaration facts, the way
// emberEnrich does, because a class's role and its members are decided by the same
// decorator the declaration walk has already moved past.
func angularEnrich(kinds *tsutil.KindTable, in []facts.Fact, root *sitter.Node, ctx *extractCtx, aliases map[string]tsAlias) ([]facts.Fact, angularCounts, map[string]*angularTemplate, *angularHTTPFile) {
	var counts angularCounts
	inline := map[string]*angularTemplate{}
	http := &angularHTTPFile{relFile: ctx.relFile, dir: ctx.dir, constants: map[string]string{}}

	imports, external := buildAngularImports(kinds, root, ctx, aliases)
	byName := make(map[string]int, len(in))
	for i := range in {
		if in[i].Kind == facts.KindSymbol {
			byName[in[i].Name] = i
		}
	}
	// Classes the file itself declares, so a collaborator defined beside its
	// consumer resolves without an import to read.
	local := make(map[string]bool)
	for name := range byName {
		local[name[strings.LastIndexByte(name, '.')+1:]] = true
	}

	for _, class := range angularClassNodes(kinds, root) {
		nameNode := findChildByKind(kinds, class, "type_identifier")
		if nameNode == nil {
			continue
		}
		className := nodeText(nameNode, ctx.src)
		idx, ok := byName[ctx.dir+"."+className]
		if !ok {
			continue
		}
		role, args, ok := angularRole(kinds, class, ctx.src)
		if !ok {
			continue
		}
		f := &in[idx]
		f.SetProp(facts.PropFramework, AngularFramework)
		f.SetProp("web_component", role)
		// framework_registered marks a class whose use is NOT derivable from the
		// graph, and after the template, composition and injection passes that is
		// only the modules: a root module is named by a bootstrap call and a lazy one
		// by an import this pass may not resolve, while a component is named by a
		// template tag, a route or a declarations array, a pipe by its name in an
		// expression, and a service by an injection site — all of which are edges
		// now. Flagging those too would hide the dead code the edges make findable:
		// on one application the unreferenced count went from 10 to 0 across
		// components, directives, pipes and services, and every one of those zeros
		// is a reading the flag would have suppressed either way.
		if role == "ng_module" {
			f.SetProp("framework_registered", true)
		}
		if tpl := angularDecoratorProps(kinds, args, ctx, role, f.Props); tpl != nil {
			inline[f.Name] = tpl
		}

		// What the class's own decorator composes: an NgModule's declarations,
		// imports, exports and providers, and a standalone component's imports. This
		// is the application's composition, and none of it is visible in the file's
		// import statements alone — those say which files were loaded, not which
		// declarations were assembled.
		if role == "ng_module" || role == "component" || role == "directive" {
			modRels, modProps, c := angularModuleEdges(kinds, args, ctx, imports, local)
			counts.merge(c)
			for k, v := range modProps {
				f.SetProp(k, v)
			}
			for _, r := range modRels {
				if !f.HasRelation(r.Kind, r.Target) {
					f.Relations = append(f.Relations, r)
				}
			}
		}

		body := findChildByKind(kinds, class, "class_body")

		// Requests made through an injected HttpClient, and the constants a request
		// path may name. Held for the repo-wide pass: a base URL is routinely a
		// static of the service that owns the resource, named by every other service
		// that touches it. Test files are excluded for the reason every other route
		// pass excludes them — a spec's traffic is not the application's.
		if !facts.IsTestPath(ctx.relFile) {
			angularHTTPRoutes(kinds, body, ctx, className, http)
		}

		rels, c := angularInjects(kinds, body, ctx, imports, external, local)
		counts.merge(c)
		for _, r := range rels {
			if !f.HasRelation(r.Kind, r.Target) {
				f.Relations = append(f.Relations, r)
			}
		}
	}
	if len(http.constants) == 0 && len(http.pending) == 0 {
		http = nil
	}
	return in, counts, inline, http
}

// angularClassNodes returns every class declaration at file scope, reaching
// through an export statement — `@Component(…) export class Foo` attaches the
// decorator to the export, not to the class.
func angularClassNodes(kinds *tsutil.KindTable, root *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for i := range root.ChildCount() {
		child := root.Child(i)
		switch kindOf(kinds, child) {
		case "class_declaration", "abstract_class_declaration", "class":
			out = append(out, child)
		case "export_statement":
			if decl := firstDeclChild(kinds, child); decl != nil {
				switch kindOf(kinds, decl) {
				case "class_declaration", "abstract_class_declaration", "class":
					out = append(out, decl)
				}
			}
		}
	}
	return out
}

// angularRole returns the container role a class's decorators give it, with the
// decorator's arguments. A class carrying none of them is not Angular's.
func angularRole(kinds *tsutil.KindTable, class *sitter.Node, src []byte) (string, *sitter.Node, bool) {
	// Sorted so a class carrying two of these (which Angular rejects, but a
	// half-migrated file can still contain) classifies identically on every run.
	names := make([]string, 0, len(angularRoles))
	for name := range angularRoles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if args, ok := classDecoratorArgs(kinds, class, src, name); ok {
			return angularRoles[name], args, true
		}
	}
	return "", nil, false
}

// angularDecoratorProps records what the decorator's own object literal states —
// the selector a template will be resolved against, the pipe's name, whether the
// class is standalone, and where its template lives.
//
// `standalone` is recorded only when the source states it. Angular's default
// flipped with v19, so absence means "this repository's Angular version decides",
// and writing either value would be a guess about a version this pass does not read.
// angularDecoratorProps records what the decorator's own object literal states, and
// returns the scan of an inline template if it carries one.
func angularDecoratorProps(kinds *tsutil.KindTable, args *sitter.Node, ctx *extractCtx, role string, props map[string]any) *angularTemplate {
	obj := firstObjectArg(kinds, args)
	if obj == nil {
		return nil
	}
	switch role {
	case "component", "directive":
		if sel := objectStringProp(kinds, obj, ctx.src, "selector"); sel != "" {
			props["angular_selector"] = sel
		}
	case "pipe":
		if name := objectStringProp(kinds, obj, ctx.src, "name"); name != "" {
			props["angular_pipe_name"] = name
		}
	}
	if v, ok := objectBoolProp(kinds, obj, ctx.src, "standalone"); ok {
		props["angular_standalone"] = v
	}
	if url := objectStringProp(kinds, obj, ctx.src, "templateUrl"); url != "" {
		// Stored repo-relative, resolved against the component's own directory,
		// because that is the only form the template pass can look a file up by.
		props["angular_template_url"] = path.Clean(factpath.Join(ctx.dir, url))
	}
	if v := objectPropValue(kinds, obj, ctx.src, "template"); v != nil {
		props["angular_inline_template"] = true
		// An inline template is markup like any other; only its delimiters differ.
		body := strings.Trim(nodeText(v, ctx.src), "`\"'")
		return scanAngularTemplate([]byte(body), ctx.relFile)
	}
	return nil
}

// angularInjects returns the injects relations a class body declares, by both
// dialects: constructor parameters and `inject()` field initializers. Angular
// applications use both, frequently in the same class, and a pass that read only
// one would miss between a quarter and three quarters of the sites depending on the
// repository's vintage.
func angularInjects(kinds *tsutil.KindTable, body *sitter.Node, ctx *extractCtx, imports map[string]string, external, local map[string]bool) ([]facts.Relation, angularCounts) {
	var rels []facts.Relation
	var counts angularCounts
	if body == nil {
		return nil, counts
	}
	add := func(typeName string) {
		if typeName == "" {
			return
		}
		target, ok := angularResolveType(typeName, ctx, imports, local)
		if !ok {
			counts.miss(angularMissCause(typeName, external))
			return
		}
		counts.resolved++
		rels = append(rels, facts.Relation{Kind: facts.RelInjects, Target: target})
	}

	for i := range body.ChildCount() {
		member := body.Child(i)
		switch kindOf(kinds, member) {
		case "method_definition":
			if methodDecoratorName(kinds, member, ctx.src) != "constructor" {
				continue
			}
			for _, t := range constructorParamTypes(kinds, member, ctx.src) {
				add(t)
			}
		case "public_field_definition":
			for _, t := range injectCallTypes(kinds, member, ctx.src) {
				add(t)
			}
		}
	}
	return rels, counts
}

// constructorParamTypes returns what each constructor parameter injects. Both the
// shorthand property form (`private users: UserService`) and a plain parameter are
// read.
//
// An `@Inject(TOKEN)` parameter names its token rather than its type, and the token
// is what the container resolves — the annotation beside it is routinely `string` or
// `unknown`, which names nothing. So the decorator wins where it is present: the
// token is a declaration like any other, and an edge to it says which token this
// class requires. A parameter with neither a token nor a plain type name yields
// nothing.
func constructorParamTypes(kinds *tsutil.KindTable, method *sitter.Node, src []byte) []string {
	params := method.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	var out []string
	for i := range params.ChildCount() {
		p := params.Child(i)
		switch kindOf(kinds, p) {
		case "required_parameter", "optional_parameter":
		default:
			continue
		}
		if token := paramInjectToken(kinds, p, src); token != "" {
			out = append(out, token)
			continue
		}
		if ann := p.ChildByFieldName("type"); ann != nil {
			out = append(out, plainTypeName(kinds, ann, src))
		}
	}
	return out
}

// paramInjectToken returns the identifier an `@Inject(TOKEN)` parameter decorator
// names, or "" when the parameter carries no such decorator.
func paramInjectToken(kinds *tsutil.KindTable, param *sitter.Node, src []byte) string {
	for i := range param.ChildCount() {
		dec := param.Child(i)
		if kindOf(kinds, dec) != "decorator" {
			continue
		}
		name, args := decoratorNameArgs(kinds, dec, src)
		if name != "Inject" || args == nil {
			continue
		}
		for j := range args.ChildCount() {
			if a := args.Child(j); kindOf(kinds, a) == "identifier" {
				return nodeText(a, src)
			}
		}
	}
	return ""
}

// injectCallTypes returns the types named by `inject(Foo)` in a field initializer.
// The generic form `inject<T>(TOKEN)` names its token positionally like the plain
// one, so only the first argument is read.
func injectCallTypes(kinds *tsutil.KindTable, member *sitter.Node, src []byte) []string {
	value := member.ChildByFieldName("value")
	if value == nil {
		return nil
	}
	var out []string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(kinds, n) == "call_expression" {
			if fn := n.ChildByFieldName("function"); fn != nil && nodeText(fn, src) == "inject" {
				if args := n.ChildByFieldName("arguments"); args != nil {
					for i := range args.ChildCount() {
						a := args.Child(i)
						if kindOf(kinds, a) == "identifier" {
							out = append(out, nodeText(a, src))
							break
						}
					}
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i))
		}
	}
	walk(value)
	return out
}

// plainTypeName reduces a type annotation to a bare type name, or "" for anything
// else. `UserService` yields itself; `Observable<User>`, `string`, a union and an
// inline object type all yield nothing — a generic wrapper names the collaborator
// nowhere the container would look for it.
func plainTypeName(kinds *tsutil.KindTable, ann *sitter.Node, src []byte) string {
	for i := range ann.ChildCount() {
		c := ann.Child(i)
		if kindOf(kinds, c) == "type_identifier" {
			return nodeText(c, src)
		}
	}
	return ""
}

// angularResolveType names the symbol a type identifier refers to, through the
// file's own imports or its own declarations. Anything else is unresolved by
// design; see the package comment.
func angularResolveType(name string, ctx *extractCtx, imports map[string]string, local map[string]bool) (string, bool) {
	if target, ok := imports[name]; ok {
		return target, true
	}
	if local[name] {
		return ctx.dir + "." + name, true
	}
	return "", false
}

// buildAngularImports maps each named import to the symbol it names, resolving the
// import path to a FILE before naming its module.
//
// It does not reuse buildImportSymbols, which takes the directory of the resolved
// path without first establishing that the path IS a file. For `from './services'`
// — a barrel index, which Angular and Nx codebases use everywhere — that yields the
// barrel's PARENT directory, and an edge to `<parent>.UserService` names a node that
// does not exist. Measured before this existed: 76% of one repository's injection
// edges pointed at no node at all. resolveModuleFile already distinguishes the two
// cases and returns the right directory for each.
func buildAngularImports(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx, aliases map[string]tsAlias) (map[string]string, map[string]bool) {
	out := make(map[string]string)
	external := make(map[string]bool)
	for i := range root.ChildCount() {
		child := root.Child(i)
		if kindOf(kinds, child) != "import_statement" {
			continue
		}
		source := findChildByKind(kinds, child, "string")
		if source == nil {
			continue
		}
		resolved, isExternal := resolveImportPath(strings.Trim(nodeText(source, ctx.src), `"'`), ctx.dir, aliases)
		moduleDir := ""
		if !isExternal {
			var ok bool
			if _, moduleDir, ok = resolveModuleFile(resolved, ctx.knownFiles); !ok {
				continue
			}
		}
		clause := findChildByKind(kinds, child, "import_clause")
		if clause == nil {
			continue
		}
		named := findChildByKind(kinds, clause, "named_imports")
		if named == nil {
			continue // a default or namespace import names no export
		}
		for j := range named.ChildCount() {
			spec := named.Child(j)
			if kindOf(kinds, spec) != "import_specifier" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			if nameNode == nil {
				continue
			}
			exportName := nodeText(nameNode, ctx.src)
			localName := exportName
			if alias := spec.ChildByFieldName("alias"); alias != nil {
				localName = nodeText(alias, ctx.src)
			}
			if isExternal {
				// Recorded, not resolved: a type from a package this repository does
				// not contain has no symbol to point at, and saying so by name is the
				// difference between "enola could not read this" and "there is nothing
				// here to read".
				external[localName] = true
				continue
			}
			out[localName] = moduleDir + "." + exportName
		}
	}
	return out, external
}

// reconcileAngularInjects makes every injects edge point at a symbol the snapshot
// actually holds, once the whole repository is visible.
//
// Per-file resolution cannot see through a barrel that re-exports from somewhere
// else: `export { UserService } from '../data/user.service'` leaves the edge naming
// the barrel's directory, and the declaration lives one directory over. Here the
// full symbol table is in hand, so an unmatched target that has exactly ONE
// same-named symbol in the repository is repointed at it — one candidate required,
// the rule the Ember resolver settled on — and anything still unmatched is REMOVED
// and counted. A dangling edge is worse than a missing one: it is invisible in
// coverage and it is followed by impact analysis exactly as a real edge would be.
func reconcileAngularInjects(all []facts.Fact) angularCounts {
	return reconcileAngularEdges(all, facts.RelInjects, facts.RelDependsOn)
}

// resolveAngularLazyComponents binds a route whose component is a file's default
// export to the class that file declares. One candidate required: a file declaring
// two Angular classes names neither unambiguously.
func resolveAngularLazyComponents(all []facts.Fact) angularCounts {
	var counts angularCounts
	byFile := map[string][]string{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.PropString(facts.PropFramework) != AngularFramework {
			continue
		}
		if f.PropString("web_component") == "component" {
			byFile[f.File] = append(byFile[f.File], f.Name)
		}
	}
	for i := range all {
		f := &all[i]
		file := f.PropString("angular_lazy_component_file")
		if f.Kind != facts.KindRoute || file == "" {
			continue
		}
		f.DelProp("angular_lazy_component_file")
		cand := byFile[file]
		if len(cand) != 1 {
			counts.miss("ambiguous_lazy_component")
			continue
		}
		counts.resolved++
		f.SetProp("handler", cand[0])
		if !f.HasRelation(facts.RelHandledBy, cand[0]) {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: cand[0]})
		}
	}
	return counts
}

// reconcileAngularEdges is the general form: it holds for every edge this dialect
// resolves through an import table, which is the injects edges and the NgModule
// composition edges alike.
func reconcileAngularEdges(all []facts.Fact, kinds ...string) angularCounts {
	wanted := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		wanted[k] = true
	}
	var counts angularCounts
	names := make(map[string]bool)
	byShort := make(map[string][]string)
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		names[f.Name] = true
		short := f.Name[strings.LastIndexByte(f.Name, '.')+1:]
		byShort[short] = append(byShort[short], f.Name)
	}

	for i := range all {
		if all[i].PropString(facts.PropFramework) != AngularFramework {
			// depends_on is written by other passes too, and this reconciliation is
			// only sound for edges resolved through an Angular import table.
			continue
		}
		rels := all[i].Relations
		kept := rels[:0]
		for _, r := range rels {
			if !wanted[r.Kind] || names[r.Target] {
				kept = append(kept, r)
				continue
			}
			short := r.Target[strings.LastIndexByte(r.Target, '.')+1:]
			if cand := byShort[short]; len(cand) == 1 {
				r.Target = cand[0]
				kept = append(kept, r)
				continue
			}
			// Resolved at emission, unresolved in fact: move it across so the
			// coverage number reports the edge that did not survive.
			counts.resolved--
			if len(byShort[short]) > 1 {
				counts.miss("ambiguous_target")
			} else {
				counts.miss("undeclared_target")
			}
		}
		all[i].Relations = kept
	}
	return counts
}

// angularMissCause names why a type did not resolve, so the coverage number is a
// task rather than a total.
//
// Both named causes are DERIVED rather than guessed from the identifier: a type the
// file imported from a package is external because the import statement said so,
// and an UPPER_SNAKE name with no import behind it is an injection token, which no
// import table could turn into a class. What is left is a real miss. An earlier
// version guessed "framework type" from name prefixes and buried most of one
// repository's genuine residual underneath it.
func angularMissCause(name string, external map[string]bool) string {
	if external[name] {
		return "external_package"
	}
	if name == strings.ToUpper(name) {
		return "injection_token"
	}
	return "unresolved_type"
}

// firstObjectArg returns the first object literal in a decorator's arguments.
func firstObjectArg(kinds *tsutil.KindTable, args *sitter.Node) *sitter.Node {
	if args == nil {
		return nil
	}
	for i := range args.ChildCount() {
		if arg := args.Child(i); kindOf(kinds, arg) == "object" {
			return arg
		}
	}
	return nil
}

// objectBoolProp returns the boolean value of a named property, and whether it was
// stated at all.
func objectBoolProp(kinds *tsutil.KindTable, obj *sitter.Node, src []byte, key string) (bool, bool) {
	v := objectPropValue(kinds, obj, src, key)
	if v == nil {
		return false, false
	}
	switch nodeText(v, src) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// objectPropValue returns the value node of a named property of an object literal.
func objectPropValue(kinds *tsutil.KindTable, obj *sitter.Node, src []byte, key string) *sitter.Node {
	for i := range obj.ChildCount() {
		pair := obj.Child(i)
		if kindOf(kinds, pair) != "pair" {
			continue
		}
		k := pair.ChildByFieldName("key")
		if k == nil || strings.Trim(nodeText(k, src), `"'`) != key {
			continue
		}
		return pair.ChildByFieldName("value")
	}
	return nil
}

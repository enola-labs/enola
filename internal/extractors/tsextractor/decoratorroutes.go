// Server-side routes declared with class decorators: NestJS (@Controller + @Get/…)
// and InversifyJS (@controller + @httpGet/…).
//
// Until this pass, tsextractor emitted server routes ONLY for file-based routers
// (Next.js, Nuxt, SvelteKit) — every `role` it wrote was "client". A decorator-routed
// TypeScript backend therefore contributed zero server routes, so in a cross-repo
// snapshot every client call against it fell into the unresolved residual and the
// backend itself was classified `isolated`, i.e. a leaf. The shape is the same one
// javaextractor already models for Spring (`springRouteFacts`): a class-level base
// path, a method-level verb + sub-path, composed.
package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// controllerDecorators maps a class-level decorator name to the framework it
// identifies. Emitting is GATED on one of these being present: it is what keeps a
// generic method decorator named @Get (or Inversify's very generic @httpGet) from
// minting routes inside an ordinary class.
var controllerDecorators = map[string]string{
	"Controller": "nestjs",    // @Controller("/users") | @Controller({path: "/users"})
	"controller": "inversify", // @controller("/users")
}

// verbDecorators maps a method-level decorator name to its HTTP verb, per framework.
// NestJS capitalises (@Get); InversifyJS prefixes (@httpGet). Kept separate so a
// class cannot mix the two vocabularies and produce a route its framework never
// serves.
var verbDecorators = map[string]map[string]string{
	"nestjs": {
		"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH",
		"Delete": "DELETE", "Head": "HEAD", "Options": "OPTIONS", "All": "ALL",
	},
	"inversify": {
		"httpGet": "GET", "httpPost": "POST", "httpPut": "PUT", "httpPatch": "PATCH",
		"httpDelete": "DELETE", "httpHead": "HEAD", "httpAll": "ALL",
	},
}

// decoratorRouteFacts emits one server-role KindRoute per verb decorator on a
// controller class, at the path formed by the class base and the method sub-path.
// Returns nil for any class without a controller decorator.
//
// className is the name the class's own symbol fact carries after dir, so each route
// can name its handler method's symbol exactly: the decorated method is emitted as
// dir.className.method by the class walk in ts.go, with no condition a route-bearing
// method can fail.
//
// Two things it deliberately does NOT compose into the path:
//
//   - a `version:` property on the @Controller object. NestJS versioning can be
//     URI-based, but it can equally be header- or media-type-based (VersioningType
//     .CUSTOM/HEADER), and the decorator alone does not say which. Guessing would
//     write a path the server never serves.
//   - the application's global prefix (app.setGlobalPrefix(...)), which is routinely
//     read from the environment and so is not knowable statically. The cross-repo
//     linker matches on >=2-segment path SUFFIXES, so "/v2/slots/available" still
//     resolves a client's "/api/v2/slots/available" without it.
func decoratorRouteFacts(kinds *tsutil.KindTable, classNode, classBody *sitter.Node, src []byte, relFile, dir, className string) []facts.Fact {
	base, framework, ok := controllerBase(kinds, classNode, src)
	if !ok || classBody == nil {
		return nil
	}
	verbs := verbDecorators[framework]

	var out []facts.Fact
	// Member decorators are children of class_body in their own right, appearing as
	// siblings immediately BEFORE the method they annotate (they are not children of
	// method_definition). So accumulate them while walking in order and flush on each
	// method — the same ordered walk ts.go already makes over this node.
	var pending []*sitter.Node
	for i := range classBody.ChildCount() {
		member := classBody.Child(i)
		switch {
		case kindOf(kinds, member) == "decorator":
			pending = append(pending, member)
		case kindOf(kinds, member) == "comment" || !member.IsNamed():
			// Transparent. A JSDoc block routinely sits BETWEEN a method's decorators
			// and the method itself, and the class braces and stray semicolons are
			// unnamed children of class_body — none of them ends a decorator run.
			// Treating a comment as a member silently dropped real routes: two on the
			// one real NestJS API measured against, both a decorated handler
			// documented just above its signature.
		case kindOf(kinds, member) == "method_definition":
			out = append(out, methodRouteFacts(kinds, pending, member, src, base, framework, verbs, relFile, dir, className)...)
			pending = pending[:0]
		default:
			// A real member (a field, an index signature) ends the run: its decorators
			// were its own, and must not carry over to the next method.
			pending = pending[:0]
		}
	}
	return out
}

// methodRouteFacts emits the routes declared by one method's decorators.
func methodRouteFacts(kinds *tsutil.KindTable, decorators []*sitter.Node, method *sitter.Node, src []byte,
	base, framework string, verbs map[string]string, relFile, dir, className string) []facts.Fact {

	handler := methodDecoratorName(kinds, method, src)
	if handler == "" {
		return nil
	}
	var out []facts.Fact
	for _, dec := range decorators {
		name, args := decoratorNameArgs(kinds, dec, src)
		verb, isVerb := verbs[name]
		if !isVerb {
			continue
		}
		full := facts.JoinRoutePath(base, decoratorPathArg(kinds, args, src))
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: full,
			File: relFile,
			Line: int(method.StartPosition().Row) + 1,
			Props: map[string]any{
				// Stated explicitly rather than left to the linker's unset=>server
				// default, so a reader of the fact does not have to know that rule.
				facts.PropRole: facts.RoleServer,
				"method":       verb,
				"framework":    framework,
				"language":     "typescript",
				"handler":      handler,
			},
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: dir},
				{Kind: facts.RelHandledBy, Target: dir + "." + className + "." + handler},
			},
		})
	}
	return out
}

// controllerBase returns the class-level base path and the framework it was declared
// with. It accepts both argument forms — @Controller("/users") and the object form
// @Controller({path: "/users"}), which is overwhelmingly the more common one in real
// NestJS code.
func controllerBase(kinds *tsutil.KindTable, classNode *sitter.Node, src []byte) (base, framework string, ok bool) {
	for name, fw := range controllerDecorators {
		args, found := classDecoratorArgs(kinds, classNode, src, name)
		if !found {
			continue
		}
		return decoratorPathArg(kinds, args, src), fw, true
	}
	return "", "", false
}

// classDecoratorArgs finds a named decorator on a class, looking at the class node
// and — because `@Controller(…) export class Foo` attaches them there — its enclosing
// export_statement. Mirrors classDecorator (storage.go), which does the same for
// TypeORM's @Entity, but yields the arguments node.
func classDecoratorArgs(kinds *tsutil.KindTable, node *sitter.Node, src []byte, want string) (*sitter.Node, bool) {
	if args, ok := decoratorArgsIn(kinds, node, src, want); ok {
		return args, true
	}
	if parent := node.Parent(); parent != nil && kindOf(kinds, parent) == "export_statement" {
		return decoratorArgsIn(kinds, parent, src, want)
	}
	return nil, false
}

// decoratorNameArgs returns a decorator's callee name and call arguments. A bare
// decorator (@Get) yields a nil args node.
func decoratorNameArgs(kinds *tsutil.KindTable, dec *sitter.Node, src []byte) (name string, args *sitter.Node) {
	for i := range dec.ChildCount() {
		inner := dec.Child(i)
		switch kindOf(kinds, inner) {
		case "identifier":
			return nodeText(inner, src), nil
		case "call_expression":
			fn := inner.ChildByFieldName("function")
			if fn == nil {
				return "", nil
			}
			return nodeText(fn, src), inner.ChildByFieldName("arguments")
		}
	}
	return "", nil
}

// decoratorPathArg reads a route path from a decorator's arguments: either the first
// string literal (@Get("/available"), @Controller("/users")) or the `path` property
// of an options object (@Controller({path: "/users", version: …})). Returns "" when
// the decorator has no arguments (@Get()) or names no path, which composes to the
// class base alone.
func decoratorPathArg(kinds *tsutil.KindTable, args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	if s := firstStringArg(kinds, args, src); s != "" {
		return s
	}
	for i := range args.ChildCount() {
		arg := args.Child(i)
		if kindOf(kinds, arg) != "object" {
			continue
		}
		if p := objectStringProp(kinds, arg, src, "path"); p != "" {
			return p
		}
	}
	return ""
}

// objectStringProp returns the string value of a named property of an object literal.
func objectStringProp(kinds *tsutil.KindTable, obj *sitter.Node, src []byte, key string) string {
	for i := range obj.ChildCount() {
		pair := obj.Child(i)
		if kindOf(kinds, pair) != "pair" {
			continue
		}
		k := pair.ChildByFieldName("key")
		if k == nil || strings.Trim(nodeText(k, src), `"'`) != key {
			continue
		}
		v := pair.ChildByFieldName("value")
		if v == nil || (kindOf(kinds, v) != "string" && kindOf(kinds, v) != "template_string") {
			continue
		}
		return strings.Trim(nodeText(v, src), "\"'`")
	}
	return ""
}

// methodDecoratorName returns a class method's name, or "" for one with no plain
// identifier name (a computed or private member).
func methodDecoratorName(kinds *tsutil.KindTable, method *sitter.Node, src []byte) string {
	n := method.ChildByFieldName("name")
	if n == nil || kindOf(kinds, n) != "property_identifier" {
		return ""
	}
	return nodeText(n, src)
}

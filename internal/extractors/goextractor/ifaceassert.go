package goextractor

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// collectInterfaceAssertions reads a package's compile-time interface assertions:
//
//	var _ Engine = (*xorm.Session)(nil)
//	var _ Store  = &sqlStore{}
//
// and returns, per interface, the types asserted to implement it. Go does not
// declare implementation, and the extractor does not type-check, so an interface
// is otherwise a dead end: a call on it names a method with no body, and nothing
// says where the body is. The assertion is the one place the source states it.
//
// Both names are canonical: `pkgDir.Name` for a type of this module, the import
// path and name for another's.
func collectInterfaceAssertions(files []*ast.File, pkgDir, modulePath string, pkgNames, aliases map[string]string) map[string][]string {
	out := make(map[string][]string)
	for _, f := range files {
		ctx := resolveCtx{pkgDir: pkgDir, imports: buildFileImports(f, modulePath, pkgNames), aliases: aliases}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil || len(vs.Values) != len(vs.Names) {
					continue
				}
				iface := typeExprName(vs.Type)
				if iface == "" {
					continue
				}
				for i, name := range vs.Names {
					if name.Name != "_" {
						continue
					}
					if impl := assertedTypeName(vs.Values[i]); impl != "" {
						key := resolveTypeName(iface, ctx)
						out[key] = append(out[key], resolveTypeName(impl, ctx))
					}
				}
			}
		}
	}
	return out
}

// assertedTypeName names the type an assertion's value is of, for the forms an
// assertion is written in: `(*T)(nil)`, `&T{}`, `T{}`, `new(T)` and `T(nil)`.
func assertedTypeName(v ast.Expr) string {
	switch e := v.(type) {
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			if lit, ok := e.X.(*ast.CompositeLit); ok {
				return typeExprName(lit.Type)
			}
		}
	case *ast.CompositeLit:
		return typeExprName(e.Type)
	case *ast.CallExpr:
		if len(e.Args) != 1 {
			return ""
		}
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "new" {
			return typeExprName(e.Args[0])
		}
		// A conversion of nil. Any other single-argument call is a constructor,
		// whose name is not a type.
		if arg, ok := e.Args[0].(*ast.Ident); ok && arg.Name == "nil" {
			return typeExprName(e.Fun)
		}
	}
	return ""
}

// typeExprName returns the name a type expression refers to, as written: `T`,
// `pkg.T`. Pointers, parentheses and type arguments are looked through. Anything
// else (a slice, a map, a func) has no single name and returns "".
func typeExprName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return pkg.Name + "." + t.Sel.Name
		}
	case *ast.StarExpr:
		return typeExprName(t.X)
	case *ast.ParenExpr:
		return typeExprName(t.X)
	case *ast.IndexExpr:
		return typeExprName(t.X)
	case *ast.IndexListExpr:
		return typeExprName(t.X)
	}
	return ""
}

// collectEmbeddedTypes returns, per struct or interface type of a package, the
// types it embeds:
//
//	type DBSession struct { *xorm.Session; events []any }
//	type Engine interface { SQLSession; Ping() error }
//
// An embedded type's methods are promoted, so `sess.Exec(…)` on a DBSession is
// `xorm.Session.Exec`, and call resolution names it `DBSession.Exec`, a method
// that is declared nowhere in the module.
func collectEmbeddedTypes(files []*ast.File, pkgDir, modulePath string, pkgNames, aliases map[string]string) map[string][]string {
	out := make(map[string][]string)
	for _, f := range files {
		ctx := resolveCtx{pkgDir: pkgDir, imports: buildFileImports(f, modulePath, pkgNames), aliases: aliases}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				var fields *ast.FieldList
				switch t := ts.Type.(type) {
				case *ast.StructType:
					fields = t.Fields
				case *ast.InterfaceType:
					fields = t.Methods
				}
				if fields == nil {
					continue
				}
				for _, field := range fields.List {
					if len(field.Names) != 0 {
						continue
					}
					if name := typeExprName(field.Type); name != "" {
						key := pkgDir + "." + ts.Name.Name
						out[key] = append(out[key], resolveTypeName(name, ctx))
					}
				}
			}
		}
	}
	return out
}

// promotedCallProps are the props that repeat a function's call targets, and so
// have to be rewritten with them.
var promotedCallProps = []string{
	"calls_in_loop", "calls_in_scaling_loop", "calls_on_loop_element", "io_calls", "calls_once",
}

// resolvePromotedCalls points a call on a promoted method at the method it runs.
//
// Call resolution names a call by its receiver's type: `sess.Exec(…)` on a
// DBSession is `DBSession.Exec`. When DBSession only embeds the type that declares
// Exec, no symbol has that name, and the edge goes nowhere: the callee has one
// caller fewer than it has, and nothing that follows calls edges gets past it.
// Where a type of this module that the receiver embeds declares the method, the
// target is rewritten to that declaration. The nearest one wins, as it does in
// the language. An interface's embedded interfaces are promoted the same way.
//
// A target that already names a symbol is left alone, and so is one whose method
// no embedded type of this module declares (it is promoted from another module).
func resolvePromotedCalls(all []facts.Fact, embeds map[string][]string) {
	if len(embeds) == 0 {
		return
	}
	exists := make(map[string]bool, len(all))
	for i := range all {
		if all[i].Kind == facts.KindSymbol {
			exists[all[i].Name] = true
		}
	}
	resolved := make(map[string]string) // dangling target → declaration, "" for none
	resolve := func(target string) string {
		if exists[target] {
			return ""
		}
		if to, ok := resolved[target]; ok {
			return to
		}
		to := ""
		if dot := strings.LastIndexByte(target, '.'); dot > 0 {
			owner, member := target[:dot], target[dot:]
			seen := map[string]bool{owner: true}
			for queue := embeds[owner]; len(queue) > 0 && to == ""; {
				t := queue[0]
				queue = queue[1:]
				if seen[t] {
					continue
				}
				seen[t] = true
				if exists[t+member] {
					to = t + member
				}
				queue = append(queue, embeds[t]...)
			}
		}
		resolved[target] = to
		return to
	}

	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		for j := range f.Relations {
			if r := &f.Relations[j]; r.Kind == facts.RelCalls {
				if to := resolve(r.Target); to != "" {
					r.Target = to
				}
			}
		}
		for _, key := range promotedCallProps {
			list, ok := f.PropAny(key).([]string)
			if !ok {
				continue
			}
			for j, c := range list {
				if to := resolve(c); to != "" {
					list[j] = to
				}
			}
		}
	}
}

// promoteReturnTypes gives a type the declared results of the methods it is
// promoted from the types it embeds, nearest first, where it declares no method
// of that name itself.
//
// The returns table is what lets a chained call resolve: `x.Where(…).Find(…)`
// reaches Find through what Where is declared to return. Where the receiver only
// embeds the type that declares Where, the table had no entry under the receiver's
// name and the chain stopped at its first link.
func promoteReturnTypes(returns map[string][]string, embeds map[string][]string) {
	byOwner := make(map[string][]string) // type → its methods that have an entry
	for key := range returns {
		if dot := strings.LastIndexByte(key, '.'); dot > 0 {
			byOwner[key[:dot]] = append(byOwner[key[:dot]], key[dot:])
		}
	}
	type entry struct {
		key   string
		types []string
	}
	var add []entry
	for owner := range embeds {
		taken := make(map[string]bool)
		seen := map[string]bool{owner: true}
		for queue := embeds[owner]; len(queue) > 0; {
			t := queue[0]
			queue = queue[1:]
			if seen[t] {
				continue
			}
			seen[t] = true
			for _, member := range byOwner[t] {
				if _, declared := returns[owner+member]; declared || taken[member] {
					continue
				}
				taken[member] = true
				add = append(add, entry{owner + member, returns[t+member]})
			}
			queue = append(queue, embeds[t]...)
		}
	}
	// Added after the walk, so a promoted entry is never itself promoted from.
	for _, e := range add {
		returns[e.key] = e.types
	}
}

package goextractor

import (
	"go/ast"
	"go/token"
	"strings"
)

// A loop over a registry of handlers.
//
//	type Service struct{ clients []authn.Client }            // set by ProvideService
//	func (s *Service) Authenticate(r *Request) (*Identity, error) {
//	    for _, c := range s.clients {                      // the registered clients
//	        if id, err := c.Authenticate(r); err == nil { return id, nil }
//	    }
//	}
//
// The loop runs once per registered handler: auth clients, providers, rotators,
// readers wired at startup. Its trip count is the wiring, not the request, so a call
// in it is not a call per element of anything that grows. Of the call-in-loop
// findings that appeared once calls on typed receivers resolved, two thirds were
// this shape, read against the source and judged wrong.
//
// A field is a registry when two things hold, and both are needed. Its elements are
// behaviour, an interface or a function type, so a slice of records loaded at
// startup and walked per request (a real O(n)) is not one. And every write to it in
// its package is wiring: a composite literal, or an assignment inside a constructor
// or registration function (New…, Provide…, Register…, init). Only unexported
// fields qualify, since nothing outside the package can write them.

// registrationFunc reports whether a function name is wiring: a constructor, a
// provider for dependency injection, a registration hook, or init.
func registrationFunc(name string) bool {
	if name == "init" {
		return true
	}
	for _, p := range []string{"New", "new", "Provide", "provide", "Register", "register"} {
		if rest, ok := strings.CutPrefix(name, p); ok && (rest == "" || rest[0] >= 'A' && rest[0] <= 'Z') {
			return true
		}
	}
	return false
}

// collectRegistryFields returns "pkgDir.Type.field" for every registry field the
// package declares. ifaces holds the module's interface types by qualified name.
func collectRegistryFields(files []*ast.File, pkgDir string, mods goModules, pkgNames, aliases map[string]string, ifaces map[string]bool) map[string]bool {
	candidates := map[string][]string{} // field name → "pkgDir.Type.field" keys
	for _, f := range files {
		ctx := resolveCtx{pkgDir: pkgDir, imports: buildFileImports(f, mods, pkgNames), aliases: aliases}
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
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				for _, field := range st.Fields.List {
					if !handlerCollection(field.Type, ctx, ifaces) {
						continue
					}
					for _, name := range field.Names {
						if ast.IsExported(name.Name) {
							continue
						}
						candidates[name.Name] = append(candidates[name.Name], pkgDir+"."+ts.Name.Name+"."+name.Name)
					}
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// A write outside wiring disqualifies the field name, whatever type it is on:
	// by name, so a collision only ever loses a registry.
	for _, f := range files {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || registrationFunc(fd.Name.Name) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					if ix, ok := lhs.(*ast.IndexExpr); ok {
						lhs = ix.X // a map field written by key
					}
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						delete(candidates, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	out := map[string]bool{}
	for _, keys := range candidates {
		for _, k := range keys {
			out[k] = true
		}
	}
	return out
}

// handlerCollection reports whether a field type is a slice, array or map of an
// interface or function type.
func handlerCollection(t ast.Expr, ctx resolveCtx, ifaces map[string]bool) bool {
	var elem ast.Expr
	switch c := t.(type) {
	case *ast.ArrayType:
		elem = c.Elt
	case *ast.MapType:
		elem = c.Value
	default:
		return false
	}
	if _, ok := elem.(*ast.FuncType); ok {
		return true
	}
	if q := declaredType(elem, ctx, nil); q != "" {
		return ifaces[aliasTarget(q, ctx.aliases)]
	}
	return false
}

// goRangeOverRegistry reports whether a range iterates a registry field of the
// method's own receiver (`range s.clients`).
func goRangeOverRegistry(x *ast.RangeStmt, ctx resolveCtx) bool {
	if len(ctx.registryFields) == 0 || ctx.recvVar == "" {
		return false
	}
	sel, ok := x.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	root, ok := sel.X.(*ast.Ident)
	return ok && root.Name == ctx.recvVar && ctx.registryFields[ctx.pkgDir+"."+ctx.recvType+"."+sel.Sel.Name]
}

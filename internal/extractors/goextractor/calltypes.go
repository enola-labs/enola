package goextractor

import (
	"go/ast"
	"go/token"
	"go/types"
)

// This file is the part of call resolution that needs to know what TYPE a value
// has. The extractor parses and does not type-check, so a type is known here only
// where the source states it: a parameter's declared type, a function's declared
// results, a type assertion, a composite literal. Everything else stays unknown, and
// a call on a value of unknown type is left as it was, unresolved or unrecorded. A
// missing edge beats a wrong one.

// callTypeTables are the module-wide tables call resolution reads besides field types.
type callTypeTables struct {
	returns map[string][]string // "pkgDir.Func" / "pkgDir.Type.Method" → declared result types
	aliases map[string]string   // "pkgDir.Alias" → the type it is an alias of
}

// collectAliases maps each alias a package declares (`type Store = facts.Store`) to
// the type it names. An alias is another name for a type, so a value declared under
// it has that type's methods, and those are declared, and named in the graph, under
// the type itself. A defined type (`type Store facts.Store`, no `=`) is a new type
// with none of them, and is not collected.
func collectAliases(files []*ast.File, pkgDir, modulePath string, pkgNames map[string]string) map[string]string {
	m := make(map[string]string)
	// A name the package also declares as a type of its own is not followed. Files
	// are read without evaluating build constraints, so `type Signature =
	// object.Signature` under one tag and `type Signature struct{…}` with methods
	// under another are both here, and the methods are real: they are declared, and
	// named in the graph, under the package's own name.
	defined := make(map[string]bool)
	for _, f := range files {
		ctx := resolveCtx{pkgDir: pkgDir, imports: buildFileImports(f, modulePath, pkgNames)}
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
				if !ts.Assign.IsValid() {
					defined[ts.Name.Name] = true
					continue
				}
				if ts.TypeParams != nil {
					continue
				}
				if target := declaredType(ts.Type, ctx, nil); target != "" && target != pkgDir+"."+ts.Name.Name {
					m[pkgDir+"."+ts.Name.Name] = target
				}
			}
		}
	}
	for name := range defined {
		delete(m, pkgDir+"."+name)
	}
	return m
}

// aliasTarget follows qualified through the alias table to the type it finally
// names. Aliases chain (`type A = B; type B = c.C`); the hop limit is only there so
// a cycle the compiler would reject cannot loop here.
func aliasTarget(qualified string, aliases map[string]string) string {
	for range 8 {
		next, ok := aliases[qualified]
		if !ok {
			break
		}
		qualified = next
	}
	return qualified
}

// declaredType returns the qualified name of the named type expr spells, as fact
// names carry it ("internal/auth.Service"), or "" when expr names none this
// extractor can stand behind: a predeclared type, a type parameter, or a composite
// type (slice, map, func, channel, anonymous struct or interface).
func declaredType(expr ast.Expr, ctx resolveCtx, typeParams map[string]bool) string {
	typeStr := typeExprToString(expr)
	if typeStr == "" {
		return ""
	}
	if id, bare := stripPointer(expr).(*ast.Ident); bare {
		// `error`, `string`, `any`: not symbols. And T in func F[T any](x T) is no type
		// at all outside the function.
		if typeParams[id.Name] || types.Universe.Lookup(id.Name) != nil {
			return ""
		}
	}
	return resolveTypeName(typeStr, ctx)
}

func stripPointer(expr ast.Expr) ast.Expr {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		case *ast.IndexExpr: // Box[T]
			expr = e.X
		case *ast.IndexListExpr: // Pair[K, V]
			expr = e.X
		default:
			return expr
		}
	}
}

// typeParamNames collects the type parameters in scope of a function: its own and,
// for a method, the names its receiver binds (`func (b *Box[T]) Get() T`).
func typeParamNames(recv *ast.FieldList, ft *ast.FuncType) map[string]bool {
	var names map[string]bool
	add := func(name string) {
		if names == nil {
			names = make(map[string]bool)
		}
		names[name] = true
	}
	if ft != nil && ft.TypeParams != nil {
		for _, field := range ft.TypeParams.List {
			for _, n := range field.Names {
				add(n.Name)
			}
		}
	}
	if recv != nil && len(recv.List) > 0 {
		t := recv.List[0].Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		var args []ast.Expr
		switch x := t.(type) {
		case *ast.IndexExpr:
			args = []ast.Expr{x.Index}
		case *ast.IndexListExpr:
			args = x.Indices
		}
		for _, a := range args {
			if id, ok := a.(*ast.Ident); ok {
				add(id.Name)
			}
		}
	}
	return names
}

// resultTypes returns one entry per result of ft, "" where the result has no
// nameable type. The count is exact, so a caller can tell `x := f()` from
// `x, err := f()`.
func resultTypes(ft *ast.FuncType, ctx resolveCtx, typeParams map[string]bool) []string {
	if ft == nil || ft.Results == nil {
		return nil
	}
	var out []string
	for _, field := range ft.Results.List {
		t := declaredType(field.Type, ctx, typeParams)
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			out = append(out, t)
		}
	}
	return out
}

// collectReturnTypes maps every function, method and interface method a package
// declares, by the name its fact carries, to its result types. Built for the whole
// module before any package is extracted, the way field types are, so a call into
// another package knows what it gets back.
func collectReturnTypes(files []*ast.File, pkgDir, modulePath string, pkgNames map[string]string, aliases map[string]string) map[string][]string {
	m := make(map[string][]string)
	for _, f := range files {
		ctx := resolveCtx{pkgDir: pkgDir, imports: buildFileImports(f, modulePath, pkgNames), aliases: aliases}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) > 0 {
					recv := typeExprToString(d.Recv.List[0].Type)
					if recv == "" {
						continue
					}
					name = recv + "." + name
				}
				if rt := resultTypes(d.Type, ctx, typeParamNames(d.Recv, d.Type)); len(rt) > 0 {
					m[pkgDir+"."+name] = rt
				}
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					iface, ok := ts.Type.(*ast.InterfaceType)
					if !ok || iface.Methods == nil {
						continue
					}
					var typeParams map[string]bool
					if ts.TypeParams != nil {
						typeParams = make(map[string]bool)
						for _, field := range ts.TypeParams.List {
							for _, n := range field.Names {
								typeParams[n.Name] = true
							}
						}
					}
					for _, method := range iface.Methods.List {
						ft, ok := method.Type.(*ast.FuncType)
						if !ok {
							continue // an embedded interface
						}
						for _, n := range method.Names {
							if rt := resultTypes(ft, ctx, typeParams); len(rt) > 0 {
								m[pkgDir+"."+ts.Name.Name+"."+n.Name] = rt
							}
						}
					}
				}
			}
		}
	}
	return m
}

// signatureTypes returns the declared types of a function's parameters and named
// results, by name. These are the surest types in a body: `func Analyze(r
// resolve.Resolver)` says outright what `r.NodeName(...)` is called on.
func signatureTypes(ft *ast.FuncType, ctx resolveCtx, typeParams map[string]bool) map[string]string {
	if ft == nil {
		return nil
	}
	var out map[string]string
	for _, list := range []*ast.FieldList{ft.Params, ft.Results} {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			// A variadic parameter is a slice of its element type, not a value of it.
			if _, variadic := field.Type.(*ast.Ellipsis); variadic {
				continue
			}
			t := declaredType(field.Type, ctx, typeParams)
			if t == "" {
				continue
			}
			for _, n := range field.Names {
				if n.Name == "_" {
					continue
				}
				if out == nil {
					out = make(map[string]string)
				}
				out[n.Name] = t
			}
		}
	}
	return out
}

// exprType returns the qualified type of an expression where the source states it,
// or "". It reads through the forms a call chain is built from: a call's declared
// result, a field, a type assertion, a literal.
func exprType(expr ast.Expr, ctx resolveCtx) string {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return exprType(e.X, ctx)
	case *ast.StarExpr:
		return exprType(e.X, ctx)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return exprType(e.X, ctx)
		}
	case *ast.CompositeLit:
		return compositeLitType(e, ctx)
	case *ast.TypeAssertExpr:
		if e.Type != nil { // nil in a type switch's x.(type)
			return declaredType(e.Type, ctx, nil)
		}
	case *ast.CallExpr:
		return callResultType(e, ctx)
	case *ast.Ident:
		if e.Name == ctx.recvVar && ctx.recvType != "" {
			return ctx.pkgDir + "." + ctx.recvType
		}
		return ctx.localTypes[e.Name]
	case *ast.SelectorExpr:
		// A field: the type of what it is selected from, then the field's own.
		if owner := exprType(e.X, ctx); owner != "" {
			if ft, ok := ctx.fieldTypes[owner+"."+e.Sel.Name]; ok {
				return resolveTypeName(ft, ctx)
			}
		}
	}
	return ""
}

// callTarget resolves the function a call expression calls to its fact name, or "".
// It is resolveChain for a callee that is a plain selector chain, and reads through
// a chain that passes over a call or an assertion: `s.resolver(store).NodeName(x)`
// is NodeName on whatever resolver is declared to return.
func callTarget(fun ast.Expr, ctx resolveCtx) string {
	if chain := flattenSelector(fun); chain != nil {
		return resolveChain(chain, ctx)
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if owner := exprType(sel.X, ctx); owner != "" {
		return owner + "." + sel.Sel.Name
	}
	return ""
}

// callResultTypes returns the declared result types of the function call calls,
// nil when that function is not one this module declares.
func callResultTypes(call *ast.CallExpr, ctx resolveCtx) []string {
	if ctx.returnTypes == nil {
		return nil
	}
	target := callTarget(call.Fun, ctx)
	if target == "" {
		return nil
	}
	return ctx.returnTypes[target]
}

// callResultType is the type of a call used as a single value. A declared result
// is exact. Where the callee is not declared in this module, the New<Type>
// convention is the one guess this extractor has always made.
func callResultType(call *ast.CallExpr, ctx resolveCtx) string {
	if rt := callResultTypes(call, ctx); rt != nil {
		if len(rt) == 1 {
			return rt[0]
		}
		return ""
	}
	return constructorReturnType(call.Fun, ctx)
}

package goextractor

import "go/ast"

// A function used as a VALUE is a reference to it, and until now the extractor saw
// none of them. analyzeBody walks a function body looking for CallExpr, so it records
// `readGoMod(rc, f)` and misses `"go.mod": readGoMod` — the dispatch-table entry that
// is the only thing keeping that parser reachable. Package-level initializers were
// worse off still: nothing walked them at all, so `var repoRoot = findRepoRoot()` was
// invisible even though it is a plain call.
//
// Both shapes read as dead code at HIGH confidence, the tier documented as the safest
// to delete. On this repository 23 of 27 first-party high-confidence findings were one
// of these two, every one of them false. It is the same failure as the predeclared-
// identifier shadowing in resolveChain: a missing edge does not merely lose a fact, it
// manufactures a confident wrong answer about the symbol on the other end.
//
// Resolution is deliberately narrow — a bare identifier naming one of THIS package's
// top-level functions. A cross-package function value (`install.Foo` handed to a
// callback) needs import resolution this pass does not attempt, so it stays missing.
// A missing edge beats a wrong one.

// funcValueRefs returns qualified names for this package's top-level functions that
// appear in n as a value rather than as the thing being called. Calls are already
// recorded by analyzeBody, so counting them here would double-count.
func funcValueRefs(n ast.Node, ctx resolveCtx) []string {
	return packageFuncRefs(n, ctx, true)
}

// packageLevelFuncRefs returns qualified names for this package's top-level functions
// referenced anywhere in n, called or not. It is used for package-level var and const
// initializers, which no body walk reaches, so here a call is a reference too.
func packageLevelFuncRefs(n ast.Node, ctx resolveCtx) []string {
	return packageFuncRefs(n, ctx, false)
}

func packageFuncRefs(n ast.Node, ctx resolveCtx, valueOnly bool) []string {
	if n == nil || len(ctx.pkgFuncs) == 0 {
		return nil
	}

	// Positions where an identifier that happens to match a function name is not a
	// reference to it, and names that something in n redeclares.
	skip := make(map[*ast.Ident]bool)
	shadowed := make(map[string]bool)

	note := func(id *ast.Ident) {
		if id != nil {
			skip[id] = true
			shadowed[id.Name] = true
		}
	}

	ast.Inspect(n, func(node ast.Node) bool {
		switch e := node.(type) {
		case *ast.CallExpr:
			// The callee itself, when only values are wanted. Arguments are still
			// walked: `rc.lock(relFile, "yarn.lock", yarnLock)` is the whole point.
			if valueOnly {
				if id, ok := e.Fun.(*ast.Ident); ok {
					skip[id] = true
				}
			}
		case *ast.KeyValueExpr:
			// In a struct literal the key is a FIELD name — `Side: sourceSide` must
			// credit sourceSide and not a same-named function called Side. A map
			// literal keyed by a function value loses its edge to this rule, which is
			// the trade the package doc describes.
			if id, ok := e.Key.(*ast.Ident); ok {
				skip[id] = true
			}
		case *ast.SelectorExpr:
			// x.Foo is a field or method on some other value, never a bare reference
			// to this package's top-level Foo.
			skip[e.Sel] = true
		case *ast.AssignStmt:
			for _, lhs := range e.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					note(id)
				}
			}
		case *ast.ValueSpec:
			for _, name := range e.Names {
				note(name)
			}
		case *ast.Field:
			for _, name := range e.Names {
				note(name)
			}
		case *ast.FuncDecl:
			skip[e.Name] = true
		case *ast.LabeledStmt:
			skip[e.Label] = true
		}
		return true
	})

	var out []string
	seen := make(map[string]bool)
	ast.Inspect(n, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok || skip[id] || shadowed[id.Name] || !ctx.pkgFuncs[id.Name] {
			return true
		}
		// Same qualification resolveChain gives a bare same-package call, so a value
		// reference and a call to one function land on the same node.
		q := ctx.pkgDir + "." + id.Name
		if !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
		return true
	})
	return out
}

// packageLevelRefs collects the function references in a file's package-level var and
// const initializers. Nothing else walks them: extractValueSpec emits a symbol for the
// exported ones and reads no expression, so a dispatch table's contents were unseen
// whether or not the table itself was exported.
func packageLevelRefs(f *ast.File, ctx resolveCtx) []string {
	var out []string
	seen := make(map[string]bool)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, val := range vs.Values {
				for _, ref := range packageLevelFuncRefs(val, ctx) {
					if !seen[ref] {
						seen[ref] = true
						out = append(out, ref)
					}
				}
			}
		}
	}
	return out
}

// packageLevelInitRefs collects every function reference in a package's var and const
// initializers, across all of its files. Imports are per file, so each file gets its
// own resolveCtx even though only pkgFuncs matters to this pass today.
func packageLevelInitRefs(pp *parsedPkg, pkgDir, modulePath string, pkgNames map[string]string, pkgFuncs map[string]bool) []string {
	var out []string
	seen := make(map[string]bool)
	for _, relFile := range pp.relFiles {
		f, ok := pp.fileMap[relFile]
		if !ok {
			continue
		}
		ctx := resolveCtx{
			pkgDir:     pkgDir,
			modulePath: modulePath,
			imports:    buildFileImports(f, modulePath, pkgNames),
			pkgFuncs:   pkgFuncs,
		}
		for _, ref := range packageLevelRefs(f, ctx) {
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	return out
}

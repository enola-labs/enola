package goextractor

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// isGoTestFile reports whether a repo-relative path is a Go test file. The Go
// toolchain defines the suffix, so this needs no directory scoping — a production
// file cannot legally be named *_test.go and still compile into the package.
func isGoTestFile(relFile string) bool { return strings.HasSuffix(relFile, "_test.go") }

// ExtractTestRefs implements plugin.TestRefExtractor. It parses *_test.go files for
// the SOLE purpose of capturing their outbound references into production code,
// emitting one facts.KindTestRef fact per file that carries only RelCalls edges —
// no symbols. Test functions therefore never become dead-code candidates, and no
// symbol/module/route explainer is affected, while the dead-code detector can see
// that a production function is exercised by a test and not mis-report it as dead.
//
// The engine hands every TestGlob match to every TestRefExtractor whose repo it
// detected, scoped by plugin.FileOwner when the extractor implements it.
// GoExtractor deliberately does not: FileOwner is what opts an extractor into the
// incremental cache (see its doc comment), and implementing it here would both
// enable Go caching and pull .go files out of computeExtractorKeys' shared
// partition, changing the shared hash that keys EVERY other extractor. So the
// filter lives here instead — Ruby's ExtractTestRefs filters internally too.
// prodFiles is read for its go.mod files only: Go call targets resolve
// lexically through the repository's modules and each file's own imports, so
// this pass needs no other view of which production files exist.
func (e *GoExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, prodFiles []string) ([]facts.Fact, error) {
	var goFiles []string
	for _, relFile := range testFiles {
		if isGoTestFile(relFile) {
			goFiles = append(goFiles, relFile)
		}
	}
	if len(goFiles) == 0 {
		return nil, nil
	}

	// The go.mod files are production files: the modules come from there.
	mods := readModules(repoPath, prodFiles)

	// One package at a time, because a test reaches production code through what
	// the package's own functions return: `testRunner().Doctor(...)`, or
	// `s := New(cfg); s.Serve()`. Those return types come from the directory's test
	// files (where helpers like testRunner live) and its production files (where
	// constructors live), so both are read for each directory that has tests.
	testsByDir := map[string][]string{}
	var dirs []string
	for _, rel := range goFiles {
		dir := factpath.Dir(rel)
		if _, ok := testsByDir[dir]; !ok {
			dirs = append(dirs, dir)
		}
		testsByDir[dir] = append(testsByDir[dir], rel)
	}
	prodByDir := map[string][]string{}
	for _, rel := range prodFiles {
		if strings.HasSuffix(rel, ".go") && !isGoTestFile(rel) {
			if dir := factpath.Dir(rel); testsByDir[dir] != nil {
				prodByDir[dir] = append(prodByDir[dir], rel)
			}
		}
	}

	perDir := parallel.MapFiles(ctx, dirs, func(dir string) []facts.Fact {
		return refsFromGoTestDir(repoPath, dir, testsByDir[dir], prodByDir[dir], mods)
	})

	var out []facts.Fact
	for _, ff := range perDir {
		out = append(out, ff...)
	}
	return out, nil
}

// refsFromGoTestDir is refsFromGoTestFile for every test file of one directory, sharing
// the return types its functions declare.
func refsFromGoTestDir(repoPath, dir string, tests, prods []string, mods goModules) []facts.Fact {
	type parsed struct {
		rel string
		f   *ast.File
	}
	var testAST []parsed
	var all []*ast.File
	for _, rel := range tests {
		if f := parseGoTest(repoPath, rel); f != nil {
			testAST = append(testAST, parsed{rel, f})
			all = append(all, f)
		}
	}
	for _, rel := range prods {
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		if f, err := parser.ParseFile(token.NewFileSet(), rel, src, parser.SkipObjectResolution); err == nil {
			all = append(all, f)
		}
	}
	returns := collectReturnTypes(all, mods.importPathOf(dir), mods, nil, nil)

	var out []facts.Fact
	for _, p := range testAST {
		out = append(out, refsFromGoTestFile(p.f, p.rel, mods, returns)...)
	}
	return out
}

// parseGoTest reads and parses one test file, keeping its clean declarations when it
// parses only in part.
func parseGoTest(repoPath, relFile string) *ast.File {
	src, err := os.ReadFile(filepath.Join(repoPath, relFile))
	if err != nil {
		log.Printf("[go-extractor] error reading test file %s: %v", relFile, err)
		return nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), relFile, src, parser.SkipObjectResolution)
	if err != nil {
		if f = keepCleanDecls(f); f == nil {
			log.Printf("[go-extractor] error parsing test file %s: %v", relFile, err)
			return nil
		}
		log.Printf("[go-extractor] read test file %s in part: %v", relFile, err)
	}
	return f
}

// refsFromGoTestFile returns, for one parsed Go test file, a single reference-only fact
// carrying the production symbols it calls, or nil when it references nothing.
//
// Call targets are resolved with the PRODUCTION resolvers (callTarget,
// collectLocalTypes), so a reference from a test is spelled exactly
// as the same reference from production code would be, and the dead-code detector
// needs no special case. That also inherits goBuiltins filtering, so len/make/min
// never become phantom targets.
//
// Only call expressions yield targets — matching analyzeBody, which likewise
// ignores composite literals. A type constructed only as `Foo{}` from a test is
// therefore still reported dead, but so is one constructed only that way from
// production code: that blind spot is pre-existing and not specific to tests.
//
// returns holds the return types the package's functions declare, so a call through a
// function's result resolves.
func refsFromGoTestFile(f *ast.File, relFile string, mods goModules, returns map[string][]string) []facts.Fact {
	base := resolveCtx{
		returnTypes: returns,
		pkgDir:      mods.importPathOf(factpath.Dir(relFile)),
		mods:        mods,
		// pkgNames is deliberately nil. It exists to recover a declared package name
		// that differs from its directory base ("go-auth" → package auth), which
		// needs a view of every parsed package — and this pass sees only test files.
		// Worse, a test file's own package name carries a _test suffix
		// (`package svc_test`), so feeding these in would alias the import under
		// test as "svc_test" and break the very idiom this exists to resolve.
		// buildFileImports then falls back to the import path's base, exactly as the
		// production pass does for any package it did not parse.
		imports: buildFileImports(f, mods, nil),
	}

	seen := make(map[string]bool)
	var rels []facts.Relation
	add := func(target string) {
		if target == "" || seen[target] {
			return
		}
		seen[target] = true
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: target})
	}
	collectCalls := func(n ast.Node, ctx resolveCtx) {
		ast.Inspect(n, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				// callTarget, as the production pass resolves calls: a plain chain
				// resolves as it always did, and one through a call's result
				// (`testRunner().Doctor(...)`) resolves to a method of the type
				// the called function returns.
				add(callTarget(call.Fun, ctx))
			}
			return true
		})
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			ctx := base
			if d.Recv != nil && len(d.Recv.List) > 0 {
				field := d.Recv.List[0]
				ctx.recvType = typeExprToString(field.Type)
				if len(field.Names) > 0 {
					ctx.recvVar = field.Names[0].Name
				}
			}
			ctx.localTypes = collectLocalTypes(d.Body, ctx)
			collectCalls(d.Body, ctx)
		case *ast.GenDecl:
			// File-scope initializers (`var _ = Register(handler)`) reference
			// production code with no enclosing function to attribute them to.
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					collectCalls(v, base)
				}
			}
		}
	}

	if len(rels) == 0 {
		return nil
	}
	return []facts.Fact{{
		Kind:      facts.KindTestRef,
		Name:      relFile,
		File:      relFile,
		Props:     map[string]any{"language": "go"},
		Relations: rels,
	}}
}

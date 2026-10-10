package pythonextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It parses pytest files for
// the SOLE purpose of capturing their outbound references into production code,
// emitting one facts.KindTestRef fact per file carrying call and named-reference
// edges — no symbols, modules or routes.
//
// Emitting nothing but references is what makes this safe to re-enable over files
// the ignore globs deliberately exclude. The FastAPI router-topology pass runs in
// Extract and is never reached from here, so a pytest fixture that assembles a
// throwaway app — app.include_router(get_x_router(), prefix="/x") — cannot put its
// test-only prefix back into the production route graph. That regression is what
// motivated ignoring Python tests in the first place; this pass must not undo it.
//
// prodFiles is the snapshot's production file list. Python spells an
// absolute-import call target as a dotted path ("pkg.mod.func"), and only the set
// of real modules says whether that names internal code or a third-party package,
// so without it resolveCallTargets would drop every production target as external
// and the emitted facts would carry no usable edges.
func (e *PythonExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, prodFiles []string) ([]facts.Fact, error) {
	var pyTests []string
	for _, relFile := range testFiles {
		if isPythonFile(relFile) {
			pyTests = append(pyTests, relFile)
		}
	}
	if len(pyTests) == 0 {
		return nil, nil
	}

	perFile := parallel.MapFiles(ctx, pyTests, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[python-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		return refsFromPyTest(src, relFile)
	})

	var out []facts.Fact
	for _, ff := range perFile {
		out = append(out, ff...)
	}
	if len(out) == 0 {
		return nil, nil
	}

	// Rewrite dotted absolute-import targets into canonical slash symbol names,
	// dropping stdlib/third-party edges. resolveCallTargets already accepts
	// KindTestRef, so nothing there needs to change.
	//
	// The module set is built from PRODUCTION files only. Including the test files
	// would let a target resolve to another test module — an edge into code that
	// has no symbol facts, which can mark nothing live and only adds noise.
	fileModules := make(map[string]bool, len(prodFiles))
	for _, f := range prodFiles {
		if isPythonFile(f) {
			fileModules[strings.TrimSuffix(f, ".py")] = true
		}
	}
	// Package-level definitions and re-exports live in __init__.py. The test
	// walker deliberately has no production index, so supply just these package
	// facts to the resolver rather than parsing the whole production tree twice.
	var initFiles []string
	for _, f := range prodFiles {
		if isInitFile(f) {
			initFiles = append(initFiles, f)
		}
	}
	resolved := append([]facts.Fact(nil), out...)
	for _, ff := range parallel.MapFiles(ctx, initFiles, func(rel string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			return nil
		}
		ff, _ := extractFileAST(src, rel, false, false, false, nil)
		return ff
	}) {
		resolved = append(resolved, ff...)
	}
	modules := make(map[string]bool)
	for _, file := range prodFiles {
		if isPythonFile(file) {
			modules[fileDir(file)] = true
		}
	}
	pkgDirs := packageDirs(prodFiles)
	resolveImports(resolved, modules, fileModules, pkgDirs)
	resolveCallTargets(resolved, fileModules, pkgDirs)
	out = resolved[:len(out)]

	// Keep only targets that resolved to a canonical symbol name. resolveCallTargets
	// rewrites internal dotted paths to slash form and drops external ones, so a
	// target still lacking a '/' is one nothing could resolve — a bare imported
	// third-party name (FastAPI), a local variable, a builtin.
	//
	// Those must not survive. The dead-code detector falls back to SHORT-NAME
	// matching, so a stray "FastAPI" would mark any production symbol of that name
	// live, silently converting real dead code into a false negative. That is the
	// one outcome worse than the false positive this pass exists to remove, so an
	// unresolved target is dropped rather than guessed at. TypeScript deliberately
	// keeps short names (its resolver only ever ADDS references); Python resolves
	// internal calls to slash form already, so here a bare name signals failure.
	kept := out[:0]
	for _, f := range out {
		rels := f.Relations[:0]
		for _, rel := range f.Relations {
			if strings.ContainsRune(rel.Target, '/') {
				rels = append(rels, rel)
			}
		}
		f.Relations = rels
		if len(f.Relations) > 0 {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// refsFromPyTest parses one test file and returns a single reference-only fact
// carrying the production symbols it calls, or nil when it references nothing.
//
// It reuses the production walker rather than a bespoke one, so a reference from a
// test is spelled exactly as the same reference from production code would be and
// the dead-code detector needs no special case. The walker's own facts are then
// DISCARDED — only their outbound relations survive — which is what keeps this pass
// within the KindTestRef contract.
//
// Two arguments are deliberately degraded:
//
//   - idx is nil. Building the global symbol index means parsing every production
//     file (Extract's Pass 1, the expensive half of Python extraction), and doing it
//     again for a reference-only pass would roughly double snapshot cost. The walker
//     nil-checks every idx lookup. The cost is receiver-typed method calls
//     (obj.method()) going unresolved — conservative, since an unresolved target is
//     dropped and its symbol simply stays the dead-code candidate it is today.
//     Import-based references, the dominant pytest idiom, are unaffected.
//   - The framework flags are false. A test file's decorators are not production
//     routes, and the walker cannot emit route facts it was never told to look for.
//     Belt-and-braces: the facts are discarded regardless.
func refsFromPyTest(src []byte, relFile string) []facts.Fact {
	ff, _ := extractFileAST(src, relFile, false, false, false, nil)
	if len(ff) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var rels []facts.Relation
	for _, f := range ff {
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelCalls && rel.Kind != facts.RelInstantiates && rel.Kind != facts.RelNames {
				continue
			}
			// Self-references inside the test file resolve to the test's own module,
			// which has no symbol facts — a helper calling another helper marks
			// nothing live. Drop them so the fact carries only outbound edges.
			kind := facts.RelCalls
			if rel.Kind == facts.RelNames {
				kind = facts.RelNames
			}
			key := kind + "\x00" + rel.Target
			if rel.Target == "" || seen[key] || strings.HasPrefix(rel.Target, strings.TrimSuffix(relFile, ".py")+".") {
				continue
			}
			seen[key] = true
			rels = append(rels, facts.Relation{Kind: kind, Target: rel.Target})
		}
	}
	if len(rels) == 0 {
		return nil
	}
	return []facts.Fact{{
		Kind:      facts.KindTestRef,
		Name:      relFile,
		File:      relFile,
		Props:     map[string]any{"language": "python"},
		Relations: rels,
	}}
}

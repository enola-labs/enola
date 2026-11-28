package cppextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/inheritedcalls"
	"github.com/enola-labs/enola/internal/parallel"
)

// The test pass recovers what C and C++ test files call (test/, tests/ and testing/
// trees, which the test globs set aside) as one facts.KindTestRef fact per file,
// carrying only RelCalls edges. It runs inside the production extraction
// (ExtractWithTests), so it is cached with it and parses only the test files.
//
// C and C++ resolve a call through the whole program: the include graph, the type
// index and the repository's function names (canonicalizeTargets), then the
// inherited-calls binder for a member reached through a base class or a field's
// type. A walk of the test files alone would leave a member call on a typed local
// or a qualified call unresolved, and its bare name would then vouch for every
// same-named method in the repository. So the test files are resolved against the
// production files' pass-one facts, exactly as when tests were indexed with them,
// while the production facts themselves resolve without the tests. The production
// side enters as a copy holding only what resolution reads (names, the props of
// declarations, base classes, includes), because resolution rewrites what it is
// given and the production pass still has to run on the originals.

// testPass carries the test files through one extraction. A nil pass has no
// C or C++ test files and does nothing.
type testPass struct {
	files  []string          // test files the grammar parses, sorted
	lang   map[string]string // test file -> "c" / "cpp"
	isTest map[string]bool
	macros macroTable // every file's #defines merged in path order, tests included
	joint  []string   // production and test C/C++ files, sorted
}

func newTestPass(ctx context.Context, repoPath string, testFiles []string, hdrLang map[string]string, prodFiles []string, prodMacros []macroTable) *testPass {
	tp := &testPass{lang: map[string]string{}, isTest: map[string]bool{}}
	for _, relFile := range testFiles {
		if lang := parsedLanguage(relFile, hdrLang); lang != "" {
			tp.files = append(tp.files, relFile)
			tp.lang[relFile] = lang
			tp.isTest[factpath.Slash(relFile)] = true
		}
	}
	if len(tp.files) == 0 {
		return nil
	}
	sort.Strings(tp.files)
	testMacros := parallel.MapFiles(ctx, tp.files, func(relFile string) macroTable {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			return nil
		}
		t := macroTable{}
		collectMacros(src, t)
		return t
	})

	// Last write wins in path order, as one extraction over both sets merged them.
	type fileMacros struct {
		path  string
		table macroTable
	}
	all := make([]fileMacros, 0, len(prodFiles)+len(tp.files))
	for i, f := range prodFiles {
		all = append(all, fileMacros{f, prodMacros[i]})
	}
	for i, f := range tp.files {
		all = append(all, fileMacros{f, testMacros[i]})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].path < all[j].path })
	tp.macros = macroTable{}
	tp.joint = make([]string, 0, len(all))
	for _, fm := range all {
		tp.joint = append(tp.joint, fm.path)
		for name, def := range fm.table {
			tp.macros[name] = def
		}
	}
	return tp
}

// resolve parses the test files and returns their test refs. prod is the
// production pass-one output (symbols, dependencies and modules, before any
// resolution); it is read, never written.
func (tp *testPass) resolve(ctx context.Context, repoPath string, prod []facts.Fact) []facts.Fact {
	if tp == nil {
		return nil
	}
	perFile := parallel.MapFiles(ctx, tp.files, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[cpp-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		return extractFileAST(src, relFile, tp.lang[relFile], tp.macros)
	})

	joint := resolutionView(prod)
	// The production pass names a header static's alternatives as one declaration
	// of its directory and fuses a static prototype with its definition before it
	// resolves; the view is named the same, or a test ref names a fact production
	// does not have, or none where two names make the short name ambiguous.
	foldHeaderAlternatives(joint)
	fuseStaticPrototypes(joint)
	moduleRefs := map[string][]facts.Relation{}
	dirLang := map[string]string{}
	for i, fileFacts := range perFile {
		dir := factpath.Dir(tp.files[i])
		for _, f := range fileFacts {
			if f.Kind == facts.KindModule {
				moduleRefs[dir] = append(moduleRefs[dir], f.Relations...)
				continue
			}
			joint = append(joint, f)
		}
		if tp.lang[tp.files[i]] == langCpp {
			dirLang[dir] = langCpp
		} else if dirLang[dir] == "" {
			dirLang[dir] = langC
		}
	}
	// Pass one met the files in path order; the indices are built in that order.
	sort.SliceStable(joint, func(i, j int) bool { return joint[i].File < joint[j].File })
	types := newTypeIndex()
	funcNames := map[string]bool{}
	for _, f := range joint {
		if f.Kind != facts.KindSymbol {
			continue
		}
		simple := lastScopeComponent(f.Name)
		if simple == "" {
			continue
		}
		switch f.PropString("symbol_kind") {
		case facts.SymbolClass, facts.SymbolStruct, facts.SymbolEnum, facts.SymbolInterface:
			types.add(simple, factpath.Dir(f.File), f.Name)
		case facts.SymbolFunc, facts.SymbolMethod:
			funcNames[simple] = true
		}
	}
	dirs := make([]string, 0, len(moduleRefs))
	for dir := range moduleRefs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		joint = append(joint, facts.Fact{Kind: facts.KindModule, Name: dir, File: dir,
			Props: map[string]any{"language": dirLang[dir]}, Relations: moduleRefs[dir]})
	}

	resolveFuncPtrRefs(joint, funcNames)
	joint = dedupeSymbols(joint)
	visibility := buildIncludeVisibility(joint, buildHeaderPathIndex(tp.joint), buildHeaderDirIndex(tp.joint))
	canonicalizeTargets(joint, types, indexFunctionNames(joint), visibility)
	resolveConstructions(joint, types, visibility)

	store := facts.NewStore()
	store.Add(joint...)
	if err := inheritedcalls.New().Bind(ctx, store); err != nil {
		log.Printf("[cpp-extractor] test refs: %v", err)
	}
	return testRefsFrom(store.FactsRef(), tp.files, tp.isTest)
}

// resolutionProps are the props of a production declaration that resolution and
// the inherited-calls binder read.
var resolutionProps = []string{
	"symbol_kind", "has_body", "receiver", "language", propReturnType, "superclass",
	"abstract", "final", "override", "exported", "static", facts.PropFieldTypes,
	"include", "include_style", propFactoryNew, propFactoryForwards, propFieldTemplates,
}

// resolutionView copies the production facts test resolution reads: each
// declaration with those props and its base classes, and each include. The copy
// is what resolution rewrites; the originals stay as pass one left them.
func resolutionView(prod []facts.Fact) []facts.Fact {
	out := make([]facts.Fact, 0, len(prod))
	for i := range prod {
		f := &prod[i]
		if f.Kind != facts.KindSymbol && f.Kind != facts.KindDependency {
			continue
		}
		v := facts.Fact{Kind: f.Kind, Name: f.Name, File: f.File, Line: f.Line}
		for _, k := range resolutionProps {
			if val, ok := f.Props[k]; ok {
				v.SetProp(k, val)
			}
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				v.Relations = append(v.Relations, r)
			}
		}
		out = append(out, v)
	}
	return out
}

// withTests is files followed by testFiles, without writing into files.
func withTests(files, testFiles []string) []string {
	if len(testFiles) == 0 {
		return files
	}
	out := make([]string, 0, len(files)+len(testFiles))
	return append(append(out, files...), testFiles...)
}

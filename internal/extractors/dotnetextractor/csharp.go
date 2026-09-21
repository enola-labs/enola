// Package csharpextractor extracts architectural facts from C# source code
// using tree-sitter AST parsing (see csharp_ast.go for the walker and
// resolve.go for the project index, type resolution and partial-type merge).
//
// The grammar is pinned to tree-sitter-c-sharp v0.23.1, which is NOT the latest.
// v0.23.2 and above are generated against tree-sitter ABI 15, while the vendored
// go-tree-sitter runtime accepts at most 14, so SetLanguage rejects them. The
// failure is silent by nature — every file parses to nothing, which looks exactly
// like a repository containing no C# — so extractFileAST logs once when the
// grammar is refused rather than returning an empty result unremarked.
package dotnetextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// CSharpExtractor extracts architectural facts from C# source code.
type CSharpExtractor struct{}

// New creates a new CSharpExtractor.
func New() *CSharpExtractor {
	return &CSharpExtractor{}
}

// Name is "dotnet", not "csharp". It read only C# when it was named, and now reads
// C#, VB.NET, F#, Razor, XAML and the MSBuild project system. The old name is
// still accepted in a config's `extractors:` list — see config.IsExtractorEnabled,
// and note that such a list REPLACES the default rather than merging, so a stale
// name silently disables the extractor instead of erroring.
func (e *CSharpExtractor) Name() string { return "dotnet" }

// Detect returns true if the repository looks like a .NET project: an MSBuild
// solution or project file of ANY .NET language, or any real C# source, within
// detectMaxDepth levels.
//
// A project file alone is not required, mirroring the Java extractor's reasoning
// in reverse: `global.json` and `Directory.Build.props` are .NET-only but appear
// in repositories that vendor a .NET tool without containing C#, while a `.cs`
// file is unambiguous — no other ecosystem uses that extension.
//
// Detecting on `.fsproj` and `.vbproj` is what stops a claimed repository from
// being an empty one. giraffe-fsharp/Giraffe ships `Giraffe.slnx`, 7 `.fsproj`
// and no `.cs` at all: this extractor matched the solution, claimed the
// repository, emitted zero facts and reported success — a green snapshot of 46
// unread sources, indistinguishable from an empty repo. Now the project graph is
// read whatever language the sources are in, so a claim always produces facts.
func (e *CSharpExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
//
// The four-level bound this replaces existed because walking dotnet/runtime to
// answer yes-or-no was judged wasted work. Measured, that objection points the
// other way: a full detection pass over dotnet/runtime cost 1.44s of exactly that
// bounded walking, repeated per extractor and again for the test-ref gate, and
// membership over the engine's list costs nothing at all.
//
// obj/ and bin/ stay excluded, and now by path segment rather than by the walk not
// reaching them: obj/ holds generated C# that would make any tree with a stale build
// directory look like a C# project.
func (e *CSharpExtractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, "obj", "bin") {
			continue
		}
		if isCSharpFile(rel) || isProjectFile(rel) || isSolutionFile(rel) ||
			isRazorFile(rel) || isXamlFile(rel) || isVBFile(rel) || isFSharpFile(rel) {
			return true, nil
		}
	}
	return false, nil
}

func isCSharpFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".cs")
}

// isProjectFile and isSolutionFile live in msbuild.go, which owns every MSBuild
// format this extractor reads.

// OwnsFile implements plugin.FileOwner for incremental caching. Project and
// solution files are owned alongside sources because they seed the project index
// and the assembly graph every module fact reads, so adding, renaming or editing
// one must invalidate the cache — editing a ProjectReference changes the graph
// without touching a single .cs file.
func (e *CSharpExtractor) OwnsFile(relFile string) bool {
	return isCSharpFile(relFile) || isProjectFile(relFile) || isSolutionFile(relFile) ||
		isRazorFile(relFile) || isXamlFile(relFile) || isVBFile(relFile) ||
		isFSharpFile(relFile)
}

// Extract parses C# files and emits architectural facts.
//
// Three phases. Every .csproj path is scanned first (path-only: no XML parsing)
// into a project-directory index, so each file's walk can name its owning project
// synchronously. Files are then parsed in parallel — extractFileAST is a pure
// function of (src, relFile, project) — and merged in file order. The remaining
// work genuinely needs the whole merged fact set: partial types are declared
// across files by design, and C# `using` imports a NAMESPACE rather than a type,
// so a bare type reference cannot be resolved from one file's imports the way
// Java's can. Both are settled in resolveCSharp once every file is in.
func (e *CSharpExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var csFiles, projFiles, slnFiles, razorFiles, xamlFiles, vbFiles, fsFiles []string
	for _, relFile := range files {
		switch {
		case isCSharpFile(relFile):
			csFiles = append(csFiles, relFile)
		case isProjectFile(relFile):
			projFiles = append(projFiles, relFile)
		case isSolutionFile(relFile):
			slnFiles = append(slnFiles, relFile)
		case isRazorFile(relFile):
			razorFiles = append(razorFiles, relFile)
		case isXamlFile(relFile):
			xamlFiles = append(xamlFiles, relFile)
		case isVBFile(relFile):
			vbFiles = append(vbFiles, relFile)
		case isFSharpFile(relFile):
			fsFiles = append(fsFiles, relFile)
		}
	}

	projects := buildProjectIndex(projFiles)
	msbuildProjects := parseProjects(repoPath, projFiles, slnFiles)

	perFileFacts := parallel.MapFiles(ctx, csFiles, func(relFile string) fileResult {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[dotnet-extractor] error reading %s: %v", relFile, err)
			return fileResult{}
		}
		if isGeneratedSource(relFile, src) {
			return fileResult{generated: true}
		}
		ff, sc := extractFileASTFull(src, relFile)
		return fileResult{facts: ff, scaffold: sc}
	})

	var allFacts []facts.Fact
	var scaffold aspnetScaffold
	// dir -> language -> file count. A module's language is the DOMINANT one among
	// the files that contributed to it, not a constant: this extractor now reads
	// five languages, and hardcoding "csharp" labelled a directory of .vb or .fs
	// sources as C# — which is what the layers explainer reads to gate its
	// pattern detection.
	modules := make(map[string]map[string]int)
	noteModule := func(rel, lang string) {
		dir := factpath.Dir(rel)
		if modules[dir] == nil {
			modules[dir] = map[string]int{}
		}
		modules[dir][lang]++
	}
	parsed := 0
	for i, r := range perFileFacts {
		if r.generated {
			continue
		}
		parsed++
		allFacts = append(allFacts, r.facts...)
		scaffold.merge(r.scaffold)
		noteModule(csFiles[i], "csharp")
	}
	if skipped := len(csFiles) - parsed; skipped > 0 {
		log.Printf("[dotnet-extractor] skipped %d generated file(s) of %d", skipped, len(csFiles))
	}

	// Every non-C# source language runs the same pass: read, scan, fold in, note
	// the module. They are listed rather than written out four times — the blocks
	// were identical apart from the scanner, and repeating them is what took this
	// function to cyclomatic 40 over the phases that added them.
	//
	// Razor and XAML run BEFORE the partial merge on purpose: a `.razor` component
	// and its `.razor.cs`, or an `x:Class` document and its `.xaml.cs`, are two
	// halves of one generated class and must converge into a single fact.
	//
	// VB.NET and F# are emitted into this same fact set rather than a separate one,
	// which is the point: a solution mixes languages inside one assembly, so a VB
	// class referencing a C# type has to resolve through the one shared type index
	// resolveCSharpTargets builds below.
	for _, pass := range []langPass{
		{lang: "razor", files: razorFiles, scan: func(rel string, src []byte) ([]facts.Fact, bool) {
			return razorFacts(string(src), rel), true
		}},
		{lang: "xaml", files: xamlFiles, scan: func(rel string, src []byte) ([]facts.Fact, bool) {
			return xamlFacts(string(src), rel), true
		}},
		{lang: "vbnet", files: vbFiles, scan: func(rel string, src []byte) ([]facts.Fact, bool) {
			if isGeneratedVB(rel, string(src)) {
				return nil, false
			}
			return scanVB(string(src), rel), true
		}},
		{lang: "fsharp", files: fsFiles, scan: func(rel string, src []byte) ([]facts.Fact, bool) {
			return scanFSharp(string(src), rel), true
		}},
	} {
		ff, read := runLangPass(ctx, repoPath, pass, noteModule)
		allFacts = append(allFacts, ff...)
		if len(pass.files) > 0 {
			log.Printf("[dotnet-extractor] parsed %d %s file(s) of %d", read, pass.lang, len(pass.files))
		}
	}

	allFacts = mergePartialTypes(allFacts)
	resolveCSharpTargets(allFacts)
	// After resolution: composeControllerRoutes walks the inheritance edges to find
	// an inherited [Route], and those targets are bare type names until resolution
	// canonicalises them.
	allFacts = append(allFacts, composeControllerRoutes(allFacts, scaffold)...)
	// After resolution too: an entity fact is named for the SYMBOL that declares
	// the type, which is only canonical once resolveCSharpTargets has run.
	allFacts = append(allFacts, composeStorageFacts(allFacts, scaffold.storage)...)
	allFacts = append(allFacts, clientRouteFacts(scaffold.clients)...)
	if len(scaffold.conventional) > 0 || scaffold.conventionalSkipped > 0 {
		symbolNames := make(map[string]bool, len(allFacts))
		for i := range allFacts {
			if allFacts[i].Kind == facts.KindSymbol {
				symbolNames[allFacts[i].Name] = true
			}
		}
		allFacts = append(allFacts, conventionalRouteFacts(scaffold.conventional, symbolNames)...)
		log.Printf("[dotnet-extractor] conventional routing: %d registration(s) resolved, "+
			"%d left generic (a {controller}/{action} template needs each controller's area)",
			len(scaffold.conventional), scaffold.conventionalSkipped)
	}
	computeCSharpPerformsIO(allFacts)

	// Module facts are built into a map keyed by directory, then overlaid with the
	// MSBuild assembly graph, so a project root that also holds sources yields one
	// fact rather than two facts the graph would have to merge.
	mods := make(map[string]*facts.Fact, len(modules)+len(msbuildProjects))
	for dir, byLang := range modules {
		props := map[string]any{
			"language":           dominantModuleLanguage(byLang),
			facts.PropModuleRole: facts.ModuleRoleForPath(dir),
		}
		if name, ok := projectForDir(dir, projects); ok {
			props["project"] = name
		}
		mods[dir] = &facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		}
	}
	nugetDeps := applyMSBuild(mods, msbuildProjects)

	dirs := make([]string, 0, len(mods))
	for dir := range mods {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		allFacts = append(allFacts, *mods[dir])
	}
	allFacts = append(allFacts, nugetDeps...)

	// len(csFiles), not `parsed`: a repository whose every .cs file was skipped as
	// generated is a different situation, and the generated-skip count already
	// reports it above.
	if len(csFiles) == 0 && len(msbuildProjects) > 0 {
		// The Giraffe case: projects found, no readable source. Say so rather than
		// reporting a successful snapshot of nothing — the assembly graph below is
		// real, but it is not the whole repository.
		langs := map[string]int{}
		for _, p := range msbuildProjects {
			langs[p.language]++
		}
		log.Printf("[dotnet-extractor] no C# sources: emitting the project graph only "+
			"(%d project(s) by language %v); sources in other .NET languages are not read yet",
			len(msbuildProjects), langs)
	}

	return allFacts, nil
}

// dominantModuleLanguage picks the language that contributed the most files to a
// directory, breaking ties alphabetically so the choice is a function of the file
// set rather than of walk order.
func dominantModuleLanguage(byLang map[string]int) string {
	best, bestN := "csharp", -1
	for lang, n := range byLang {
		if n > bestN || (n == bestN && lang < best) {
			best, bestN = lang, n
		}
	}
	return best
}

// langPass is one non-C# source language's per-file pass.
//
// scan reports whether the file was READ, distinct from whether it produced
// facts: a generated VB file is skipped and must not mark its directory as a
// module, while a `_Imports.razor` is read, produces nothing, and still belongs
// to one.
type langPass struct {
	lang  string
	files []string
	scan  func(relFile string, src []byte) ([]facts.Fact, bool)
}

// runLangPass parses one language's files in parallel and returns their facts
// plus the number actually read. MapFiles preserves input order, so the fact set
// stays a function of the file list rather than of scheduling.
func runLangPass(ctx context.Context, repoPath string, p langPass, note func(rel, lang string)) ([]facts.Fact, int) {
	type result struct {
		facts []facts.Fact
		read  bool
	}
	results := parallel.MapFiles(ctx, p.files, func(relFile string) result {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[dotnet-extractor] error reading %s: %v", relFile, err)
			return result{}
		}
		ff, read := p.scan(relFile, src)
		return result{facts: ff, read: read}
	})

	var out []facts.Fact
	n := 0
	for i, r := range results {
		if !r.read {
			continue
		}
		n++
		out = append(out, r.facts...)
		note(p.files[i], p.lang)
	}
	return out, n
}

// fileResult holds one file's extracted facts plus its ASP.NET routing evidence,
// returned together from the parallel per-file walk. `generated` distinguishes a
// file that was deliberately skipped from one that merely produced nothing, so the
// skip count stays honest.
type fileResult struct {
	facts     []facts.Fact
	scaffold  aspnetScaffold
	generated bool
}

// computeCSharpPerformsIO propagates the direct-I/O signal (io_direct, set by the
// walker on members that call a network/file/database primitive) transitively over
// the intra-repo call graph into a performs_io prop, so a member reaching I/O only
// through wrapper layers is still flagged and a per-iteration call to it reads as
// an N+1. Cycle-safe monotone fixpoint; mirrors computeRustPerformsIO.
func computeCSharpPerformsIO(allFacts []facts.Fact) {
	exists := make(map[string]bool)
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			exists[allFacts[i].Name] = true
		}
	}

	io := make(map[string]bool)
	adj := make(map[string][]string)
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if b, _ := f.PropAny("io_direct").(bool); b {
			io[f.Name] = true
		}
		seen := make(map[string]bool)
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls || r.Target == f.Name || seen[r.Target] || !exists[r.Target] {
				continue
			}
			seen[r.Target] = true
			adj[f.Name] = append(adj[f.Name], r.Target)
		}
	}

	for changed := true; changed; {
		changed = false
		for name, callees := range adj {
			if io[name] {
				continue
			}
			for _, c := range callees {
				if io[c] {
					io[name] = true
					changed = true
					break
				}
			}
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol && io[f.Name] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp("performs_io", true)
		}
	}
}

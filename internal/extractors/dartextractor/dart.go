// Package dartextractor extracts architectural facts from Dart and Flutter source
// code using tree-sitter AST parsing.
//
// The walker lives in dart_ast.go, the pub-package model in pubspec.go, and the
// post-extraction resolution passes in resolve.go. The Flutter-specific surfaces —
// widgets, navigation routes, HTTP clients and storage — live in their own files and
// are all gated the same way: on what the FILE ITSELF IMPORTS.
//
// # Why import gating rather than structural matching
//
// The Scala extractor learned this the expensive way: its DSL names are ordinary method
// names, so matching `path(...)` structurally turned a metrics timer into four routes
// and a forum's `closeTopic` into seven phantom Kafka topics. Dart is exposed to the
// same class of error — `go` is a go_router call and also a perfectly ordinary method
// name, `get` is an HTTP verb and a map accessor — but it has a defence Scala does not:
// Dart imports are EXPLICIT AND MANDATORY. There is no ambient namespace, no wildcard
// import of a package's symbols without naming the package. A file that has not written
// `import 'package:go_router/go_router.dart'` cannot be calling go_router, as a matter
// of language rule rather than of probability.
//
// So every framework pass here keys on the importing file's own URIs. That is a
// stronger gate than the Scala one (which had to key on the file importing the
// framework and still accept the residual risk within such files), and it is why this
// extractor can match on short, common method names at all.
package dartextractor

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

// DartExtractor extracts architectural facts from Dart/Flutter source code.
type DartExtractor struct{}

// New creates a new DartExtractor.
func New() *DartExtractor { return &DartExtractor{} }

func (e *DartExtractor) Name() string { return "dart" }

// Detect reports whether the repository contains Dart.
//
// pubspec.yaml is the definitive marker — it is what makes a directory a pub package —
// and a bare .dart file is the fallback for the genuine but rarer case of Dart scripts
// or a package whose manifest lives outside the scanned depth. Unlike the Kotlin/Java
// split there is no ambiguity to guard against here: nothing but Dart uses either
// marker.
func (e *DartExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
//
// This is the extractor for which the engine's name set has to be the PRE-ignore
// one. A Dart repository is spelled by pubspec.yaml, and the bundled mcp-arch.yaml
// ignores **/*.yaml wholesale — so under the indexed list alone, pubspec never
// appears and a Flutter monorepo detects only by its .dart files. Both signals are
// kept; see plugin.FileListDetector for why the walk hands over ignored names.
func (e *DartExtractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if skipDetectPath(rel) {
			continue
		}
		name := detectnames.Base(rel)
		if name == "pubspec.yaml" || (strings.HasSuffix(name, ".dart") && !isGeneratedDart(rel)) {
			return true, nil
		}
	}
	return false, nil
}

// skipDetectPath applies skipDetectDir to every segment of a repo-relative name, so
// the directories the old walk refused to descend into are refused identically when
// the names arrive from the engine instead.
func skipDetectPath(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		if skipDetectDir(seg) {
			return true
		}
	}
	return false
}

// skipDetectDir keeps Detect out of dependency and build trees. A vendored .pub-cache
// or a Flutter build directory holds thousands of .dart files that belong to somebody
// else's package, and detecting on one would be detecting on a dependency.
func skipDetectDir(name string) bool {
	switch name {
	case ".git", ".dart_tool", ".pub-cache", "build", "node_modules", ".symlinks", "Pods":
		return true
	}
	return false
}

func isDartFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".dart")
}

// generatedSuffixes are the build_runner / protoc / codegen conventions.
//
// Excluding these is not a tidiness preference — it is the difference between a usable
// graph and an unusable one. Generated Dart is the MAJORITY of files in a real
// build_runner project: a single @freezed model yields a .freezed.dart of hundreds of
// lines of copyWith/== boilerplate plus a .g.dart of serialization, none of it code
// anybody navigates, all of it inflating symbol counts and manufacturing god-class and
// complexity findings about machine output. The C# extractor draws the same line for
// the same reason (.g.cs, .Designer.cs, <auto-generated>).
var generatedSuffixes = []string{
	".g.dart",       // build_runner: json_serializable, drift, retrofit, injectable
	".freezed.dart", // freezed: immutable data classes
	".mocks.dart",   // mockito
	".gr.dart",      // auto_route
	".config.dart",  // injectable
	".gform.dart",   // reactive_forms
	".pb.dart",      // protoc-gen-dart
	".pbenum.dart",
	".pbjson.dart",
	".pbserver.dart",
	".pbgrpc.dart",
}

// isGeneratedDart reports whether a path is code generation output.
func isGeneratedDart(relFile string) bool {
	base := strings.ToLower(filepath.Base(relFile))
	for _, suf := range generatedSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// OwnsFile implements plugin.FileOwner for incremental caching.
//
// pubspec.yaml is owned as well as .dart: it names the package, decides which files are
// attributed to which compilation unit, and resolves every `package:` import, so a
// change to one changes this extractor's output for files it did not touch.
func (e *DartExtractor) OwnsFile(relFile string) bool {
	if filepath.Base(relFile) == "pubspec.yaml" || strings.HasSuffix(filepath.ToSlash(relFile), "lib/libraries.json") {
		return true
	}
	return isDartFile(relFile) && !isGeneratedDart(relFile)
}

// fileResult is one file's contribution, kept separate from the merged fact list so the
// post-passes below can see the observations a single file could not resolve alone.
type fileResult struct {
	facts []facts.Fact
	// partOf is the library file this file is a `part of`, if any (repo-relative).
	partOf string
	// parts are the files this file declares as its `part`s (repo-relative).
	parts []string
	// typeNames are the type names this file declares, feeding the project-wide index
	// that resolves bare references.
	typeNames []string
	// importURIs is what the file imports, kept so a `part` can inherit its host
	// library's imports (see reextractParts).
	importURIs []string
	// stringConsts maps `<Type>.<CONST>` to its literal, merged repo-wide so a route
	// declared as `path: HomeScreen.routeName` resolves across files.
	stringConsts map[string]string
}

// reextractParts re-walks `part` files with their host library's imports.
//
// A Dart `part` declares no directives of its own — it shares the host library's import
// scope, which is the whole point of the construct. Every framework pass in this
// extractor is gated on the importing file's URIs, so a part file walked in isolation
// looks like a file that imports nothing and every gate closes: its go_router calls,
// its http calls and its drift tables all go unseen.
//
// Rather than leave that as a documented hole, the affected files are simply walked
// again with the host's imports in hand. The cost is bounded and small — only files
// carrying a `part of` are re-parsed, and the dominant source of parts (build_runner
// output) is excluded from the file list before this point.
func reextractParts(results []fileResult, dartFiles []string, pkgs *packageIndex, read func(string) []byte) []fileResult {
	hostImports := make(map[string][]string, len(results))
	partHosts := map[string]string{}
	for i, r := range results {
		if len(r.parts) > 0 {
			host := filepath.ToSlash(dartFiles[i])
			hostImports[host] = r.importURIs
			for _, part := range r.parts {
				if prior, ok := partHosts[part]; ok && prior != host {
					partHosts[part] = ""
				} else if !ok {
					partHosts[part] = host
				}
			}
		}
	}
	if len(hostImports) == 0 {
		return results
	}
	for i, r := range results {
		host := r.partOf
		if host == "" {
			host = partHosts[filepath.ToSlash(dartFiles[i])]
		}
		if host == "" {
			continue
		}
		imports, ok := hostImports[filepath.ToSlash(host)]
		if !ok || len(imports) == 0 {
			continue
		}
		results[i] = extractFile(read(dartFiles[i]), dartFiles[i], pkgs, imports)
	}
	return results
}

// Extract parses Dart files and emits architectural facts.
//
// Three phases, and the split is forced by the language rather than chosen:
//
//  1. Every pubspec.yaml is read first (a line scan, no parse) to build the package
//     index. Import resolution needs it, and a `package:` URI can name a package
//     declared anywhere in the repo, so it cannot be done per-file.
//  2. Files are walked in parallel. Each emits what it can see alone: symbols,
//     imports, and call edges as written.
//  3. Serial post-passes resolve what no single file could: part/part-of merging,
//     bare call targets against the project-wide type index, and the transitive
//     performs_io closure.
func (e *DartExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var dartFiles []string
	for _, relFile := range files {
		if isDartFile(relFile) && !isGeneratedDart(relFile) {
			dartFiles = append(dartFiles, relFile)
		}
	}
	if len(dartFiles) == 0 {
		return nil, nil
	}

	// Read straight from disk: `**/*.yaml` is ignore-globbed, so no pubspec.yaml is
	// ever in `files`. See scanPubspecs for why this bypass is correct rather than a
	// workaround.
	pkgs := buildPackageIndex(repoPath, scanPubspecs(repoPath))

	read := func(relFile string) []byte {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[dart-extractor] error reading %s: %v", relFile, err)
			return nil
		}
		return src
	}

	results := parallel.MapFiles(ctx, dartFiles, func(relFile string) fileResult {
		return extractFile(read(relFile), relFile, pkgs, nil)
	})
	results = reextractParts(results, dartFiles, pkgs, read)

	var allFacts []facts.Fact
	stringConsts := map[string]string{}
	for _, r := range results {
		allFacts = append(allFacts, r.facts...)
		for k, v := range r.stringConsts {
			// First writer wins, so the merged map does not depend on which worker
			// finished first. parallel.MapFiles preserves input order, so iterating
			// results here is deterministic.
			if _, seen := stringConsts[k]; !seen {
				stringConsts[k] = v
			}
		}
	}

	// Post-passes, in order: each consumes the previous one's output.
	allFacts = attributeSymbolsToModules(allFacts)
	allFacts = resolveRoutePathRefs(allFacts, stringConsts)
	allFacts = resolveCallTargets(allFacts)
	computeDartPerformsIO(allFacts)

	allFacts = append(allFacts, moduleFacts(dartFiles, pkgs)...)
	return allFacts, nil
}

// moduleFacts emits one module per directory holding Dart, carrying the pub package it
// compiles into.
//
// The `pub_package` prop is registered in facts.CompilationUnitProps, and that
// registration is what makes the cycles explainer word a Dart cycle correctly. Dart is
// the most permissive language in the corpus on this point: circular imports between
// libraries are LEGAL and compile — the analyzer does not object, and neither does the
// VM — because the compilation unit is the package, not the library. A cycle enola
// finds inside one package is therefore never a build-order defect, and claiming it
// "can cause initialization issues" would be untrue in exactly the way the explainer's
// build-unit table exists to prevent.
func moduleFacts(dartFiles []string, pkgs *packageIndex) []facts.Fact {
	dirs := make(map[string]string, len(dartFiles))
	for _, relFile := range dartFiles {
		dir := factpath.Dir(relFile)
		if _, seen := dirs[dir]; seen {
			continue
		}
		name := ""
		if p := pkgs.ownerOf(relFile); p != nil {
			name = p.Name
		}
		dirs[dir] = name
	}
	// Sorted, not map order. facts.jsonl is hashed into the snapshot ID, and while the
	// store sorts before writing, an extractor that emits in Go's randomized map order
	// is one refactor away from making an unchanged tree produce a different snapshot
	// on every run — the failure ARCHITECTURE.md documents for the insight statistics.
	names := make([]string, 0, len(dirs))
	for dir := range dirs {
		names = append(names, dir)
	}
	sort.Strings(names)

	out := make([]facts.Fact, 0, len(names))
	for _, dir := range names {
		props := map[string]any{"language": "dart"}
		if pkg := dirs[dir]; pkg != "" {
			props["pub_package"] = pkg
		}
		out = append(out, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		})
	}
	return out
}

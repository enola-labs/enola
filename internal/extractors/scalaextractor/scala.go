// Package scalaextractor extracts architectural facts from Scala source code
// using tree-sitter AST parsing (see scala_ast.go for the walker implementation).
//
// Scala is the fourth JVM language enola reads, and it shares the source layout
// and package semantics of the other three: it resolves imports through the
// cross-language package index in internal/extractors/jvmsrc, and it names symbols
// with the same "<dir>.<Type>" / "<dir>.<Type>.<member>" convention. What it does
// NOT share is the assumption that one language owns a repository — apache/spark
// holds 1,355 .java files beside 6,275 .scala, and apache/pekko 582 — so the Java
// and Scala extractors routinely run over the same tree and must resolve into each
// other's packages rather than past them.
package scalaextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/extractors/jvmsrc"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// ScalaExtractor extracts architectural facts from Scala source code.
type ScalaExtractor struct{}

// New creates a new ScalaExtractor.
func New() *ScalaExtractor {
	return &ScalaExtractor{}
}

func (e *ScalaExtractor) Name() string {
	return "scala"
}

// buildMarkers are files whose presence pins the repository as Scala regardless of
// where the sources live. `project/build.properties` earns its place empirically:
// apache/spark, the largest Scala repository in the benchmark corpus, has NO root
// build.sbt — it builds with Maven and keeps only sbt's launcher pin — so detecting
// on build.sbt alone would miss it entirely.
var buildMarkers = []string{
	"build.sbt",                // sbt, the common case
	"project/build.properties", // sbt launcher pin; present when build.sbt is not
	"build.sc",                 // Mill (legacy)
	"build.mill",               // Mill (current)
	"build.mill.scala",
}

// Detect returns true if the repository looks like a Scala project.
//
// Detection deliberately does NOT stop at "some other JVM extractor already claimed
// this repo". Scala coexists with Java and Kotlin in the same tree far more often
// than it owns one alone, and each extractor reads only the files it owns, so
// several claiming the same repository is the correct outcome rather than a
// conflict — the opposite of the Java/Kotlin split, where a shared Gradle file
// could not distinguish the two.
func (e *ScalaExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// detectByBuild answers from the build definition alone: the markers, a root
// *.sbt, or a Maven/Gradle file that names Scala. Those files are equally used by
// Java and Kotlin, so the file alone proves nothing and the coordinate is what
// names the language. Kept separate from the source fallback because it is the
// half that must stay conservative.
func (e *ScalaExtractor) detectByBuild(repoPath string) (bool, error) {

	for _, m := range buildMarkers {
		if _, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(m))); err == nil {
			return true, nil
		}
	}
	// A root *.sbt under any name (build.sbt is conventional, not required).
	if entries, err := os.ReadDir(repoPath); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sbt") {
				return true, nil
			}
		}
	}
	// Maven and Gradle builds that declare Scala. Both files are equally used by
	// Java and Kotlin projects, so the file alone proves nothing — the plugin or
	// library coordinate is what names the language.
	for _, name := range []string{"pom.xml", "build.gradle", "build.gradle.kts"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		content := string(data)
		if strings.Contains(content, "scala-maven-plugin") ||
			strings.Contains(content, "scala-library") ||
			strings.Contains(content, "org.scala-lang") ||
			strings.Contains(content, "scala-compiler") ||
			strings.Contains(content, "scalaVersion") {
			return true, nil
		}
	}
	return false, nil
}

// DetectFiles implements plugin.FileListDetector. The build-marker and coordinate
// checks above are unchanged — they are what tell Scala apart from Java and Kotlin
// under a shared Gradle or Maven build — and only the source fallback loses its
// eight-level bound.
func (e *ScalaExtractor) DetectFiles(repoPath string, files []string) (bool, error) {
	if ok, err := e.detectByBuild(repoPath); ok || err != nil {
		return ok, err
	}
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, "build", "target", "out") {
			continue
		}
		if isScalaFile(detectnames.Base(rel)) {
			return true, nil
		}
	}
	return false, nil
}

// isScalaFile reports whether the path is a Scala source. `.sc` covers Mill build
// files and Ammonite scripts, which are Scala and carry real declarations.
func isScalaFile(path string) bool {
	l := strings.ToLower(path)
	return strings.HasSuffix(l, ".scala") || strings.HasSuffix(l, ".sc")
}

// OwnsFile implements plugin.FileOwner for incremental caching.
func (e *ScalaExtractor) OwnsFile(relFile string) bool { return isScalaFile(relFile) }

// AffectsKey implements plugin.KeyDependent: a .java or .kt file's package
// declaration feeds the cross-language package index this extractor resolves
// imports through, so a change to either must invalidate the Scala cache. The
// reverse direction matters just as much and is declared by those extractors.
func (e *ScalaExtractor) AffectsKey(relFile string) bool {
	l := strings.ToLower(relFile)
	return strings.HasSuffix(l, ".java") || strings.HasSuffix(l, ".kt")
}

// Extract parses Scala files and emits architectural facts.
//
// Two passes. Pass 1 walks each file's AST in parallel (extractFileAST), emitting
// declaration symbols, import dependencies and the type-reference edges a single
// file can see. Pass 2 (canonicalizeTargets) rewrites those edge targets from the
// names as written — a bare `Base`, a fully qualified `com.example.core.Base` —
// to the canonical "<dir>.<Type>" fact names, using a project-wide index built
// from pass 1. Without it, `extends Base` in one file and the declaration of Base
// in another are two unrelated strings and the graph has no edge between them.
func (e *ScalaExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var scalaFiles []string
	for _, relFile := range files {
		if isScalaFile(relFile) {
			scalaFiles = append(scalaFiles, relFile)
		}
	}

	// The cross-language package index spans .scala, .java and .kt, so a Scala
	// import of a Java type in a sibling Maven module resolves to the directory
	// that declares it instead of being written off as an external dependency.
	packageIndex := jvmsrc.BuildPackageIndex(repoPath, files)

	// extractFileFacts is a pure function of (src, relFile); parse in parallel and
	// merge in file order so the output is deterministic regardless of scheduling.
	// It parses each file ONCE and runs both the declaration walk and the DSL
	// route/storage walks over that one tree.
	perFileFacts := parallel.MapFiles(ctx, scalaFiles, func(relFile string) fileResult {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[scala-extractor] error reading %s: %v", relFile, err)
			return fileResult{}
		}
		ff, pkg := extractFileFacts(src, relFile, packageIndex)
		return fileResult{facts: ff, pkg: pkg}
	})

	var allFacts []facts.Fact
	modules := make(map[string]bool, len(scalaFiles))
	filePkg := make(map[string]string, len(scalaFiles))
	for i, r := range perFileFacts {
		allFacts = append(allFacts, r.facts...)
		modules[factpath.Dir(scalaFiles[i])] = true
		if r.pkg != "" {
			filePkg[scalaFiles[i]] = r.pkg
		}
	}

	// Play declares its whole HTTP surface in conf/routes, a DSL of its own that no
	// glob admits and the walker cannot reach — read from disk like the OpenAPI and
	// Symfony route configs.
	allFacts = append(allFacts, extractPlayRoutes(repoPath)...)

	canonicalizeTargets(allFacts, packageIndex, filePkg)
	// After canonicalization, so the closure walks edges that point at real facts:
	// run before it and every cross-file call target would still be a bare name and
	// the propagation would stop at the first hop.
	computeScalaPerformsIO(allFacts)

	for dir := range modules {
		props := map[string]any{
			"language":           "scala",
			facts.PropModuleRole: jvmsrc.ModuleRole(dir),
		}
		// The build module this directory compiles into. The cycles explainer reads
		// it to tell a build-order defect from a cycle that is merely internal to one
		// module: sbt and Maven both reject a circular dependency between modules,
		// while Scala imposes no package-level acyclicity WITHIN one, so a cycle
		// found inside a module is legal and ordinary — and reporting it as
		// something that "can cause initialization issues" is simply untrue.
		if unit := jvmsrc.BuildModule(dir); unit != "" {
			props["jvm_module"] = unit
		}
		allFacts = append(allFacts, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		})
	}

	return allFacts, nil
}

// fileResult carries one file's facts together with the package it declares. The
// package travels alongside rather than inside the facts because it is a property
// of the FILE, not of any one declaration, and canonicalizeTargets needs it to
// resolve bare type references after the merge.
type fileResult struct {
	facts []facts.Fact
	pkg   string
}

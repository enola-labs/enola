// Package jvmsrc holds source-layout helpers shared by the JVM-language
// extractors (Kotlin, Java). Its job is to resolve a declared package name to the
// directory that actually holds it, so cross-module imports resolve correctly in
// multi-module Gradle/Maven projects where several modules root their packages at
// the same prefix (e.g. app/, api/, business/ all declaring de.foo.*).
package jvmsrc

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linescan"
)

// ModuleRole classifies a JVM module directory as production or test/tooling for
// the module_role fact prop. It defers to facts.ModuleRoleForPath (which knows
// the Gradle src/test and src/androidTest layouts) and, since every JVM module
// directory holds real source, treats an otherwise-unknown directory as
// production so downstream analyses include it explicitly.
//
// Under a `src/main` source set, only the segments BEFORE it are classified. That
// boundary is the whole rule, and each side of it means something different: the
// module name ahead of `src/` states the module's purpose, so a Gradle module called
// `ui-test-utils` is test tooling however it compiles; the path after `src/main` is
// a PACKAGE, and a package named `test` says nothing about the role. Spark ships
// `sql/core/src/main/scala/org/apache/spark/sql/test/SQLTestUtils.scala` as
// production code, and classifying it as test would drop it from package metrics and
// layer analysis on the strength of a package name. Without the split, the segment
// scan sees `test` on both sides and cannot tell them apart. Applies to all four JVM
// languages; the ignore globs draw the same boundary for the same reason (see
// facts.matchDirScopedGlob).
func ModuleRole(dir string) string {
	if isMetaBuildDir(dir) {
		return facts.ModuleRoleTooling
	}
	role := facts.ModuleRoleForPath(moduleRoleScope(dir))
	if role == facts.ModuleRoleUnknown {
		return facts.ModuleRoleProduction
	}
	return role
}

// isMetaBuildDir reports whether a directory holds BUILD DEFINITIONS rather than
// application code. sbt compiles `project/` as a second, meta build and Gradle does
// the same with `buildSrc/`; both are real source in the language, so an extractor
// reads them like anything else and they land in the graph as production packages
// unless told otherwise. That is the same category `scripts/` already occupies, and
// getting it wrong feeds build plumbing into package metrics and dead-code review.
//
// Anchored at the FIRST path segment, which is where both tools require it. A
// package named `project` further down a source tree is application code and is
// unaffected.
func isMetaBuildDir(dir string) bool {
	first := filepath.ToSlash(dir)
	if i := strings.Index(first, "/"); i >= 0 {
		first = first[:i]
	}
	return first == "project" || first == "buildSrc"
}

// BuildModule returns the build module a source directory compiles into — the sbt
// project, Maven module or Gradle subproject — or "" when the directory sits outside
// any source set.
//
// It is derived from the layout rather than from the build file, which is what makes
// it cheap and build-tool agnostic: sbt, Mill, Maven and Gradle all put a module's
// sources under `<module>/src/<sourceSet>/`, so the path prefix before the first
// `src/` segment names the module. A repository whose sources start at `src/`
// directly is a single-module build, and returns "." — a NAME rather than the empty
// string, because callers read "" as "not attributed", and a single-module
// repository is the case where attribution matters most.
//
// The cycles explainer is the consumer (see facts.CompilationUnitProps). A cycle
// between packages inside ONE module is legal and ordinary in Scala — the compiler
// takes the module as a unit and imposes no package-level acyclicity, unlike Go —
// while sbt and Maven both reject a circular dependency BETWEEN modules. So the
// distinction this draws is exactly the one that decides whether a cycle is a
// build-order defect or a coupling signal.
func BuildModule(dir string) string {
	d := strings.Trim(filepath.ToSlash(dir), "/")
	if d == "" {
		return ""
	}
	segs := strings.Split(d, "/")
	for i, seg := range segs {
		if seg != "src" {
			continue
		}
		if i == 0 {
			return "." // sources begin at the repository root: one module
		}
		return strings.Join(segs[:i], "/")
	}
	return "" // outside any source set — build definitions, scripts, stray files
}

// moduleRoleScope returns the part of dir that carries role information: everything
// before a `src/main` source set, or the whole path when there is none (so a
// `src/test` or `src/androidTest` set is still classified by the segment scan).
func moduleRoleScope(dir string) string {
	d := filepath.ToSlash(dir)
	if i := strings.Index(d, "/src/main/"); i >= 0 {
		return d[:i]
	}
	if strings.HasPrefix(d, "src/main/") || d == "src/main" {
		return ""
	}
	if i := strings.Index(d, "/src/main"); i >= 0 && strings.HasSuffix(d, "/src/main") {
		return d[:i]
	}
	return d
}

// packageRe extracts a JVM source file's package declaration. It matches Kotlin
// and Scala (`package de.foo.bar`) as well as Java (`package de.foo.bar;`) — the
// trailing `;` and any comment are excluded by the `[\w.]+` capture.
var packageRe = regexp.MustCompile(`^\s*package\s+([\w.]+)`)

// jvmSourceExts are the file extensions BuildPackageIndex reads. All four JVM
// languages enola supports share one package namespace, and a repository mixing
// them is the norm rather than the exception — apache/spark holds 1,355 .java
// beside 6,275 .scala — so an index that read only one of them would report a
// sibling module's types as an external dependency.
func isJVMSource(path string) bool {
	p := strings.ToLower(path)
	return strings.HasSuffix(p, ".kt") || strings.HasSuffix(p, ".java") ||
		strings.HasSuffix(p, ".scala") || strings.HasSuffix(p, ".sc")
}

// isTestSource reports whether a file lives in a Gradle/Maven test source set
// (src/test, src/androidTest). Test sources are kept OUT of the package index so
// an import never resolves into a test directory — package-metrics and other
// analyses exclude test modules, and a main package can share its name with a
// test helper in the same package. A package's test directories rank below its
// others in BuildPackageDirs.
//
// A Kotlin Multiplatform test source set is named for its target (src/jvmTest,
// src/commonTest, src/androidUnitTest) and is a test set the same way.
func isTestSource(relFile string) bool {
	p := "/" + filepath.ToSlash(relFile)
	for {
		i := strings.Index(p, "/src/")
		if i < 0 {
			return false
		}
		p = p[i+len("/src/"):]
		set, _, _ := strings.Cut(p, "/")
		if set == "test" || strings.HasSuffix(set, "Test") {
			return true
		}
		p = "/" + p
	}
}

// BuildPackageIndex maps a declared JVM package FQN (e.g. "com.example.app.api")
// to the on-disk directory that declares it, scanning every .kt/.java/.scala file
// in files. It is the reliable, multi-module replacement for assuming a
// single global source root: an import of com.example.app.api.model.search.Foo
// resolves to whichever module's directory actually contains that package, not
// to whichever module happens to hold the most files.
//
// When two directories declare the same package, the main source set wins over
// Gradle build-variant source sets (src/debug, src/release, src/staging, flavors):
// an Android app's src/main and src/debug both declare the root application
// package, and imports of it must resolve to the primary (main) module, not a
// variant. Among equally-primary candidates the lexicographically smallest
// directory wins so the result is deterministic across runs.
func BuildPackageIndex(repoPath string, files []string) map[string]string {
	return IndexOf(BuildPackageDirs(repoPath, files))
}

// BuildPackageDirs maps a declared JVM package FQN to every directory that
// declares it, the one BuildPackageIndex resolves to first and test source sets
// last. A package is split across directories in a multi-module build (a library
// and its testing support both declaring com.example.internal), in a Kotlin
// Multiplatform module (src/commonMain and src/jvmMain), and between a module's
// main and test sets, and only one of them declares a given name. See
// SplitSiblings.
func BuildPackageDirs(repoPath string, files []string) map[string][]string {
	byPkg := make(map[string][]string)
	seen := make(map[string]bool)
	for _, relFile := range files {
		if !isJVMSource(relFile) {
			continue
		}
		pkg, ok := readPackage(filepath.Join(repoPath, relFile))
		if !ok {
			continue
		}
		dir := factpath.Dir(relFile)
		if seen[pkg+"\x00"+dir] {
			continue
		}
		seen[pkg+"\x00"+dir] = true
		byPkg[pkg] = append(byPkg[pkg], dir)
	}
	for _, dirs := range byPkg {
		sort.Slice(dirs, func(i, j int) bool { return preferDir(dirs[i], dirs[j]) })
	}
	return byPkg
}

// IndexOf is the package index BuildPackageDirs implies: each package to the
// directory it prefers. A package only test sources declare is left out; see
// TestOnlyIndex.
func IndexOf(dirs map[string][]string) map[string]string {
	index := make(map[string]string, len(dirs))
	for pkg, ds := range dirs {
		if !isTestSource(ds[0]) {
			index[pkg] = ds[0]
		}
	}
	return index
}

// TestOnlyIndex maps each package only test sources declare to the directory it
// prefers, for resolving an import made by test code: a test helper in a package
// of its own is reachable from the tests that import it and from nothing else.
func TestOnlyIndex(dirs map[string][]string) map[string]string {
	index := make(map[string]string)
	for pkg, ds := range dirs {
		if isTestSource(ds[0]) {
			index[pkg] = ds[0]
		}
	}
	return index
}

// IsTestSource reports whether a file lives in a test source set. See isTestSource.
func IsTestSource(relFile string) bool { return isTestSource(relFile) }

// SplitSiblings maps each directory of a package declared in more than one to the
// others, for SplitTarget.
func SplitSiblings(dirs map[string][]string) map[string][]string {
	out := make(map[string][]string)
	for _, ds := range dirs {
		if len(ds) < 2 {
			continue
		}
		for _, d := range ds {
			for _, other := range ds {
				if other != d {
					out[d] = append(out[d], other)
				}
			}
		}
	}
	return out
}

// SplitTarget re-homes a fact name "<dir>.<Name>…" that resolved into the wrong
// directory of a split package: when no fact of that name exists and exactly one
// sibling directory declares <Name>, the name in that directory. An import is
// resolved per package, before any file has been read, so it can only name the
// package's preferred directory. It returns target unchanged otherwise.
func SplitTarget(target string, siblings map[string][]string, exists func(string) bool) string {
	if len(siblings) == 0 || exists(target) {
		return target
	}
	slash := strings.LastIndexByte(target, '/')
	dot := strings.IndexByte(target[slash+1:], '.')
	if dot < 0 {
		return target
	}
	dir, rest := target[:slash+1+dot], target[slash+1+dot:]
	found := ""
	for _, other := range siblings[dir] {
		if exists(other + rest) {
			if found != "" {
				return target // declared in two: no way to tell which
			}
			found = other + rest
		}
	}
	if found == "" {
		return target
	}
	return found
}

// preferDir reports whether candidate should replace existing as the directory
// for a package declared in more than one place. A main source set (…/src/main/…)
// is preferred over a build-variant source set; otherwise the lexicographically
// smaller path wins (deterministic).
func preferDir(candidate, existing string) bool {
	if cTest, eTest := isTestSource(candidate), isTestSource(existing); cTest != eTest {
		return eTest // a test source set is never the package's directory
	}
	cMain, eMain := isMainSourceSet(candidate), isMainSourceSet(existing)
	if cMain != eMain {
		return cMain // promote a main source set over a variant one
	}
	return candidate < existing
}

// isMainSourceSet reports whether a directory lives in a Gradle main source set
// (src/main/...), as opposed to a build-variant source set like src/debug.
func isMainSourceSet(dir string) bool {
	return strings.Contains(dir, "/src/main/") || strings.HasPrefix(dir, "src/main/")
}

// readPackage returns the package declared in a source file, reading only the
// header (declarations always precede any type). ok is false when the file cannot
// be read or declares no package (the default/root package).
//
// Two details are Scala's, and both are wrong answers rather than missing ones if
// skipped. Scala permits CHAINED clauses — `package com.example` followed by
// `package model` names `com.example.model`, not `com.example` — so consecutive
// clauses are joined; Java and Kotlin allow only one, so they are unaffected.
// And `package object util` is a package OBJECT, a declaration whose name is
// `util`, not a clause opening a package called `object`: matching it would index
// a package by that name and make every import resolve against a package that does
// not exist.
func readPackage(absFile string) (string, bool) {
	f, err := os.Open(absFile)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	var parts []string
	scanner := linescan.New(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || isCommentLine(line) {
			continue
		}
		// `package object util` declares a MEMBER of the enclosing package rather
		// than opening one called `object`, so it is not a clause.
		m := packageRe.FindStringSubmatch(line)
		if m == nil || m[1] == "object" {
			if len(parts) > 0 {
				break // the chain ended at the first real declaration
			}
			continue // still in the preamble, keep looking
		}
		parts = append(parts, m[1])
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "."), true
}

// isCommentLine reports whether a trimmed line opens or continues a comment. It is
// deliberately shallow — enough to step over a licence header or a scaladoc block
// between chained package clauses, not a comment parser.
func isCommentLine(line string) bool {
	return strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") ||
		strings.HasPrefix(line, "*")
}

// generatedClasses are the classes the Android Gradle plugin writes into a module's
// namespace at build time. No source file declares them.
var generatedClasses = map[string]bool{"R": true, "BuildConfig": true}

// GeneratedImport reports whether a JVM import names code the build generates
// rather than a source file the repository holds, so it must not become an edge to
// any of its directories. Two shapes:
//
//   - A generated class: `R` or `BuildConfig` (and R's nested `R.string`, …)
//     directly under a package. They live in the Gradle module's NAMESPACE, which
//     often no source file declares as a package (`com.example.app.ui.R` in a
//     module whose code is all under `com.example.app.ui.home`).
//   - An undeclared sub-package of a declared one, named by two or more further
//     segments of which the first is lowercase: `com.example.app.databinding.
//     ActivityMainBinding`, which View Binding generates.
//
// Both used to resolve to the longest declared prefix. For a namespace no file
// declares, that is usually the app's root package, so every `import ….ui.R` in a
// feature module became an edge into the application module — edges Gradle forbids
// between modules, which fused whole apps into one dependency cycle. A nested type
// (`com.example.Outer.Inner`, uppercase) and a top-level function
// (`com.example.util.someFunction`, one segment) still resolve.
func GeneratedImport(importPath string, index map[string]string) bool {
	segs := strings.Split(strings.TrimSuffix(importPath, ".*"), ".")
	for i, seg := range segs {
		if generatedClasses[seg] && i > 0 {
			return true
		}
		if seg == "" || seg[0] < 'a' || seg[0] > 'z' {
			break // past the package part: a type name, not a package segment
		}
	}
	// The longest declared prefix decides, the whole path included: a wildcard
	// import of a declared package (`….ui.home.*`) names that package, not an
	// undeclared one below a shorter prefix.
	for i := len(segs); i > 0; i-- {
		if _, ok := index[strings.Join(segs[:i], ".")]; !ok {
			continue
		}
		rest := segs[i:]
		return len(rest) >= 2 && rest[0] != "" && rest[0][0] >= 'a' && rest[0][0] <= 'z'
	}
	return false
}

// ResolveImport maps a JVM import FQN to the directory of the package that holds
// it, using a package index from BuildPackageIndex. It walks from the longest
// dotted prefix down to the shortest, so that `de.foo.bar.Baz` resolves via the
// package `de.foo.bar` and a nested-type import `de.foo.bar.Outer.Inner` still
// resolves once a known package prefix is found. Any remaining segments (the
// type / nested-type name) are appended as path components so the caller's
// module-resolution still lands on the package directory and the produced fact
// name matches the "<dir>.<Type>" symbol convention. Returns ok=false when no
// prefix is a known package (i.e. an external dependency), and for an import of
// generated code (see GeneratedImport).
func ResolveImport(importPath string, index map[string]string) (string, bool) {
	if GeneratedImport(importPath, index) {
		return "", false
	}
	p := strings.TrimSuffix(importPath, ".*")
	segs := strings.Split(p, ".")
	for i := len(segs); i > 0; i-- {
		pkg := strings.Join(segs[:i], ".")
		dir, ok := index[pkg]
		if !ok {
			continue
		}
		if rest := segs[i:]; len(rest) > 0 {
			return dir + "/" + strings.Join(rest, "/"), true
		}
		return dir, true
	}
	return "", false
}

package javaextractor

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

// JavaExtractor extracts architectural facts from Java source code using
// tree-sitter AST parsing (see java_ast.go for the walker and spring.go for
// Spring/JPA/Dubbo framework specialization).
type JavaExtractor struct{}

// New creates a new JavaExtractor.
func New() *JavaExtractor {
	return &JavaExtractor{}
}

func (e *JavaExtractor) Name() string {
	return "java"
}

// Detect returns true if the repository looks like a Java project: a Maven project
// (pom.xml), or any actual .java source file. A Gradle build file alone is not
// sufficient — Gradle is equally used by Kotlin, Android, and Groovy projects, so
// detecting on it would wrongly claim pure-Kotlin repos. Requiring real .java
// sources keeps the Java extractor off non-Java JVM projects.
func (e *JavaExtractor) Detect(repoPath string) (bool, error) {
	if _, err := os.Stat(filepath.Join(repoPath, "pom.xml")); err == nil {
		return true, nil
	}
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
//
// The bound this replaces was eight levels, which reads as generous until a real
// repository is measured against it: flutterfire's Java lives at
// packages/<pkg>/<pkg>/android/src/main/java/io/flutter/plugins/... — depth 10 — so
// all 66 of its Java files, and 509 of flutter-packages', were invisible. That is
// the argument against raising a bound rather than deleting it.
func (e *JavaExtractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, "build", "target") {
			continue
		}
		if isJavaFile(detectnames.Base(rel)) {
			return true, nil
		}
	}
	return false, nil
}

// Extract parses Java files and emits architectural facts.
//
// Two passes: pass 1 walks each file's AST (extractFileAST) to emit declaration,
// import, route, storage and call-graph facts while indexing every declared type by
// its fully-qualified name. Pass 2 (canonicalizeTargets) rewrites type-reference
// edge targets (implements/instantiates/injects) and import targets from FQNs to
// canonical "<dir>.<Type>" / module-dir names so reverse traversal connects
// dependents. Module facts are emitted per directory.
func (e *JavaExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	var javaFiles []string
	for _, relFile := range files {
		if isJavaFile(relFile) {
			javaFiles = append(javaFiles, relFile)
		}
	}

	// extractFileAST is a pure function of (src, relFile); parse files in parallel
	// and merge in file order for deterministic output.
	perFileFacts := parallel.MapFiles(ctx, javaFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[java-extractor] error reading %s: %v", relFile, err)
			return nil
		}
		return extractFileAST(src, relFile)
	})

	modules := make(map[string]bool)
	for i, ff := range perFileFacts {
		allFacts = append(allFacts, ff...)
		modules[factpath.Dir(javaFiles[i])] = true
	}

	// Cross-language package index (.kt AND .java) so a Java import of a Kotlin
	// type resolves to the module that declares it, instead of being dropped as
	// external. Java→Java imports still resolve via the in-facts FQN index below;
	// this only fills the cross-language gap.
	packageIndex := jvmsrc.BuildPackageIndex(repoPath, files)
	typeIndex := canonicalizeTargets(allFacts, packageIndex)
	resolveTableConstants(allFacts)

	// Fold Java/Dubbo SPI service-file registrations in as references so an impl
	// loaded by name (ExtensionLoader/ServiceLoader) — never called in code — is not
	// reported as dead code.
	allFacts = append(allFacts, extractSPIRefs(repoPath, files, typeIndex)...)

	for dir := range modules {
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language":           "java",
				facts.PropModuleRole: jvmsrc.ModuleRole(dir),
			},
		})
	}

	return allFacts, nil
}

// canonicalizeTargets resolves FQN-based edge targets to canonical fact names.
//
//   - implements/instantiates/injects targets that match a declared type's FQN are
//     rewritten to that type's "<dir>.<Type>" fact name; unresolved targets (external
//     libraries) are left as written.
//   - import dependency facts whose target FQN resolves to a declared type — or whose
//     value names a known source package — are marked source="internal" and pointed at
//     the owning module dir.
//
// canonicalizeTargets returns the FQN -> "<dir>.<Type>" index it builds, so callers
// (e.g. the SPI service-file fold) can resolve implementation FQNs to canonical
// fact names without rebuilding it.
func canonicalizeTargets(allFacts []facts.Fact, crossLangIndex map[string]string) map[string]string {
	typeIndex := make(map[string]string) // FQN -> "<dir>.<Type>" canonical name
	typeDir := make(map[string]string)   // FQN -> dir
	packageDir := make(map[string]string)
	// Seed with cross-language packages (e.g. Kotlin modules) so a Java import of
	// a package we didn't index from .java files still resolves. Java-declared
	// packages below take precedence (they overwrite these entries).
	for pkg, dir := range crossLangIndex {
		packageDir[pkg] = dir
	}
	for _, f := range allFacts {
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch f.PropAny("symbol_kind") {
		case facts.SymbolClass, facts.SymbolInterface, facts.SymbolEnum:
			fqn, _ := f.PropAny("fqn").(string)
			if fqn == "" {
				continue
			}
			dir := f.File
			if i := strings.LastIndex(dir, "/"); i >= 0 {
				dir = dir[:i]
			} else {
				dir = "."
			}
			typeIndex[fqn] = f.Name
			typeDir[fqn] = dir
			if pkg := parentName(fqn); pkg != "" {
				packageDir[pkg] = dir
			}
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindDependency {
			resolveImport(f, typeDir, packageDir)
			continue
		}
		for j := range f.Relations {
			r := &f.Relations[j]
			switch r.Kind {
			case facts.RelImplements, facts.RelInstantiates, facts.RelInjects:
				if canon, ok := typeIndex[r.Target]; ok {
					r.Target = canon
				}
			}
		}
	}
	return typeIndex
}

// spiServiceFile reports whether relFile is a Java/Dubbo SPI service-registration
// file — JDK ServiceLoader (META-INF/services/<iface-FQN>) or Dubbo SPI
// (META-INF/dubbo[/internal]/<iface-FQN>). Each lists implementation classes loaded
// by name, so their impls have no in-code caller.
func spiServiceFile(relFile string) bool {
	return strings.Contains(relFile, "META-INF/services/") ||
		strings.Contains(relFile, "META-INF/dubbo/")
}

// extractSPIRefs reads SPI service files and emits one KindFileRef fact per file,
// carrying a RelCalls edge to each registered implementation class that resolves to
// an in-repo type via typeIndex (FQN -> "<dir>.<Type>"). Line format is Dubbo's
// "key=FQN" or a bare "FQN" (JDK ServiceLoader); '#' comments and blank lines are
// skipped, and entries that do not resolve to an in-repo type (external classes) are
// dropped. The fact Name is the service-file path, which never equals a symbol name,
// so it introduces no self-reference — matching the KindFileRef contract the orphan
// detector folds in.
func extractSPIRefs(repoPath string, files []string, typeIndex map[string]string) []facts.Fact {
	var out []facts.Fact
	for _, relFile := range files {
		if !spiServiceFile(relFile) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		var rels []facts.Relation
		seen := map[string]bool{}
		for _, line := range strings.Split(string(src), "\n") {
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i] // strip whole-line or trailing comment
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if i := strings.IndexByte(line, '='); i >= 0 {
				line = strings.TrimSpace(line[i+1:]) // Dubbo "key=FQN" -> FQN
			}
			canon, ok := typeIndex[line]
			if !ok || seen[canon] {
				continue // external class, or already recorded
			}
			seen[canon] = true
			rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: canon})
		}
		if len(rels) == 0 {
			continue
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindFileRef,
			Name:      relFile,
			File:      relFile,
			Line:      1,
			Props:     map[string]any{"language": "java"},
			Relations: rels,
		})
	}
	return out
}

func resolveImport(f *facts.Fact, typeDir, packageDir map[string]string) {
	imp, _ := f.PropAny("import").(string)
	if imp == "" {
		return
	}
	var dir string
	var ok bool
	if dir, ok = typeDir[imp]; !ok {
		// Wildcard / package import (e.g. "com.example.foo").
		dir, ok = packageDir[imp]
	}
	if !ok {
		// Parent-FQN fallback for static-member imports
		// ("com.foo.Constants.MAX" -> declaring type "com.foo.Constants") and
		// imports of internal types we didn't index ("com.foo.Bar" -> package
		// "com.foo"). Skipped for wildcards, whose import string is already the
		// package — walking to the grandparent would mis-resolve. Only our own
		// types/packages are in the indices, so this never flags an external import.
		if wc, _ := f.PropAny("wildcard").(bool); !wc {
			if parent := parentName(imp); parent != "" {
				if dir, ok = typeDir[parent]; !ok {
					dir, ok = packageDir[parent]
				}
			}
		}
	}
	if !ok {
		return // external dependency
	}
	f.SetProp("source", "internal")
	for j := range f.Relations {
		if f.Relations[j].Kind == facts.RelImports {
			f.Relations[j].Target = dir
		}
	}
}

// resolveTableConstants rewrites storage facts whose "table" prop names a string
// constant (e.g. @Table(name = ADMIN_SETTINGS_TABLE_NAME)) to that constant's
// literal value. Constants are indexed by simple name across all files, since the
// table-name constants typically live in a shared ModelConstants class. When the
// same simple name maps to conflicting values it is left unresolved (ambiguous).
func resolveTableConstants(allFacts []facts.Fact) {
	values := make(map[string]string)
	ambiguous := make(map[string]bool)
	for _, f := range allFacts {
		if f.Kind != facts.KindSymbol {
			continue
		}
		v, ok := f.PropAny("value").(string)
		if !ok {
			continue
		}
		simple := f.Name
		if i := strings.LastIndex(simple, "."); i >= 0 {
			simple = simple[i+1:]
		}
		if existing, seen := values[simple]; seen && existing != v {
			ambiguous[simple] = true
			continue
		}
		values[simple] = v
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindStorage {
			continue
		}
		tbl, ok := f.PropAny("table").(string)
		if !ok {
			continue
		}
		if ambiguous[tbl] {
			continue
		}
		if v, ok := values[tbl]; ok {
			f.SetProp("table", v)
			f.SetProp("table_constant", tbl)
		}
	}
}

func parentName(fqn string) string {
	if i := strings.LastIndex(fqn, "."); i >= 0 {
		return fqn[:i]
	}
	return ""
}

func isJavaFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".java")
}

// OwnsFile implements plugin.FileOwner for incremental caching.
func (e *JavaExtractor) OwnsFile(relFile string) bool { return isJavaFile(relFile) }

// AffectsKey implements plugin.KeyDependent: a .kt or .scala file's package
// declaration feeds the cross-language package index used to resolve Java imports
// of Kotlin and Scala types, so a change to either must invalidate the Java
// extractor's cache. Scala matters here in practice, not only in principle —
// apache/spark and apache/pekko both hold hundreds of .java files beside their
// Scala sources, in the same packages.
func (e *JavaExtractor) AffectsKey(relFile string) bool {
	l := strings.ToLower(relFile)
	return strings.HasSuffix(l, ".kt") || strings.HasSuffix(l, ".scala") || strings.HasSuffix(l, ".sc")
}

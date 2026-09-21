package kotlinextractor

import (
	"bufio"
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/extractors/jvmsrc"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// KotlinExtractor extracts architectural facts from Kotlin source code using
// tree-sitter AST parsing (see kotlin_ast.go for the walker implementation).
type KotlinExtractor struct{}

// New creates a new KotlinExtractor.
func New() *KotlinExtractor {
	return &KotlinExtractor{}
}

func (e *KotlinExtractor) Name() string {
	return "kotlin"
}

// Detect returns true if the repository looks like a Kotlin or Android project.
// It recognizes Kotlin regardless of build tool: Gradle (build.gradle[.kts]) and
// Maven (pom.xml declaring the Kotlin plugin/dependency). Build files pin the
// language authoritatively; when none match, a Kotlin source tree under the
// conventional src/main/kotlin root is a sufficient fallback so a repo with an
// unrecognized build setup is still extracted rather than silently skipped.
func (e *KotlinExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// detectByBuild answers from the build definition at the repository root. A
// Gradle or Maven file is shared with Java and Scala, so the plugin or coordinate
// is what names the language; this half stays exactly as conservative as it was.
func (e *KotlinExtractor) detectByBuild(repoPath string) (bool, error) {

	for _, name := range []string{"build.gradle.kts", "build.gradle"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		content := string(data)
		if strings.Contains(content, "kotlin") || strings.Contains(content, "android") {
			return true, nil
		}
	}
	// Maven: a Kotlin project declares the kotlin-maven-plugin / org.jetbrains.kotlin
	// dependency and typically a src/main/kotlin sourceDirectory in its pom.xml.
	if data, err := os.ReadFile(filepath.Join(repoPath, "pom.xml")); err == nil {
		content := string(data)
		if strings.Contains(content, "org.jetbrains.kotlin") ||
			strings.Contains(content, "kotlin-maven-plugin") ||
			strings.Contains(content, "src/main/kotlin") {
			return true, nil
		}
	}
	// Fallback: a conventional Kotlin source root exists even without a recognized
	// build file. Cheap stat, no directory walk.
	if fi, err := os.Stat(filepath.Join(repoPath, "src", "main", "kotlin")); err == nil && fi.IsDir() {
		return true, nil
	}
	return false, nil
}

// DetectFiles implements plugin.FileListDetector.
//
// Everything detectByBuild reads is anchored at the repository ROOT, which is why
// an Android module inside a Flutter plugin never matched: flutterfire declares
// Kotlin in packages/<pkg>/<pkg>/android/build.gradle, and 61 .kt files went
// unindexed for want of a root that would never exist. .kt and .kts are
// unambiguous — the caution that applies to build.gradle does not apply to the
// source extension itself — so membership settles it.
func (e *KotlinExtractor) DetectFiles(repoPath string, files []string) (bool, error) {
	if ok, err := e.detectByBuild(repoPath); ok || err != nil {
		return ok, err
	}
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, "build", "target") {
			continue
		}
		if l := strings.ToLower(rel); strings.HasSuffix(l, ".kt") || strings.HasSuffix(l, ".kts") {
			return true, nil
		}
	}
	return false, nil
}

// Extract parses Kotlin files and emits architectural facts.
//
// Each file is parsed with tree-sitter and walked by the AST visitor in
// kotlin_ast.go, which produces declaration symbol facts, import dependency
// facts, Room storage facts, and call-graph relations (RelInstantiates,
// RelInjects) suitable for reverse-dependency queries.
func (e *KotlinExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var allFacts []facts.Fact

	isAndroid := detectAndroidProject(repoPath)
	sourceRoot := detectKotlinSourceRoot(repoPath, files)
	basePackage := detectKotlinBasePackage(repoPath)
	// Multi-module resolution: map every declared package (across .kt AND .java
	// files) to its real directory so cross-module imports resolve to the module
	// that actually holds the package, instead of collapsing onto the single most
	// common source root. sourceRoot is retained only as a graceful fallback for
	// internal imports whose package we somehow did not index.
	packageIndex := jvmsrc.BuildPackageIndex(repoPath, files)

	var kotlinFiles []string
	for _, relFile := range files {
		if isKotlinFile(relFile) {
			kotlinFiles = append(kotlinFiles, relFile)
		}
	}

	// The detected flags above are read-only, and the per-file extractors are
	// pure, so parse in parallel and merge in file order for deterministic output.
	perFileFacts := parallel.MapFiles(ctx, kotlinFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[kotlin-extractor] error reading %s: %v", relFile, err)
			return nil
		}
		ff := extractFileAST(src, relFile, isAndroid, sourceRoot, basePackage, packageIndex)
		ff = append(ff, extractRetrofitFacts(src, relFile)...)
		return append(ff, extractServletRouteFacts(src, relFile)...)
	})

	modules := make(map[string]bool)
	for i, ff := range perFileFacts {
		allFacts = append(allFacts, ff...)
		modules[factpath.Dir(kotlinFiles[i])] = true
	}

	for dir := range modules {
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language":           "kotlin",
				facts.PropModuleRole: jvmsrc.ModuleRole(dir),
			},
		})
	}

	return allFacts, nil
}

// --- Regex helpers shared with the AST walker ---

var (
	// packageRe extracts a Kotlin file's package declaration. Used to locate
	// the source-root prefix when resolving internal imports to filesystem paths.
	packageRe = regexp.MustCompile(`^\s*package\s+([\w.]+)`)

	// privateOrInternalRe is used by the AST walker to determine whether a
	// declaration's modifier text contains a visibility keyword that excludes
	// it from the project's exported surface.
	privateOrInternalRe = regexp.MustCompile(`\b(private|internal)\b`)
)

func isKotlinFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".kt")
}

// OwnsFile implements plugin.FileOwner for incremental caching.
func (e *KotlinExtractor) OwnsFile(relFile string) bool { return isKotlinFile(relFile) }

// AffectsKey implements plugin.KeyDependent: a .java or .scala file's package
// declaration feeds the cross-language package index used to resolve Kotlin
// imports, so a change to either must invalidate the Kotlin extractor's cache.
func (e *KotlinExtractor) AffectsKey(relFile string) bool {
	l := strings.ToLower(relFile)
	return strings.HasSuffix(l, ".java") || strings.HasSuffix(l, ".scala") || strings.HasSuffix(l, ".sc")
}

// --- Android & framework detection helpers (called by the AST walker) ---

// detectAndroidProject checks for AndroidManifest.xml.
func detectAndroidProject(repoPath string) bool {
	candidates := []string{
		filepath.Join(repoPath, "app", "src", "main", "AndroidManifest.xml"),
		filepath.Join(repoPath, "src", "main", "AndroidManifest.xml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// addAndroidProps classifies a class/interface declaration as an Android component.
func addAndroidProps(f *facts.Fact, name string, annotations []string, supertypes string) {
	if containsAnnotation(annotations, "HiltAndroidApp") {
		f.SetProp("android_component", "application")
		f.SetProp("framework", "android")
		return
	}
	if containsAnnotation(annotations, "HiltViewModel") {
		f.SetProp("android_component", "viewmodel")
		f.SetProp("framework", "android")
		return
	}
	if containsAnnotation(annotations, "AndroidEntryPoint") {
		f.SetProp("framework", "android")
		if supertypeMatches(supertypes, "Activity", "ComponentActivity", "AppCompatActivity", "FragmentActivity") {
			f.SetProp("android_component", "activity")
		} else if supertypeMatches(supertypes, "Fragment") {
			f.SetProp("android_component", "fragment")
		} else if supertypeMatches(supertypes, "Service") {
			f.SetProp("android_component", "service")
		} else if supertypeMatches(supertypes, "BroadcastReceiver") {
			f.SetProp("android_component", "broadcast_receiver")
		}
		return
	}
	if containsAnnotation(annotations, "Component") || containsAnnotation(annotations, "Subcomponent") {
		// Dagger/Hilt DI component interface — infrastructure, not domain code.
		f.SetProp("di_component", true)
		f.SetProp("framework", "android")
		return
	}
	if containsAnnotation(annotations, "Module") {
		f.SetProp("android_component", "di_module")
		f.SetProp("di_module", true)
		f.SetProp("framework", "android")
		return
	}

	if strings.HasSuffix(name, "ViewModel") || supertypeMatches(supertypes, "ViewModel") {
		f.SetProp("android_component", "viewmodel")
		f.SetProp("framework", "android")
		return
	}
	if supertypeMatches(supertypes, "Application") {
		f.SetProp("android_component", "application")
		f.SetProp("framework", "android")
		return
	}
	if supertypeMatches(supertypes, "Activity", "ComponentActivity", "AppCompatActivity") {
		f.SetProp("android_component", "activity")
		f.SetProp("framework", "android")
		return
	}
	if supertypeMatches(supertypes, "Fragment") {
		f.SetProp("android_component", "fragment")
		f.SetProp("framework", "android")
		return
	}
	if supertypeMatches(supertypes, "Service", "FirebaseMessagingService", "IntentService", "JobIntentService") {
		f.SetProp("android_component", "service")
		f.SetProp("framework", "android")
		return
	}
	if supertypeMatches(supertypes, "BroadcastReceiver") {
		f.SetProp("android_component", "broadcast_receiver")
		f.SetProp("framework", "android")
		return
	}
	if supertypeMatches(supertypes, "ContentProvider") {
		f.SetProp("android_component", "content_provider")
		f.SetProp("framework", "android")
		return
	}
	if supertypeMatches(supertypes, "Worker", "CoroutineWorker", "ListenableWorker") {
		f.SetProp("android_component", "worker")
		f.SetProp("framework", "android")
		return
	}
	if strings.HasSuffix(name, "Repository") || strings.HasSuffix(name, "RepositoryImpl") {
		f.SetProp("android_component", "repository")
		f.SetProp("framework", "android")
		return
	}
	if strings.HasSuffix(name, "UseCase") {
		f.SetProp("android_component", "usecase")
		f.SetProp("framework", "android")
		return
	}
}

// detectRoomStorage emits a storage fact for Room-annotated classes/interfaces.
func detectRoomStorage(name string, annotations []string, relFile string, line int, dir string) *facts.Fact {
	var storageKind string
	switch {
	case containsAnnotation(annotations, "Entity"):
		storageKind = "entity"
	case containsAnnotation(annotations, "Dao"):
		storageKind = "dao"
	case containsAnnotation(annotations, "Database"):
		storageKind = "database"
	default:
		return nil
	}
	return &facts.Fact{
		Kind: facts.KindStorage,
		Name: dir + "." + name,
		File: relFile,
		Line: line,
		Props: map[string]any{
			"storage_kind": storageKind,
			"language":     "kotlin",
			"framework":    "room",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: dir},
		},
	}
}

// containsAnnotation reports whether the simple-name list contains `name`.
func containsAnnotation(annotations []string, name string) bool {
	for _, a := range annotations {
		if a == name {
			return true
		}
	}
	return false
}

// supertypeMatches reports whether any of the comma-joined supertype names
// matches one of the provided candidates. Used by addAndroidProps to classify
// Android components by their parent type.
func supertypeMatches(supertypes string, names ...string) bool {
	if supertypes == "" {
		return false
	}
	for _, st := range parseSupertypes(supertypes) {
		for _, name := range names {
			if st == name {
				return true
			}
		}
	}
	return false
}

// parseSupertypes splits a supertype clause like "Foo(), Bar, Baz<T>" into type
// names. It tolerates nested generic and constructor-argument parentheses.
func parseSupertypes(clause string) []string {
	var result []string
	depth := 0
	start := 0
	for i, ch := range clause {
		switch ch {
		case '<', '(':
			depth++
		case '>', ')':
			depth--
		case ',':
			if depth == 0 {
				if t := extractTypeName(clause[start:i]); t != "" {
					result = append(result, t)
				}
				start = i + 1
			}
		}
	}
	if t := extractTypeName(clause[start:]); t != "" {
		result = append(result, t)
	}
	return result
}

// extractTypeName strips generic parameters and constructor calls off a
// supertype entry like "Foo()" or "Bar<T>" and returns the simple type name.
func extractTypeName(s string) string {
	s = strings.TrimSpace(s)
	for i, ch := range s {
		if ch == '<' || ch == '(' || ch == ' ' {
			s = s[:i]
			break
		}
	}
	s = strings.TrimSpace(s)
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		s = s[idx+1:]
	}
	return s
}

// --- Source-root and import resolution (project-level) ---

// detectKotlinSourceRoot derives the source-root directory shared by the
// project's production Kotlin files, by stripping each file's package path from
// its directory. For "app/src/main/java/com/foo/Bar.kt" declaring `package
// com.foo`, the per-file root is "app/src/main/java/".
//
// It deliberately ignores test source sets (src/test, src/androidTest) and picks
// the MOST COMMON production root rather than the first file seen. File order is
// not guaranteed: with the old "first file wins" logic, a project whose first
// walked file was a test ("app/src/androidTest/java/…") resolved every internal
// import under that test root, so the targets never matched the real (main)
// module dirs and coupling collapsed to zero.
func detectKotlinSourceRoot(repoPath string, files []string) string {
	counts := make(map[string]int) // production source root -> file count
	fallback := ""                 // any root seen, used only if all files are tests
	haveFallback := false

	for _, relFile := range files {
		if !isKotlinFile(relFile) {
			continue
		}
		root, ok := kotlinFileSourceRoot(repoPath, relFile)
		if !ok {
			continue
		}
		if !haveFallback {
			fallback, haveFallback = root, true
		}
		if isKotlinTestSource(relFile) {
			continue
		}
		counts[root]++
	}

	best, bestN, found := "", 0, false
	for root, n := range counts {
		if !found || n > bestN || (n == bestN && root < best) {
			best, bestN, found = root, n, true
		}
	}
	if found {
		return best
	}
	return fallback
}

// kotlinFileSourceRoot returns a single file's source root: its directory with
// its package path stripped. ok is false when the file has no package decl.
func kotlinFileSourceRoot(repoPath, relFile string) (string, bool) {
	absFile := filepath.Join(repoPath, relFile)
	f, err := os.Open(absFile)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		m := packageRe.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		pkgPath := strings.ReplaceAll(m[1], ".", "/")
		dir := factpath.Dir(relFile)
		if strings.HasSuffix(dir, pkgPath) {
			return strings.TrimSuffix(dir, pkgPath), true
		}
		return "", true // package found but dir doesn't mirror it — root is ""
	}
	return "", false
}

// isKotlinTestSource reports whether a file lives in a Gradle test source set
// (src/test or src/androidTest), which must not drive source-root detection.
func isKotlinTestSource(relFile string) bool {
	p := filepath.ToSlash(relFile)
	return strings.Contains(p, "/src/test/") || strings.HasPrefix(p, "src/test/") ||
		strings.Contains(p, "/src/androidTest/") || strings.HasPrefix(p, "src/androidTest/")
}

// detectKotlinBasePackage reads the Android namespace from build.gradle.kts so
// that internal imports (matching the project's package) can be resolved to
// filesystem paths rather than being treated as external library imports.
func detectKotlinBasePackage(repoPath string) string {
	candidates := []string{
		filepath.Join(repoPath, "app", "build.gradle.kts"),
		filepath.Join(repoPath, "app", "build.gradle"),
		filepath.Join(repoPath, "build.gradle.kts"),
		filepath.Join(repoPath, "build.gradle"),
	}
	// Match both Kotlin-DSL (`namespace = "x"`) and Groovy (`namespace 'x'`)
	// build scripts — Groovy uses single quotes and omits the `=`, so a
	// double-quote-only pattern silently fails on `.gradle` files, leaving the base
	// package empty and making every in-repo import resolve as external.
	nsRe := regexp.MustCompile(`namespace\s*=?\s*['"]([^'"]+)['"]`)
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if m := nsRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// resolveKotlinImport normalizes a Kotlin import path. Internal imports
// (matching the project's base package) become filesystem-relative paths so
// the graph can connect them to module facts; everything else is treated as
// an external dependency.
//
// Resolution is package-index first: an import is mapped to the directory that
// actually declares its package (across all modules and both .kt/.java files),
// so cross-module imports in a multi-module Gradle project land on the right
// module rather than collapsing onto the single most common source root. Only
// when the package is not in the index do we fall back to the base-package +
// single-source-root heuristic, so behaviour degrades gracefully instead of
// dropping an otherwise-internal edge.
func resolveKotlinImport(importPath string, packageIndex map[string]string, sourceRoot, basePackage string) (string, bool) {
	if resolved, ok := jvmsrc.ResolveImport(importPath, packageIndex); ok {
		return resolved, false
	}
	if basePackage != "" && sourceRoot != "" && strings.HasPrefix(importPath, basePackage+".") {
		asPath := strings.ReplaceAll(importPath, ".", "/")
		return factpath.Clean(sourceRoot + asPath), false
	}
	return importPath, true
}

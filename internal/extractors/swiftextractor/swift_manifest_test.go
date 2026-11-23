package swiftextractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// writeManifestRepo builds a temp SPM package: a Package.swift plus a stub source
// file per target, and an Info.plist so iOS classification runs.
func writeManifestRepo(t *testing.T, manifest string, targets []string) (string, []string) {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []string{"Packages/Mods/Package.swift"}
	mustWrite(t, repo, "Packages/Mods/Package.swift", manifest)
	for _, tgt := range targets {
		rel := "Packages/Mods/Sources/" + tgt + "/" + tgt + "Placeholder.swift"
		mustWrite(t, repo, rel, "public enum "+tgt+"Placeholder { public static let moduleName = \""+tgt+"\" }\n")
		files = append(files, rel)
	}
	return repo, files
}

func mustWrite(t *testing.T, repo, rel, content string) {
	t.Helper()
	p := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sampleManifest = `// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Mods",
    products: [
        .library(name: "AppShell", targets: ["AppShell"])
    ],
    targets: [
        .target(name: "CoreTypes"),
        .target(name: "DesignSystem", dependencies: ["CoreTypes"]),
        .target(name: "FeatureDashboard", dependencies: ["DesignSystem"]),
        .target(name: "AppComposition", dependencies: ["DesignSystem"]),
        .target(name: "AppShell", dependencies: ["AppComposition", "FeatureDashboard"]),
        .testTarget(name: "AppShellTests", dependencies: ["AppShell"])
    ]
)
`

func TestManifest_TargetDependencyEdges(t *testing.T) {
	targets := []string{"CoreTypes", "DesignSystem", "FeatureDashboard", "AppComposition", "AppShell"}
	repo, files := writeManifestRepo(t, sampleManifest, targets)

	allFacts, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}

	base := "Packages/Mods/Sources"
	// Dependency facts present with resolved module-dir targets.
	wantDeps := [][2]string{
		{base + "/AppShell", base + "/AppComposition"},
		{base + "/AppShell", base + "/FeatureDashboard"},
		{base + "/DesignSystem", base + "/CoreTypes"},
		{base + "/FeatureDashboard", base + "/DesignSystem"},
	}
	for _, wd := range wantDeps {
		if !hasDependencyEdge(allFacts, wd[0], wd[1]) {
			t.Errorf("missing SPM dependency edge %s -> %s", wd[0], wd[1])
		}
	}

	// Target module facts carry SPM identity.
	if m, ok := findFact(allFacts, base+"/AppShell"); !ok {
		t.Error("expected module fact for AppShell target")
	} else if m.Kind != facts.KindModule || m.Props["spm_target"] != "AppShell" {
		t.Errorf("AppShell module fact wrong: kind=%s props=%v", m.Kind, m.Props)
	}
}

func TestManifest_GraphTraversal(t *testing.T) {
	targets := []string{"CoreTypes", "DesignSystem", "FeatureDashboard", "AppComposition", "AppShell"}
	repo, files := writeManifestRepo(t, sampleManifest, targets)
	allFacts, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}

	base := "Packages/Mods/Sources"
	g := facts.NewGraph(allFacts)

	// Forward from AppShell reaches its transitive dependencies.
	fwd := g.Traverse(base+"/AppShell", "forward", []string{"imports"}, nil, 0, 0)
	reached := map[string]bool{}
	for _, n := range fwd.Nodes {
		reached[n.Name] = true
	}
	for _, want := range []string{base + "/AppComposition", base + "/FeatureDashboard", base + "/DesignSystem", base + "/CoreTypes"} {
		if !reached[want] {
			t.Errorf("forward traverse from AppShell did not reach %s; reached=%v", want, reached)
		}
	}

	// Reverse impact on DesignSystem lists the targets that depend on it.
	impact := g.ImpactSet(base+"/DesignSystem", 0, 0, false)
	dependents := map[string]bool{}
	for _, nodes := range impact.ByDepth {
		for _, n := range nodes {
			dependents[n.Name] = true
		}
	}
	for _, want := range []string{base + "/FeatureDashboard", base + "/AppComposition", base + "/AppShell"} {
		if !dependents[want] {
			t.Errorf("impact on DesignSystem missing dependent %s; got %v", want, dependents)
		}
	}
}

func TestManifest_JunkSuppressed(t *testing.T) {
	targets := []string{"CoreTypes", "DesignSystem", "FeatureDashboard", "AppComposition", "AppShell"}
	repo, files := writeManifestRepo(t, sampleManifest, targets)
	allFacts, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range allFacts {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, ".package") {
			t.Errorf("manifest produced junk symbol fact: %s", f.Name)
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports && r.Target == "PackageDescription" {
				t.Errorf("manifest produced junk PackageDescription import on %s", f.Name)
			}
		}
	}

	// Sanity: the real placeholder enums in target dirs are still extracted.
	if _, ok := findFact(allFacts, "Packages/Mods/Sources/AppShell.AppShellPlaceholder"); !ok {
		t.Error("expected placeholder enum AppShellPlaceholder to still be extracted")
	}
}

func hasDependencyEdge(ff []facts.Fact, fromFileDir, target string) bool {
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		if fileDir(f.File) != fromFileDir {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports && r.Target == target {
				return true
			}
		}
	}
	return false
}

func fileDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}

// TestManifestSkipsDependenciesOfAnUnindexedTestTarget pins what happens to a test
// target none of whose files was indexed, which since test trees are ignored by
// default is every SPM test target. Its dependency facts are not emitted: an
// invented `<dir>/Package.swift` names a file that does not exist, and the manifest's
// path files the edge under the manifest's directory, a module that does not exist
// (it headed a "Deep dependency chain: ." finding). A production target keeps its
// edges, and so does a test target whose source is indexed.
func TestManifestSkipsDependenciesOfAnUnindexedTestTarget(t *testing.T) {
	const manifestRel = "Packages/Mods/Package.swift"
	run := func(dirToFile map[string]string) map[string]string {
		got := map[string]string{}
		for _, f := range parsePackageManifest([]byte(sampleManifest), manifestRel, dirToFile) {
			if f.Kind == facts.KindDependency {
				got[f.Name] = f.File
			}
		}
		return got
	}
	const testDep = "Packages/Mods/Tests/AppShellTests -> Packages/Mods/Sources/AppShell"
	const prodDep = "Packages/Mods/Sources/AppShell -> Packages/Mods/Sources/AppComposition"
	prodFile := "Packages/Mods/Sources/AppShell/AppShellPlaceholder.swift"

	got := run(map[string]string{"Packages/Mods/Sources/AppShell": prodFile})
	if f, ok := got[testDep]; ok {
		t.Errorf("unindexed test target still emits %s (file %q)", testDep, f)
	}
	if got[prodDep] != prodFile {
		t.Errorf("production dependency file = %q, want %q", got[prodDep], prodFile)
	}
	for name, f := range got {
		if strings.HasPrefix(fileDir(f), "Packages/Mods/Tests/AppShellTests") || f == manifestRel {
			t.Errorf("%s: file %q", name, f)
		}
	}

	testFile := "Packages/Mods/Tests/AppShellTests/AppShellTests.swift"
	got = run(map[string]string{"Packages/Mods/Sources/AppShell": prodFile, "Packages/Mods/Tests/AppShellTests": testFile})
	if got[testDep] != testFile {
		t.Errorf("indexed test target's dependency file = %q, want %q", got[testDep], testFile)
	}
}

// TestManifestTestTargetDefaultsToTestsDir pins SwiftPM's default layout: a
// `.testTarget(name: X)` with no `path:` lives in `Tests/X`, every other target
// kind in `Sources/X`, and an explicit `path:` wins for both. The module fact, the
// resolver seed and the dependency edges all name the test target's real directory,
// so a test file indexed under `Tests/X` joins the target instead of sitting beside
// an empty `Sources/X` module.
func TestManifestTestTargetDefaultsToTestsDir(t *testing.T) {
	const manifest = `// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "Mods",
    targets: [
        .target(name: "Core"),
        .testTarget(name: "CoreTests", dependencies: ["Core"]),
        .testTarget(name: "PinnedTests", dependencies: ["Core"], path: "Checks/Pinned")
    ]
)
`
	const manifestRel = "Packages/Mods/Package.swift"
	wantDirs := map[string]string{
		"Core":        "Packages/Mods/Sources/Core",
		"CoreTests":   "Packages/Mods/Tests/CoreTests",
		"PinnedTests": "Packages/Mods/Checks/Pinned",
	}

	roots := manifestTargetRoots([]byte(manifest), manifestRel)
	for name, want := range wantDirs {
		if roots[name] != want {
			t.Errorf("manifestTargetRoots[%s] = %q, want %q", name, roots[name], want)
		}
	}

	modules := map[string]string{}
	for _, f := range parsePackageManifest([]byte(manifest), manifestRel, nil) {
		if f.Kind == facts.KindModule {
			modules[f.Props["spm_target"].(string)] = f.Name
		}
	}
	for name, want := range wantDirs {
		if modules[name] != want {
			t.Errorf("module fact for %s = %q, want %q", name, modules[name], want)
		}
	}

	// End to end, with the test file indexed: its symbols declare into the test
	// target's module and the target's dependency sits in that file.
	repo := t.TempDir()
	mustWrite(t, repo, manifestRel, manifest)
	coreFile := "Packages/Mods/Sources/Core/Core.swift"
	testFile := "Packages/Mods/Tests/CoreTests/CoreTests.swift"
	mustWrite(t, repo, coreFile, "public enum CoreThing {}\n")
	mustWrite(t, repo, testFile, "import Core\nfinal class CoreThingCheck {}\n")
	all, err := New().Extract(context.Background(), repo, []string{manifestRel, coreFile, testFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findFact(all, "Packages/Mods/Tests/CoreTests.CoreThingCheck"); !ok {
		t.Error("test file's class did not declare into Packages/Mods/Tests/CoreTests")
	}
	const dep = "Packages/Mods/Tests/CoreTests -> Packages/Mods/Sources/Core"
	found := false
	for _, f := range all {
		if f.Kind == facts.KindDependency && f.Name == dep {
			found = true
			if f.File != testFile {
				t.Errorf("%s: file %q, want %q", dep, f.File, testFile)
			}
		}
		if f.Kind == facts.KindModule && f.Name == "Packages/Mods/Sources/CoreTests" {
			t.Errorf("module fact for a directory that does not exist: %s", f.Name)
		}
	}
	if !found {
		t.Errorf("missing dependency %s", dep)
	}
}

// TestManifestAppendedTargets: targets added to the Package binding after the
// literal (`package.targets.append(contentsOf: [...])` inside a condition,
// `.append(.testTarget(...))`, `+= [...]`) are targets like the literal's, with
// the same per-kind directory defaults, and their dependencies resolve to
// target directories. An append to some other value's targets is not one.
func TestManifestAppendedTargets(t *testing.T) {
	const manifest = `// swift-tools-version: 5.9
import Foundation
import PackageDescription

var package = Package(
    name: "Game",
    targets: [
        .target(name: "Models"),
    ]
)

if ProcessInfo.processInfo.environment["CLIENT_ONLY"] == nil {
    package.products.append(contentsOf: [
        .library(name: "Server", targets: ["Server"]),
    ])
    package.targets.append(contentsOf: [
        .target(name: "Server", dependencies: ["Models", .product(name: "NIO", package: "swift-nio")]),
        .target(name: "Pinned", dependencies: ["Server"], path: "Custom/Pinned"),
    ])
}
package.targets.append(.testTarget(name: "ServerTests", dependencies: ["Server"]))
package.targets += [.executableTarget(name: "runner", dependencies: ["Server"])]
other.targets.append(.target(name: "NotOurs"))
`
	const manifestRel = "Package.swift"
	wantDirs := map[string]string{
		"Models":      "Sources/Models",
		"Server":      "Sources/Server",
		"Pinned":      "Custom/Pinned",
		"ServerTests": "Tests/ServerTests",
		"runner":      "Sources/runner",
	}

	roots := manifestTargetRoots([]byte(manifest), manifestRel)
	if len(roots) != len(wantDirs) {
		t.Errorf("manifestTargetRoots = %v, want %v", roots, wantDirs)
	}
	for name, want := range wantDirs {
		if roots[name] != want {
			t.Errorf("manifestTargetRoots[%s] = %q, want %q", name, roots[name], want)
		}
	}

	modules := map[string]facts.Fact{}
	deps := map[string]string{}
	dirToFile := map[string]string{"Tests/ServerTests": "Tests/ServerTests/ServerTests.swift"}
	for _, f := range parsePackageManifest([]byte(manifest), manifestRel, dirToFile) {
		switch f.Kind {
		case facts.KindModule:
			modules[f.Props["spm_target"].(string)] = f
		case facts.KindDependency:
			deps[f.Name] = f.Props["source"].(string)
		}
	}
	for name, want := range wantDirs {
		if modules[name].Name != want {
			t.Errorf("module fact for %s = %q, want %q", name, modules[name].Name, want)
		}
	}
	if role := modules["ServerTests"].Props["module_role"]; role != facts.ModuleRoleTest {
		t.Errorf("ServerTests module_role = %v, want test", role)
	}
	if role := modules["Server"].Props["module_role"]; role != facts.ModuleRoleProduction {
		t.Errorf("Server module_role = %v, want production", role)
	}
	wantDeps := map[string]string{
		"Sources/Server -> Sources/Models":    "internal",
		"Sources/Server -> NIO":               "external",
		"Custom/Pinned -> Sources/Server":     "internal",
		"Tests/ServerTests -> Sources/Server": "internal",
		"Sources/runner -> Sources/Server":    "internal",
	}
	for name, src := range wantDeps {
		if deps[name] != src {
			t.Errorf("dependency %s: source %q, want %q", name, deps[name], src)
		}
	}
	if len(deps) != len(wantDeps) {
		t.Errorf("dependencies = %v, want %v", deps, wantDeps)
	}
}

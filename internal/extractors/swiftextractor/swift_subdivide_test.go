package swiftextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractModuleSet runs the extractor over a temp repo and returns the set of
// emitted module fact names.
func extractModuleSet(t *testing.T, repo string, files []string) map[string]facts.Fact {
	t.Helper()
	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	modules := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind == facts.KindModule {
			modules[f.Name] = f
		}
	}
	return modules
}

// TestSubdivide_ApplicationTargetSplitsByDirectory: a single XcodeGen application
// target spanning several nested directories emits one module PER LEAF DIRECTORY
// (Go/Ruby-style), instead of collapsing the whole app into one module. A file at
// the target root forms the root-named package.
func TestSubdivide_ApplicationTargetSplitsByDirectory(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, repo, "project.yml", `
name: app
targets:
  App:
    type: application
    sources:
      - App
`)
	files := []string{}
	add := func(rel, content string) {
		mustWrite(t, repo, rel, content)
		files = append(files, rel)
	}
	add("App/App.swift", "public struct AppRoot {}\n")
	add("App/Domain/Models/Round.swift", "public struct Round { public let strokes: Int = 0 }\n")
	add("App/Domain/Contracts/RoundGateway.swift", "public protocol RoundGateway { func load() }\n")
	add("App/Data/Network/Client.swift", "public final class Client {}\n")
	add("App/Screens/Dashboard/DashboardView.swift", "public struct DashboardView {}\n")

	modules := extractModuleSet(t, repo, files)

	// One module per leaf directory, including the target root for App/App.swift.
	for _, want := range []string{
		"App",
		"App/Domain/Models",
		"App/Domain/Contracts",
		"App/Data/Network",
		"App/Screens/Dashboard",
	} {
		if _, ok := modules[want]; !ok {
			t.Errorf("missing per-directory module %q; got %v", want, keysOf(modules))
		}
	}
	// The whole app must NOT collapse: intermediate/whole-target identities are not
	// a single catch-all module holding every type.
	if m, ok := modules["App/Domain/Models"]; ok {
		if _, hasType := m.Props["xcode_target"]; hasType {
			t.Errorf("leaf module App/Domain/Models should be a plain directory module, got props %v", m.Props)
		}
	}
	// A type declared in a leaf dir is attributed to that leaf module, not "App".
	if !symbolDeclaredIn(t, repo, files, "App/Domain/Models.Round", "App/Domain/Models") {
		t.Error("Round should declare into module App/Domain/Models")
	}
}

// TestSubdivide_FrameworkTargetStaysWhole: a framework target (an import unit) is
// NOT subdivided — its nested directories collapse to the single target module, so
// `import FrameworkName` from another target still resolves.
func TestSubdivide_FrameworkTargetStaysWhole(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, repo, "project.yml", `
name: app
targets:
  Feature:
    type: framework
    sources:
      - Sources/Feature
`)
	files := []string{}
	add := func(rel, content string) {
		mustWrite(t, repo, rel, content)
		files = append(files, rel)
	}
	add("Sources/Feature/Feature.swift", "public struct FeatureEntry {}\n")
	add("Sources/Feature/Views/Home/HomeView.swift", "public struct HomeView {}\n")
	add("Sources/Feature/Models/Item.swift", "public struct Item {}\n")

	modules := extractModuleSet(t, repo, files)

	if _, ok := modules["Sources/Feature"]; !ok {
		t.Fatalf("missing whole framework module Sources/Feature; got %v", keysOf(modules))
	}
	for _, leaked := range []string{"Sources/Feature/Views/Home", "Sources/Feature/Models", "Sources/Feature/Views"} {
		if _, bad := modules[leaked]; bad {
			t.Errorf("framework sub-directory %q leaked as its own module (frameworks stay whole)", leaked)
		}
	}
	if modules["Sources/Feature"].Props["xcode_type"] != "framework" {
		t.Errorf("Sources/Feature xcode_type = %v, want framework", modules["Sources/Feature"].Props["xcode_type"])
	}
}

// TestSubdivide_TestBundleStaysWhole: a bundle.unit-test target collapses to one
// module per bundle (tagged test), never exploding into per-leaf-directory modules,
// even though its sibling application target IS subdivided.
func TestSubdivide_TestBundleStaysWhole(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, repo, "project.yml", `
name: app
targets:
  App:
    type: application
    sources:
      - App
  AppTests:
    type: bundle.unit-test
    sources:
      - Tests/App
`)
	files := []string{}
	add := func(rel, content string) {
		mustWrite(t, repo, rel, content)
		files = append(files, rel)
	}
	add("App/Feature/Widget.swift", "public struct Widget {}\n")
	add("Tests/App/Feature/WidgetTests.swift", "struct WidgetTests {}\n")
	add("Tests/App/Support/Helpers.swift", "struct Helpers {}\n")

	modules := extractModuleSet(t, repo, files)

	// Application subdivides.
	if _, ok := modules["App/Feature"]; !ok {
		t.Errorf("application should subdivide: missing App/Feature; got %v", keysOf(modules))
	}
	// Test bundle collapses to one module tagged test.
	tb, ok := modules["Tests/App"]
	if !ok {
		t.Fatalf("missing collapsed test module Tests/App; got %v", keysOf(modules))
	}
	if tb.Props["module_role"] != facts.ModuleRoleTest {
		t.Errorf("Tests/App module_role = %v, want test", tb.Props["module_role"])
	}
	for _, leaked := range []string{"Tests/App/Feature", "Tests/App/Support"} {
		if _, bad := modules[leaked]; bad {
			t.Errorf("test leaf %q leaked as its own module (test bundles stay whole)", leaked)
		}
	}
}

// TestSubdivide_IntraTargetTypeReferenceEdges is the key coupling test: within one
// subdivided application target, a type reference across directories (Swift needs no
// `import` inside a module) synthesizes a directory→directory dependency edge, so the
// per-directory packages get real Ca/Ce.
func TestSubdivide_IntraTargetTypeReferenceEdges(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, repo, "project.yml", `
name: app
targets:
  App:
    type: application
    sources:
      - App
`)
	files := []string{}
	add := func(rel, content string) {
		mustWrite(t, repo, rel, content)
		files = append(files, rel)
	}
	// RoundStore lives in App/Data/Stores; DashboardView (App/Screens/Dashboard)
	// references it via a property type annotation with no import — same Swift module.
	add("App/Data/Stores/RoundStore.swift", "public final class RoundStore { public init() {} }\n")
	add("App/Screens/Dashboard/DashboardView.swift", "public struct DashboardView {\n    let store: RoundStore\n}\n")

	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if !hasIntraEdge(ff, "App/Screens/Dashboard", "App/Data/Stores") {
		t.Errorf("missing synthesized intra-target edge App/Screens/Dashboard -> App/Data/Stores; deps: %v", depEdges(ff))
	}
}

// symbolDeclaredIn reports whether a symbol fact of the given name declares into the
// expected module via a RelDeclares relation.
func symbolDeclaredIn(t *testing.T, repo string, files []string, symbol, module string) bool {
	t.Helper()
	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol || f.Name != symbol {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelDeclares && r.Target == module {
				return true
			}
		}
	}
	return false
}

// hasIntraEdge reports whether a dependency fact links the two directory modules.
func hasIntraEdge(ff []facts.Fact, from, to string) bool {
	for _, f := range ff {
		if f.Kind == facts.KindDependency && slashDir(f.File) == from && importTargetOf(f) == to {
			return true
		}
	}
	return false
}

func depEdges(ff []facts.Fact) []string {
	var out []string
	for _, f := range ff {
		if f.Kind == facts.KindDependency {
			out = append(out, slashDir(f.File)+" -> "+importTargetOf(f))
		}
	}
	return out
}

// The synthesized edge names the file that makes the reference, not another file of
// the source directory: a layer finding built on it points at code that uses the type.
func TestSubdivide_IntraTargetEdgeNamesTheReferencingFile(t *testing.T) {
	repo := t.TempDir()
	mustWrite(t, repo, "project.yml", `
name: app
targets:
  App:
    type: application
    sources:
      - App
`)
	files := []string{}
	add := func(rel, content string) {
		mustWrite(t, repo, rel, content)
		files = append(files, rel)
	}
	add("App/Data/Models/SummaryRequest.swift", "public struct SummaryRequest { public init() {} }\n")
	add("App/Domain/UseCases/AEntitlementUseCase.swift", "final class AEntitlementUseCase {\n    func run(id: Int) -> Bool { true }\n}\n")
	add("App/Domain/UseCases/GetSummaryUseCase.swift", "final class GetSummaryUseCase {\n    let request: SummaryRequest\n}\n")

	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, f := range ff {
		if f.Kind == facts.KindDependency && importTargetOf(f) == "App/Data/Models" && slashDir(f.File) == "App/Domain/UseCases" {
			if f.File != "App/Domain/UseCases/GetSummaryUseCase.swift" {
				t.Errorf("edge App/Domain/UseCases -> App/Data/Models names %s, want the file that references SummaryRequest", f.File)
			}
			return
		}
	}
	t.Fatalf("missing edge App/Domain/UseCases -> App/Data/Models; deps: %v", depEdges(ff))
}

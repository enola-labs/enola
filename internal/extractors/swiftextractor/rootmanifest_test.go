package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Package.swift at the repository root declares targets at their default
// Sources/<name> path. Spelled "./Sources/<name>" the target matched no source
// directory, so each of its subdirectories became a module of its own and a
// reference each way between two of them was reported as a cycle.
func TestRootManifestTargetsOwnTheirSubdirectories(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Package.swift": `// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Kit",
    targets: [
        .target(name: "KitUI"),
        .target(name: "KitCore", dependencies: ["KitUI"]),
    ]
)
`,
		"Sources/KitUI/Components/Accordion/Accordion.swift":     "public struct Accordion { let t = AccordionTokens() }\n",
		"Sources/KitUI/Components/Accordion/Tokens/Tokens.swift": "struct AccordionTokens { let a: Accordion? = nil }\n",
		"Sources/KitCore/Core.swift":                             "import KitUI\n\nstruct Core { let a: Accordion? = nil }\n",
	})
	modules := map[string]bool{}
	var coreImports []string
	for _, f := range ff {
		switch f.Kind {
		case facts.KindModule:
			modules[f.Name] = true
		case facts.KindDependency:
			if f.File == "Sources/KitCore/Core.swift" {
				for _, r := range f.Relations {
					coreImports = append(coreImports, r.Target)
				}
			}
		}
	}
	for _, want := range []string{"Sources/KitUI", "Sources/KitCore"} {
		if !modules[want] {
			t.Errorf("no target module %s among %v", want, modules)
		}
	}
	for m := range modules {
		if m != "Sources/KitUI" && m != "Sources/KitCore" {
			t.Errorf("unexpected module %s: a directory inside a target is not a module", m)
		}
	}
	found := false
	for _, imp := range coreImports {
		found = found || imp == "Sources/KitUI"
	}
	if !found {
		t.Errorf("KitCore's import of KitUI resolved to %v, want Sources/KitUI", coreImports)
	}
}

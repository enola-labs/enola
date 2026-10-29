package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Targets beside a Project.swift: a folder named after an imported module is that
// module, a …Tests folder and every folder of a test project are tests, and anything
// else (resources, a folder sharing a system framework's name) is not a target.
func TestConventionTargets_Layout(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Features/Orders/Project.swift":                          "import ProjectDescription\nlet project = Orders\n",
		"Features/Orders/OrdersContract/OrdersRouting.swift":     "public protocol OrdersRouting {}\n",
		"Features/Orders/OrdersImplementation/OrdersView.swift":  "import OrdersContract\nimport Networking\nimport SwiftUI\nfinal class OrdersView {}\n",
		"Features/Orders/OrdersImplementation/SwiftUI/Row.swift": "import SwiftUI\nstruct Row {}\n",
		"Features/Orders/OrdersTests/OrdersViewTests.swift":      "@testable import OrdersImplementation\nfinal class OrdersViewTests {}\n",
		"Features/Orders/OpenAPI/Generated.swift":                "struct Generated {}\n",
		"Features/Orders/OrdersDemoApplication/DemoApp.swift":    "import OrdersImplementation\nstruct DemoApp {}\n",
		"Tuist/ProjectDescriptionHelpers/Orders.swift":           "import ProjectDescription\nlet Orders = 1\n",
		"Core/Networking/Project.swift":                          "import ProjectDescription\nlet project = Networking\n",
		"Core/Networking/Networking/Client.swift":                "public final class Client {}\n",
		"UITests/Project.swift":                                  "import ProjectDescription\n",
		"UITests/Orders/OrdersFlow.swift":                        "import OrdersContract\nfinal class OrdersFlow {}\n",
	})
	modules := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind == facts.KindModule {
			modules[f.Name] = f
		}
	}
	for dir, want := range map[string][2]string{
		"Features/Orders/OrdersContract":        {facts.ModuleRoleProduction, "OrdersContract"},
		"Features/Orders/OrdersImplementation":  {facts.ModuleRoleProduction, "OrdersImplementation"},
		"Features/Orders/OrdersTests":           {facts.ModuleRoleTest, ""},
		"Core/Networking/Networking":            {facts.ModuleRoleProduction, "Networking"},
		"UITests/Orders":                        {facts.ModuleRoleTest, ""},
		"Features/Orders/OrdersDemoApplication": {facts.ModuleRoleTooling, ""},
		"Tuist/ProjectDescriptionHelpers":       {facts.ModuleRoleTooling, ""},
	} {
		m, ok := modules[dir]
		if !ok {
			t.Errorf("no module %s", dir)
			continue
		}
		if m.PropString("module_role") != want[0] || m.PropString("xcode_target") != want[1] {
			t.Errorf("%s: role %q target %q, want %v", dir, m.PropString("module_role"), m.PropString("xcode_target"), want)
		}
	}
	if m, ok := modules["Features/Orders"]; ok && (m.PropString("xcode_target") != "" || m.PropString("module_role") == facts.ModuleRoleProduction) {
		t.Errorf("a project manifest made its project folder a target: %v", m.Props)
	}
	if _, ok := modules["Features/Orders/OrdersImplementation/SwiftUI"]; ok {
		t.Errorf("a folder inside a target became a module")
	}

	internal := map[string]string{}
	for _, f := range ff {
		if f.Kind != facts.KindDependency {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImports {
				internal[r.Target] = f.PropString("source")
				if r.Target == "enum" || r.Target == "class" {
					t.Errorf("import kind recorded as a module: %s", f.Name)
				}
			}
		}
	}
	for target, want := range map[string]string{
		"Features/Orders/OrdersContract": "internal",
		"Core/Networking/Networking":     "internal",
		"SwiftUI":                        "stdlib",
	} {
		if internal[target] != want {
			t.Errorf("import %s: source %q, want %q (all: %v)", target, internal[target], want, internal)
		}
	}
}

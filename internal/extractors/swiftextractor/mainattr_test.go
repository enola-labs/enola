package swiftextractor

import "testing"

// @main makes a type the program's entry point: an executable target's, a
// command line tool's or a compiler plugin's. The runtime enters it on every
// platform, not only a SwiftUI App on iOS.
func TestMainAttributeMarksAnyTypeAsEntered(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Run/entrypoint.swift": `
@main
struct Entrypoint {
    static func main() async throws {}
}
`,
		"Sources/MacrosPlugin/Plugin.swift": `
@main struct MacrosPlugin: CompilerPlugin {
    let providingMacros: [Macro.Type] = []
}
final class NotMain {}
`,
	})
	want := map[string]bool{
		"Sources/Run.Entrypoint":            true,
		"Sources/MacrosPlugin.MacrosPlugin": true,
		"Sources/MacrosPlugin.NotMain":      false,
	}
	seen := map[string]bool{}
	for _, f := range ff {
		w, ok := want[f.Name]
		if !ok {
			continue
		}
		seen[f.Name] = true
		if got := f.PropBool("framework_registered"); got != w {
			t.Errorf("%s: framework_registered=%v, want %v", f.Name, got, w)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("no fact %s", name)
		}
	}
}

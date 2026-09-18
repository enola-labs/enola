package swiftextractor

import (
	"testing"
)

// TestOverrideModifier_EmitsOverrideProp asserts that a method declared with the
// `override` modifier carries override=true, so the dead-code detector
// excludes framework lifecycle callbacks (viewDidLoad, viewWillAppear, …) that are
// dispatched polymorphically and never called by their own literal name. GAP-SW-01.
func TestOverrideModifier_EmitsOverrideProp(t *testing.T) {
	ff := extractAST(t, `
class Base {
    func viewDidLoad() {}
}
final class Screen: Base {
    override func viewDidLoad() {}
}
`, false)

	f, ok := findFact(ff, "pkg.Screen.viewDidLoad")
	if !ok {
		t.Fatal("expected fact for pkg.Screen.viewDidLoad")
	}
	if v, _ := f.Props["override"].(bool); !v {
		t.Errorf("override method should carry override=true; props=%v", f.Props)
	}
}

// TestOverrideModifier_AbsentWithoutModifier asserts the prop is not set for a
// method with no `override` modifier — it must be a positive signal only.
func TestOverrideModifier_AbsentWithoutModifier(t *testing.T) {
	ff := extractAST(t, `
class Base {
    func plain() {}
}
`, false)

	f, ok := findFact(ff, "pkg.Base.plain")
	if !ok {
		t.Fatal("expected fact for pkg.Base.plain")
	}
	if _, present := f.Props["override"]; present {
		t.Errorf("non-override method must not carry override prop; props=%v", f.Props)
	}
}

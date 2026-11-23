package swiftextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A #selector names the method the Objective-C runtime will send a message to:
// target-action, a gesture recognizer, a notification observer. It is a use of
// that method, recorded the way a method value is.
func TestSelectorRecordsUse(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/vc.swift": `
final class DashboardViewController: UIViewController {
    func setup() {
        button.addTarget(self, action: #selector(self.savedTapped), for: .touchUpInside)
        let g = UITapGestureRecognizer(target: self, action: #selector(DashboardViewController.viewTapped))
        other.addTarget(self, action: #selector(self.save(_:)), for: .valueChanged)
        center.addObserver(self, selector: #selector(refresh), name: n, object: nil)
        switch action {
        case #selector(copyText(_:)):
            break
        default:
            break
        }
    }
    @objc func savedTapped() {}
    @objc func viewTapped() {}
    @objc func save(_ sender: Any) {}
    @objc func refresh() {}
    @objc func copyText(_ sender: Any) {}
    @objc func unused() {}
    var title: String = ""
    func read() { _ = #selector(getter: UIView.frame) }
}
`,
	})
	got := valueCallTargets(t, ff, "Sources/App.DashboardViewController.setup")
	for _, want := range []string{"savedTapped", "viewTapped", "save", "refresh", "copyText"} {
		if !slices.Contains(got, "Sources/App.DashboardViewController."+want) {
			t.Errorf("setup: calls %v, want %s", got, want)
		}
	}
	if slices.Contains(got, "Sources/App.DashboardViewController.unused") {
		t.Errorf("setup: calls %v, must not reach unused", got)
	}
	if got := valueCallTargets(t, ff, "Sources/App.DashboardViewController.read"); len(got) != 0 {
		t.Errorf("read: a getter selector names a property, got calls %v", got)
	}
}

// Interface Builder connects an @IBAction or @IBSegueAction by its selector from a
// storyboard or nib, and @objc(name:) gives a method the runtime name a string
// selector or Objective-C caller uses. Each is entered by the runtime. A bare @objc
// only makes a method reachable; it is not evidence that anything does.
func TestObjCRuntimeEntryAttributes(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/vc.swift": `
final class SettingsViewController: UIViewController {
    @IBAction func didEndEditing(_ sender: UITextField) {}
    @IBSegueAction func makeDetail(_ coder: NSCoder) -> UIViewController? { nil }
    @objc(doThing:) func thing(_ x: Int) {}
    @objc func plain() {}
    @objc private func plainPrivate() {}
    func helper() {}
}
`,
	})
	byName := map[string]facts.Fact{}
	for _, f := range ff {
		byName[f.Name] = f
	}
	for name, want := range map[string]bool{
		"didEndEditing": true, "makeDetail": true, "thing": true,
		"plain": false, "plainPrivate": false, "helper": false,
	} {
		f, ok := byName["Sources/App.SettingsViewController."+name]
		if !ok {
			t.Fatalf("no fact for %s", name)
		}
		if got := f.PropBool("framework_registered"); got != want {
			t.Errorf("%s: framework_registered=%v, want %v", name, got, want)
		}
	}
}

package swiftextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestKotlinFacadeCallsAreNoted: a static call on a receiver named like a Kotlin
// file facade is listed on the caller for the kmp-swift binder; a call on any
// other type, or through a chain, is not.
func TestKotlinFacadeCallsAreNoted(t *testing.T) {
	ff := extractAST(t, `
import shared

struct ComposeView {
    func make() -> UIViewController {
        FirebaseApp.configure()
        Main_iosKt.doInitApp()
        IOSNotificationsKt.handleRemoteNotification(userInfo: info)
        Outer.InnerKt.helper()
        return Main_iosKt.MainViewController()
    }
}
`, true)
	f, ok := findFact(ff, "pkg.ComposeView.make")
	if !ok {
		t.Fatal("no ComposeView.make")
	}
	got, _ := f.PropAny(facts.PropKotlinFacadeCalls).([]string)
	want := []string{"IOSNotificationsKt.handleRemoteNotification", "Main_iosKt.MainViewController", "Main_iosKt.doInitApp"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", facts.PropKotlinFacadeCalls, got, want)
	}
}

package check

import (
	"os"
	"testing"
)

// TestMain sandboxes the home directory for the entire package test binary. Snapshots
// write the per-workspace receipt and history under ~/.enola/graphs and refresh
// ~/.enola/receipt.json, so without this every run left hundreds of files in the
// developer's real ~/.enola, and a real server restored a test's temp-dir graph.
//
// Individual tests may still call t.Setenv("HOME", ...) for their own temp dir; this
// is the backstop for any that do not. CI runs the suite under an empty HOME and fails
// if anything lands there.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "enola-check-test-home-")
	if err != nil {
		panic(err)
	}
	for _, key := range []string{
		"HOME",        // unix/darwin: os.UserHomeDir reads $HOME
		"USERPROFILE", // windows
	} {
		if err := os.Setenv(key, tmp); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

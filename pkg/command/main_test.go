package command

import (
	"os"
	"os/exec"
	"strings"
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
	// The Go caches stay put, as CI keeps them: under the sandboxed HOME every
	// run fetched the whole module cache again, and its read-only files made
	// the RemoveAll below fail, leaving about 150 MB in the temp dir per run.
	keepGoCaches()
	tmp, err := os.MkdirTemp("", "enola-command-test-home-")
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

func keepGoCaches() {
	keys := []string{"GOCACHE", "GOMODCACHE"}
	out, err := exec.Command("go", append([]string{"env"}, keys...)...).Output()
	if err != nil {
		return
	}
	values := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, key := range keys {
		if os.Getenv(key) == "" && i < len(values) && values[i] != "" {
			_ = os.Setenv(key, values[i])
		}
	}
}

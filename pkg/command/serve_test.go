package command

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTokens(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadTokenFile(t *testing.T) {
	good := strings.Repeat("a", minTokenLen)
	other := strings.Repeat("b", minTokenLen+8)

	tokens, err := readTokenFile(writeTokens(t, "# consumer one\n"+good+"\n\n  "+other+"  \n", 0o600))
	if err != nil || len(tokens) != 2 || tokens[0] != good || tokens[1] != other {
		t.Fatalf("readTokenFile = %v, %v", tokens, err)
	}

	for name, c := range map[string]struct {
		content string
		mode    os.FileMode
	}{
		"empty":          {"# nothing\n\n", 0o600},
		"short":          {"short-token\n", 0o600},
		"whitespace":     {strings.Repeat("a", 20) + " " + strings.Repeat("b", 20) + "\n", 0o600},
		"group readable": {good + "\n", 0o640},
		"world readable": {good + "\n", 0o644},
	} {
		if (name == "group readable" || name == "world readable") && runtime.GOOS == "windows" {
			continue
		}
		if _, err := readTokenFile(writeTokens(t, c.content, c.mode)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := readTokenFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: accepted")
	}
}

// serve refuses to start without its two required flags, and before listening on
// anything, so a half-configured service never opens a port.
func TestServe_RequiresRootAndTokenFile(t *testing.T) {
	tokens := writeTokens(t, strings.Repeat("a", minTokenLen)+"\n", 0o600)
	root := t.TempDir()
	for _, args := range [][]string{
		{},
		{"--root", root},
		{"--token-file", tokens},
		{"--root", filepath.Join(root, "missing"), "--token-file", tokens},
		{"--root", root, "--token-file", writeTokens(t, "x\n", 0o600)},
		{"--root", root, "--token-file", tokens, t.TempDir()}, // a graph to restore outside the root
	} {
		var stderr bytes.Buffer
		if code := testRunner().runServe(t.Context(), args, &stderr); code != 2 {
			t.Errorf("serve %v: exit %d, want 2\n%s", args, code, stderr.String())
		}
	}
}

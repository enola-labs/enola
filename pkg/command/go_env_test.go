package command

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSandboxKeepsGoEnvFile(t *testing.T) {
	got := os.Getenv("GOENV")
	if got == "" {
		t.Fatal("GOENV is not carried over, so `go env -w` settings are lost under the sandboxed HOME")
	}
	out, err := exec.Command("go", "env", "GOENV").Output()
	if err != nil {
		t.Skipf("go not runnable: %v", err)
	}
	if strings.TrimSpace(string(out)) != got {
		t.Fatalf("go resolves GOENV to %q, want %q", out, got)
	}
}

package facts

import "testing"

// A Go package is named by its import path, and its module path is no location: a
// module called example.com/e2e does not make every package in it test code, while a
// package in a test directory still is one.
func TestModulePath_GoPackageIsItsDirectory(t *testing.T) {
	goPkg := func(name, file string) Fact {
		return Fact{Kind: KindModule, Name: name, File: file, Repo: "r", Props: map[string]any{"language": "go"}}
	}
	for _, c := range []struct {
		m        Fact
		path     string
		testCode bool
	}{
		{goPkg("example.com/e2e/a", "a"), "a", false},
		{goPkg("github.com/acme/testing/svc", "svc"), "svc", false},
		{goPkg("example.com/app/internal/e2e", "internal/e2e"), "internal/e2e", true},
		{goPkg("example.com/app", "."), ".", false},
		{Fact{Kind: KindModule, Name: "app/tests", File: "app/tests", Props: map[string]any{"language": "python"}}, "app/tests", true},
		{Fact{Kind: KindModule, Name: "billing", File: "app/domain/billing", Props: map[string]any{"language": "ruby"}}, "billing", false},
	} {
		if got := ModulePath(c.m); got != c.path {
			t.Errorf("ModulePath(%s) = %q, want %q", c.m.Name, got, c.path)
		}
		if got := IsTestPath(ModulePath(c.m)); got != c.testCode {
			t.Errorf("%s judged test code = %v, want %v", c.m.Name, got, c.testCode)
		}
	}
	prefixed := goPkg("example.com/e2e/a", "r/a")
	prefixed.RepoPrefixed = true
	paths := NewModulePaths([]Fact{prefixed})
	if got := paths.Of("example.com/e2e/a"); got != "a" {
		t.Errorf("ModulePaths.Of = %q, want the repo-stripped directory", got)
	}
	// A repository labelled like a directory it holds: r/a is that directory, not a
	// label a multi-repo snapshot put there.
	if got := ModulePath(goPkg("example.com/e2e/r/a", "r/a")); got != "r/a" {
		t.Errorf("ModulePath of an unprefixed r/a in repo r = %q, want r/a", got)
	}
	if got := paths.Of("other"); got != "other" {
		t.Errorf("ModulePaths.Of(other) = %q, want the name itself", got)
	}
}

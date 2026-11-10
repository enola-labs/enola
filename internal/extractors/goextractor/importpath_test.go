package goextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractRaw extracts files without dirNamed, so the import-path names are tested as
// the extractor emits them.
func extractRaw(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := setupGoProject(t, files)
	var rels []string
	for f := range files {
		rels = append(rels, f)
	}
	sort.Strings(rels)
	ff, err := New().Extract(context.Background(), dir, rels)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func moduleFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Kind == facts.KindModule && f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// Packages and their symbols are named by import path: the module path its go.mod
// declares, joined with the package's place in the module. Two repositories that both
// have an internal/config are two packages, not one; another repository's import of a
// package names it the way the package names itself.
func TestPackagesAreNamedByImportPath(t *testing.T) {
	ff := extractRaw(t, map[string]string{
		"go.mod":                    "module example.com/app\n\ngo 1.22\n",
		"main.go":                   "package main\n\nimport \"example.com/app/internal/store\"\n\nfunc main() { store.Open() }\n",
		"internal/store/store.go":   "package store\n\nimport \"example.com/app/internal/config\"\n\nfunc Open() string { return config.Load() }\n",
		"internal/config/config.go": "package config\n\nfunc Load() string { return \"\" }\n",
	})
	for name, dir := range map[string]string{
		"example.com/app":                 ".",
		"example.com/app/internal/store":  "internal/store",
		"example.com/app/internal/config": "internal/config",
	} {
		m, ok := moduleFact(ff, name)
		if !ok {
			t.Errorf("no module %q", name)
			continue
		}
		if m.File != dir {
			t.Errorf("module %q: File = %q, want its directory %q", name, m.File, dir)
		}
	}
	open, ok := findFact(ff, "example.com/app/internal/store.Open")
	if !ok {
		t.Fatal("no symbol example.com/app/internal/store.Open")
	}
	if !hasRelation(open, facts.RelCalls, "example.com/app/internal/config.Load") {
		t.Errorf("Open does not call example.com/app/internal/config.Load: %v", open.Relations)
	}
	if !hasRelation(open, facts.RelDeclares, "example.com/app/internal/store") {
		t.Errorf("Open is not declared by its package: %v", open.Relations)
	}
	main, ok := findFact(ff, "example.com/app.main")
	if !ok {
		t.Fatal("the root package's main is not named example.com/app.main")
	}
	if !hasRelation(main, facts.RelCalls, "example.com/app/internal/store.Open") {
		t.Errorf("main does not call example.com/app/internal/store.Open: %v", main.Relations)
	}
	var imports bool
	for _, f := range ff {
		if f.Kind == facts.KindDependency && hasRelation(f, facts.RelImports, "example.com/app/internal/config") {
			imports = true
		}
	}
	if !imports {
		t.Error("no import edge to example.com/app/internal/config")
	}
}

// A standard library package and one of the repository's that share a directory name
// are two names now: a call to log.Printf is the standard library's, not the
// repository's log package's, which a directory name could not tell apart.
func TestStandardLibraryImportIsNotTheRepositorysSameNamedPackage(t *testing.T) {
	ff := extractRaw(t, map[string]string{
		"go.mod":     "module example.com/app\n\ngo 1.22\n",
		"log/log.go": "package log\n\nfunc Printf(string, ...any) {}\n",
		"a/a.go":     "package a\n\nimport \"log\"\n\nfunc Run() { log.Printf(\"x\") }\n",
	})
	run, ok := findFact(ff, "example.com/app/a.Run")
	if !ok {
		t.Fatal("no symbol example.com/app/a.Run")
	}
	if hasRelation(run, facts.RelCalls, "example.com/app/log.Printf") {
		t.Errorf("a standard library call landed on the repository's log package: %v", run.Relations)
	}
}

// A nested module is named by its own path, and a package no go.mod covers keeps its
// directory name.
func TestNestedModuleAndModulelessPackageNames(t *testing.T) {
	ff := extractRaw(t, map[string]string{
		"go.mod":           "module example.com/app\n\ngo 1.22\n",
		"tools/go.mod":     "module example.com/tools\n\ngo 1.22\n",
		"tools/gen/gen.go": "package gen\n\nfunc Gen() {}\n",
		"svc/svc.go":       "package svc\n\nfunc Serve() {}\n",
	})
	for _, name := range []string{"example.com/tools/gen", "example.com/app/svc"} {
		if _, ok := moduleFact(ff, name); !ok {
			t.Errorf("no module %q", name)
		}
	}
	dir := t.TempDir() // no go.mod at all
	if err := os.MkdirAll(filepath.Join(dir, "loose"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loose", "loose.go"), []byte("package loose\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := New().Extract(context.Background(), dir, []string{"loose/loose.go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := moduleFact(got, "loose"); !ok {
		t.Error("a package outside any module lost its directory name")
	}
}

// A field's type is stored qualified by import path, and a host's first label is often
// an import alias too: go-github is imported as github. Reading the stored type must
// not split it at its first dot and graft it onto that import.
func TestQualifiedFieldTypeIsNotReadAsAnImportAlias(t *testing.T) {
	ff := extractRaw(t, map[string]string{
		"go.mod":         "module github.com/acme/app\n\ngo 1.22\n",
		"conn/config.go": "package conn\n\ntype Config struct{}\n\nfunc (c Config) AppID() string { return \"\" }\n",
		"conn/conn.go": `package conn

import github "github.com/google/go-github/v82/github"

type Connection struct {
	cfg    Config
	client *github.Client
}

func (c *Connection) Token() string { return c.cfg.AppID() }
`,
	})
	tok, ok := findFact(ff, "github.com/acme/app/conn.Connection.Token")
	if !ok {
		t.Fatal("no symbol Connection.Token")
	}
	if !hasRelation(tok, facts.RelCalls, "github.com/acme/app/conn.Config.AppID") {
		t.Errorf("c.cfg.AppID() did not resolve through the field's qualified type: %v", tok.Relations)
	}
	for _, r := range tok.Relations {
		if strings.HasPrefix(r.Target, "github.com/google/go-github/v82/github.com/") {
			t.Errorf("a qualified type was grafted onto the github import: %s", r.Target)
		}
	}
}

package goextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// callTargetsOf returns the calls edges of the named symbol fact.
func callTargetsOf(ff []facts.Fact, name string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind != facts.KindSymbol || f.Name != name {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				out = append(out, r.Target)
			}
		}
	}
	return out
}

// moduleCallTargets returns the calls edges hanging on a package fact.
func moduleCallTargets(ff []facts.Fact, pkgDir string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind != facts.KindModule || f.Name != pkgDir {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				out = append(out, r.Target)
			}
		}
	}
	return out
}

func hasTarget(targets []string, want string) bool {
	for _, t := range targets {
		if t == want {
			return true
		}
	}
	return false
}

// A function named in a package-level dispatch table is referenced by that table. The
// table is usually unexported, so it has no symbol of its own and the edge belongs to
// the package. Before this, every parser reachable only through such a map read as
// dead code at high confidence.
func TestPackageLevelDispatchTableReferencesItsFuncs(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/reader.go": `package reader

var manifestReaders = map[string]func(string) []string{
	"go.mod":       readGoMod,
	"package.json": readPackageJSON,
}

func readGoMod(f string) []string       { return nil }
func readPackageJSON(f string) []string { return nil }
`})

	targets := moduleCallTargets(ff, "pkg")
	for _, want := range []string{"pkg.readGoMod", "pkg.readPackageJSON"} {
		if !hasTarget(targets, want) {
			t.Errorf("package fact missing %q; has %v", want, targets)
		}
	}
}

// A package-level initializer that CALLS a function is a reference too. Nothing walked
// these expressions at all, so `var repoRoot = findRepoRoot()` left findRepoRoot with
// no incoming edge even though the call is plain.
func TestPackageLevelInitializerCallIsAReference(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/corpus.go": `package corpus

var repoRoot = findRepoRoot()

func findRepoRoot() string { return "" }
`})

	if targets := moduleCallTargets(ff, "pkg"); !hasTarget(targets, "pkg.findRepoRoot") {
		t.Errorf("package fact missing pkg.findRepoRoot; has %v", targets)
	}
}

// A function passed as an argument, or stored in a struct field inside a body, is used.
// analyzeBody only records CallExpr, so these had no edge.
func TestFuncValueInBodyIsReferenced(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/hooks.go": `package hooks

type pattern struct {
	name  string
	parse func(string) []string
}

func install() {
	writeHooks(claudeHookEntry)
	pats := []pattern{{name: "SELECT", parse: selectTables}}
	_ = pats
}

func writeHooks(f func(string) map[string]any) {}
func claudeHookEntry(cmd string) map[string]any { return nil }
func selectTables(lit string) []string          { return nil }
`})

	targets := callTargetsOf(ff, "pkg.install")
	for _, want := range []string{"pkg.claudeHookEntry", "pkg.selectTables"} {
		if !hasTarget(targets, want) {
			t.Errorf("install missing %q; has %v", want, targets)
		}
	}
}

// A struct literal's KEY is a field name, not a reference. A package that happens to
// declare a function sharing a field's name must not gain an edge from `Side: source`.
func TestStructLiteralKeyIsNotAReference(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/forms.go": `package forms

type form struct {
	Side func() string
}

var forms = []form{{Side: sourceSide}}

func sourceSide() string { return "source" }
func Side() string       { return "never referenced" }
`})

	targets := moduleCallTargets(ff, "pkg")
	if !hasTarget(targets, "pkg.sourceSide") {
		t.Errorf("the field VALUE sourceSide should be referenced; has %v", targets)
	}
	if hasTarget(targets, "pkg.Side") {
		t.Errorf("the field NAME Side must not become a reference; has %v", targets)
	}
}

// A local variable shadowing a package function name is not a use of that function.
// The guard is conservative: any redeclaration of the name in the walked node
// suppresses every identifier of that name there, so a shadow costs an edge rather
// than inventing one.
func TestShadowedNameDoesNotBecomeAReference(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/shadow.go": `package shadow

func caller() {
	readGoMod := "not the function"
	_ = readGoMod
}

func readGoMod() string { return "" }
`})

	if targets := callTargetsOf(ff, "pkg.caller"); hasTarget(targets, "pkg.readGoMod") {
		t.Errorf("a shadowing local must not reference the function; has %v", targets)
	}
}

// The edge must carry the same qualification a plain call gets, or a value reference
// and a call to one function would land on two different graph nodes.
func TestFuncValueSharesQualificationWithACall(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/both.go": `package both

func direct() { helper() }

func indirect() { use(helper) }

func use(f func()) {}
func helper()      {}
`})

	fromCall := callTargetsOf(ff, "pkg.direct")
	fromValue := callTargetsOf(ff, "pkg.indirect")
	if !hasTarget(fromCall, "pkg.helper") {
		t.Fatalf("direct call lost: %v", fromCall)
	}
	if !hasTarget(fromValue, "pkg.helper") {
		t.Fatalf("value reference lost: %v", fromValue)
	}
}

// A function both called and used as a value in one body yields one edge, not two.
func TestCallAndValueInOneBodyEmitOneEdge(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/dup.go": `package dup

func caller() {
	helper()
	use(helper)
}

func use(f func()) {}
func helper()      {}
`})

	n := 0
	for _, tgt := range callTargetsOf(ff, "pkg.caller") {
		if tgt == "pkg.helper" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("pkg.helper edge count = %d, want 1", n)
	}
}

// A cross-package function value is deliberately NOT resolved: it needs import
// resolution this pass does not attempt, and a missing edge beats a wrong one. This
// pins the limit so a later change to it is a decision rather than an accident.
func TestCrossPackageFuncValueIsNotResolved(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/cross.go": `package cross

import "other/install"

func wire() { use(install.Entry) }

func use(f func()) {}
`})

	for _, tgt := range callTargetsOf(ff, "pkg.wire") {
		if strings.HasSuffix(tgt, ".Entry") {
			t.Errorf("cross-package func value should stay unresolved, got %q", tgt)
		}
	}
}

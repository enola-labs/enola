package cppextractor

// A function-pointer typedef names a type, and the name is nested under declarator
// wrappers that carry no field tag: `typedef int (WINAPI *CompareFn)(int, int)` puts the
// pointer declarator inside a parenthesized_declarator with the macro on one side of it.
// Descending by the `declarator` field alone found no name there, so the typedef was
// missing and the *macro* was reported as the type instead — a symbol named `WINAPI`,
// `CALLBACK` or `_stdcall` that no source declares.
//
// Measured on a 3,274-file tree: 10 such phantom type rows disappeared from find_orphans
// and the typedefs they stand for became symbols.

import "testing"

func TestCppFunctionPointerTypedefIsTheTypeNotTheMacro(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"r/cb.c": `
#define WINAPI __stdcall
#define CALLBACK __stdcall

typedef int (WINAPI *CompareFn)(int a, int b);
typedef void (CALLBACK *NotifyFn)(int code);
int (WINAPI *g_compare)(int, int);
`,
	})

	ct := mustFact(t, ff, "r.CompareFn")
	if sk, _ := ct.Props["symbol_kind"].(string); sk != "type" {
		t.Errorf("CompareFn: symbol_kind = %q, want type", sk)
	}
	if _, ok := findFact(ff, "r.NotifyFn"); !ok {
		t.Error("NotifyFn: missing — the second parenthesized typedef must be extracted too")
	}

	// The macro is a spelling, not a declaration. It used to be the only "type" this
	// typedef produced, which is how `WINAPI` ended up in a dead-code report.
	for _, phantom := range []string{"r.WINAPI", "r.CALLBACK", "r._stdcall"} {
		if f, ok := findFact(ff, phantom); ok {
			t.Errorf("%s: phantom symbol emitted (kind %v) — a calling-convention macro is not a declared type", phantom, f.Props["symbol_kind"])
		}
	}

	// A function-pointer *variable* keeps its name for the same reason: the identifier
	// sits under the same untagged wrappers.
	if _, ok := findFact(ff, "r.g_compare"); !ok {
		t.Error("g_compare: missing — `int (WINAPI *g_compare)(int, int)` declares a variable")
	}
}

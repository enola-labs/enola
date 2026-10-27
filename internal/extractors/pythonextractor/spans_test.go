package pythonextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in: a class ends with its last method, a function with its body.
func TestSymbolSpans(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/store.py": `import os


class Store:
    def save(self):
        return os.getcwd()

    @property
    def size(self):
        return 2


def helper():
    return Store()
`,
		"app/routes.py": `from fastapi import APIRouter


def get_router():
    router = APIRouter()

    @router.get("/items")
    async def list_items():
        return []

    return router
`,
	})
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && (f.EndLine < f.Line || f.Line <= 0) {
			t.Errorf("%s: span %d-%d", f.Name, f.Line, f.EndLine)
		}
	}
	for suffix, want := range map[string][2]int{
		"Store":      {4, 10},
		"Store.save": {5, 6},
		"Store.size": {9, 10},
		"helper":     {13, 14},
		"get_router": {4, 11},
		// A handler a router factory declares is a symbol of its own.
		"get_router.list_items": {8, 9},
	} {
		f := pySym(t, ff, "."+suffix)
		if got := [2]int{f.Line, f.EndLine}; got != want {
			t.Errorf("%s: span %v, want %v", suffix, got, want)
		}
	}
}

package pythonextractor

import (
	"strings"
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

// A line in a callback belongs to the callback, not to the method around it. The calls stay
// the method's, so the graph is what it was without these symbols.
func TestNestedFunctionsAreSymbolsOfTheirOwn(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/client.py": `def close_it():
    return 1


class Client:
    def send(self):
        def on_close(response):
            def inner():
                return close_it()
            return inner()

        class Local:
            def method(self):
                return 1

        return on_close


def plain():
    def wrapper():
        return 1
    return wrapper
`,
	})
	byName := map[string]facts.Fact{}
	count := map[string]int{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			byName[f.Name] = f
			count[f.Name]++
		}
	}
	for name, want := range map[string]struct {
		line, end int
		parent    string
	}{
		"app/client.Client.send.on_close":       {7, 10, "app/client.Client.send"},
		"app/client.Client.send.on_close.inner": {8, 9, "app/client.Client.send"},
		"app/client.Client.send.Local.method":   {13, 14, "app/client.Client.send"},
		"app/client.plain.wrapper":              {20, 21, "app/client.plain"},
	} {
		f, ok := byName[name]
		if !ok {
			t.Errorf("no symbol %s among %v", name, count)
			continue
		}
		if count[name] != 1 {
			t.Errorf("%s emitted %d times", name, count[name])
		}
		if f.Line != want.line || f.EndLine != want.end || f.PropString(facts.PropMemberOf) != want.parent {
			t.Errorf("%s: %d-%d member_of %q, want %+v", name, f.Line, f.EndLine, f.PropString(facts.PropMemberOf), want)
		}
		if len(f.Relations) != 1 || f.Relations[0].Kind != facts.RelDeclares {
			t.Errorf("%s has edges: %+v", name, f.Relations)
		}
	}
	calls := false
	for _, r := range byName["app/client.Client.send"].Relations {
		calls = calls || (r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, "close_it"))
	}
	if !calls {
		t.Error("the method lost the calls its nested functions make")
	}
}

package pythonextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func handledByOf(ff []facts.Fact, path string) (string, bool) {
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.Name != path {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				return r.Target, true
			}
		}
		return "", true
	}
	return "", false
}

func symbolNames(ff []facts.Fact) map[string]bool {
	out := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			out[f.Name] = true
		}
	}
	return out
}

// A decorated function and a call-registered one defined later in the file both
// bind; a handler imported from elsewhere does not, since this file declares no
// symbol under that name.
func TestRouteHandlers_HandledByDeclaredSymbols(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/api.py": `from fastapi import APIRouter
from app.other import imported_handler

router = APIRouter()

@router.get("/items/{item_id}")
def read_item(item_id: int): ...

router.add_api_route("/later", defined_later, methods=["GET"])
router.add_api_route("/imported", imported_handler, methods=["GET"])

def defined_later(): ...
`,
	})
	symbols := symbolNames(ff)
	for path, want := range map[string]string{
		"/items/{item_id}": "app/api.read_item",
		"/later":           "app/api.defined_later",
	} {
		got, found := handledByOf(ff, path)
		if !found {
			t.Fatalf("route %s not emitted", path)
		}
		if got != want || !symbols[got] {
			t.Errorf("%s handled_by = %q, want %q (an emitted symbol)", path, got, want)
		}
	}
	if got, found := handledByOf(ff, "/imported"); !found || got != "" {
		t.Errorf("/imported: found=%v handled_by=%q, want the route with no edge", found, got)
	}
}

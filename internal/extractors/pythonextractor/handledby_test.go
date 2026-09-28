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

// A route handler defined inside a router factory becomes a symbol under the
// factory, the route names it, and the calls in its body are its own. A nested def
// that is not a route handler stays part of the factory.
func TestRouteHandlers_FactoryNestedHandlerIsASymbol(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"app/routers.py": `from fastapi import APIRouter

def load(): ...
def audit(): ...

def get_items_router() -> APIRouter:
    router = APIRouter()

    def local_helper():
        audit()

    @router.get("/items")
    async def list_items():
        load()

    return router
`,
	})
	const handler = "app/routers.get_items_router.list_items"
	var sym *facts.Fact
	for i := range ff {
		if ff[i].Kind == facts.KindSymbol && ff[i].Name == handler {
			sym = &ff[i]
		}
		if ff[i].Kind == facts.KindSymbol && ff[i].Name == "app/routers.get_items_router.local_helper" {
			t.Errorf("a nested def with no route became a symbol")
		}
	}
	if sym == nil {
		t.Fatalf("no symbol %s", handler)
	}
	if sym.Props["web_component"] != "route_handler" || sym.Props["async"] != true {
		t.Errorf("handler props = %v, want route_handler and async", sym.Props)
	}
	if !sym.HasRelation(facts.RelCalls, "app/routers.load") {
		t.Errorf("the handler's call is not its own: %+v", sym.Relations)
	}
	if got, _ := handledByOf(ff, "/items"); got != handler {
		t.Errorf("/items handled_by = %q, want %q", got, handler)
	}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == "app/routers.get_items_router" {
			if f.HasRelation(facts.RelCalls, "app/routers.load") {
				t.Errorf("the factory still claims the handler's call")
			}
			if !f.HasRelation(facts.RelCalls, "app/routers.audit") {
				t.Errorf("the factory lost the non-route nested def's call: %+v", f.Relations)
			}
		}
	}
}

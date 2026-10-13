package pythonextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestPythonDoc_FunctionsMethodsAndClasses(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"shop/order.py": `class Cart:
    """What a customer picked.

    Holds lines.
    """

    def total(self):
        '''Sum the cart.'''
        return 0

    def bare(self):
        return 1


def place(cart):
    # a comment before the docstring does not hide it
    r"""
    Place an order
    and charge for it.

    Args:
        cart: the cart.
    """
    return cart.total()


async def later():
    "Run after the response."


def not_a_docstring():
    x = 1
    """a string that is not first"""
    return x


def calls_first():
    print("no docstring here")
`,
	})
	for suffix, want := range map[string]string{
		"order.Cart":            "What a customer picked.",
		"order.Cart.total":      "Sum the cart.",
		"order.Cart.bare":       "",
		"order.place":           "Place an order and charge for it.",
		"order.later":           "Run after the response.",
		"order.not_a_docstring": "",
		"order.calls_first":     "",
	} {
		f := pySym(t, ff, suffix)
		if got := f.PropString(facts.PropDoc); got != want {
			t.Errorf("%s: doc = %q, want %q", suffix, got, want)
		}
		if _, has := f.Prop(facts.PropDoc); want == "" && has {
			t.Errorf("%s: a symbol with no docstring must carry no doc prop", suffix)
		}
	}
}

// A handler defined inside a router factory is emitted on its own path, and is
// the symbol a route's flow starts from.
func TestPythonDoc_NestedRouteHandler(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"requirements.txt": "fastapi\n",
		"api/routers.py": `from fastapi import APIRouter


def get_recall_router():
    router = APIRouter()

    @router.post("/recall")
    async def recall(payload: dict):
        """Recall what was stored."""
        return payload

    return router
`,
	})
	f := pySym(t, ff, "get_recall_router.recall")
	if got := f.PropString(facts.PropDoc); got != "Recall what was stored." {
		t.Errorf("doc = %q", got)
	}
}

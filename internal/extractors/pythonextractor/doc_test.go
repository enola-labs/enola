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

// A package's doc is its __init__.py module docstring. A comment may come before
// it, as the interpreter allows; a plain module's docstring is no package's doc.
func TestPythonDoc_PackageDocstring(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"shop/__init__.py": "# -*- coding: utf-8 -*-\n\"\"\"Orders and payments.\n\nMore detail.\n\"\"\"\nfrom .orders import place\n",
		"shop/orders.py":   "\"\"\"Not the package's doc.\"\"\"\ndef place():\n    pass\n",
		"late/__init__.py": "import os\n\"\"\"Too late to be a docstring.\"\"\"\n",
		"plain/tool.py":    "\"\"\"A module with no package.\"\"\"\n",
	})
	for dir, want := range map[string]string{
		"shop":  "Orders and payments.",
		"late":  "",
		"plain": "",
	} {
		var mod *facts.Fact
		for i := range ff {
			if ff[i].Kind == facts.KindModule && ff[i].Name == dir {
				mod = &ff[i]
			}
		}
		if mod == nil {
			t.Errorf("no module fact %s", dir)
			continue
		}
		if got := mod.PropString(facts.PropDoc); got != want {
			t.Errorf("%s: doc = %q, want %q", dir, got, want)
		}
		if _, has := mod.Prop(facts.PropDoc); want == "" && has {
			t.Errorf("%s: an undocumented package must carry no doc prop", dir)
		}
	}
}

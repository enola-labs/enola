package pythonextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func namesOf(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelNames {
			out = append(out, r.Target)
		}
	}
	slices.Sort(out)
	return out
}

// A type a signature or a field names is used, though nothing calls it: a request
// model a route takes, the record a function returns, a model nested in another.
func TestAnnotations_NameTheTypesTheyUse(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"shop/models.py": `
from dataclasses import dataclass

@dataclass
class Line:
    sku: str

@dataclass
class Order:
    lines: list[Line]
    note: "Note | None" = None

class Note:
    text: str
`,
		"shop/api.py": `
from typing import Optional, TYPE_CHECKING
from shop import models
from shop.models import Order

if TYPE_CHECKING:
    from shop.models import Note

def place(order: Order, note: Optional["Note"] = None) -> models.Line:
    return order.lines[0]
`,
	})
	if got := namesOf(pySym(t, ff, "api.place")); !slices.Equal(got, []string{"shop/models.Line", "shop/models.Note", "shop/models.Order"}) {
		t.Errorf("place names %v", got)
	}
	if got := namesOf(pySym(t, ff, "models.Order")); !slices.Equal(got, []string{"shop/models.Line", "shop/models.Note"}) {
		t.Errorf("Order names %v", got)
	}
}

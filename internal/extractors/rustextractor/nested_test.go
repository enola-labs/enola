package rustextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A let-bound closure inside a function is listed on it. A nested fn item that is a symbol
// already is not, and neither is an unbound closure.
func TestNestedClosuresBoundToANameAreListed(t *testing.T) {
	ff := extractRustRepo(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"s\"\n",
		"src/lib.rs": `pub fn send() -> i32 {
    fn on_close() -> i32 {
        1
    }
    let on_open = |x: i32| {
        x + 1
    };
    let _ = [1].iter().map(|x| x + 1);
    on_open(on_close())
}
`,
	})
	nested := map[string][]string{}
	for i := range ff {
		if ff[i].Kind == facts.KindSymbol {
			for _, n := range facts.NestedOf(&ff[i]) {
				nested[ff[i].Name] = append(nested[ff[i].Name], n.Name)
			}
		}
	}
	var holder string
	for name, list := range nested {
		if slices.Contains(list, "on_open") {
			holder = name
		}
		if slices.Contains(list, "on_close") {
			t.Errorf("%s lists on_close, which is a symbol of its own: %v", name, list)
		}
	}
	if holder == "" || !slices.Equal(nested[holder], []string{"on_open"}) {
		t.Errorf("nested = %v, want on_open on send", nested)
	}
}

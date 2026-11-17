package pythonextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractPy(t *testing.T, file, src string) []facts.Fact {
	t.Helper()
	return extractRepo(t, map[string]string{file: src})
}

func findSym(ff []facts.Fact, short string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && (f.Name == short || strings.HasSuffix(f.Name, "."+short)) {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// A behave step definition is registered by its decorator and matched to a
// feature file's step text; a function decorated @given in a file that does not
// import a BDD runner is not one.
func TestBDDStepsAreFrameworkRegistered(t *testing.T) {
	steps := extractPy(t, "features/steps/orders.py", `from behave import given, then

@given("an order")
def given_an_order(context):
    context.order = 1

@then('the total is "{total}"')
def then_total(context, total):
    assert context.order

def helper():
    return 1
`)
	plain := extractPy(t, "app/rules.py", `from app.registry import given

@given("x")
def rule():
    return 1
`)
	for name, want := range map[string]bool{"given_an_order": true, "then_total": true, "helper": false} {
		f, ok := findSym(steps, name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if got := f.PropBool("framework_registered"); got != want {
			t.Errorf("%s framework_registered = %v, want %v", name, got, want)
		}
	}
	if f, ok := findSym(plain, "rule"); ok && f.PropBool("framework_registered") {
		t.Error("@given without a BDD import was read as a step")
	}
}

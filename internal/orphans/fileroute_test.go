package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A route named by its file (a SvelteKit +page/+layout/+error component, a form
// action) carries no handler prop: its only binding is handled_by. The framework
// renders or invokes the target, so it is not dead code.
func TestFileRouteHandledByIsNotDeadCode(t *testing.T) {
	sym := func(name, file string, rels ...facts.Relation) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file,
			Props:     map[string]any{"symbol_kind": facts.SymbolFunc, "language": "typescript", "exported": true, "web_component": "component", "framework": "sveltekit"},
			Relations: append([]facts.Relation{{Kind: facts.RelDeclares, Target: "src/routes"}}, rels...)}
	}
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/", File: "src/routes/+page.svelte",
			Props:     map[string]any{"method": "GET", "type": "page", "router": "sveltekit", "framework": "sveltekit"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "src/routes.RoutesPage"}}},
		facts.Fact{Kind: facts.KindRoute, Name: "/", File: "src/routes/+error.svelte",
			Props:     map[string]any{"method": "GET", "type": "error", "router": "sveltekit", "framework": "sveltekit"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "src/routes.RoutesError"}}},
		facts.Fact{Kind: facts.KindRoute, Name: "/login", File: "src/routes/login/+page.server.js",
			Props:     map[string]any{"method": "POST", "router": "sveltekit", "framework": "sveltekit"},
			Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: "src/routes/login.actions.default"}}},
		sym("src/routes.RoutesPage", "src/routes/+page.svelte"),
		sym("src/routes.RoutesError", "src/routes/+error.svelte"),
		sym("src/routes/login.actions.default", "src/routes/login/+page.server.js"),
		sym("src/lib.Unused", "src/lib/Unused.svelte"),
	)
	st.BuildGraph()
	reported := map[string]bool{}
	for _, o := range Detect(st) {
		reported[o.Name] = true
	}
	for _, name := range []string{"src/routes.RoutesPage", "src/routes.RoutesError", "src/routes/login.actions.default"} {
		if reported[name] {
			t.Errorf("%s is reported as an orphan, but a route is handled by it", name)
		}
	}
	if !reported["src/lib.Unused"] {
		t.Errorf("src/lib.Unused is not reported; the test no longer detects orphans")
	}
}

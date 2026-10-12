package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// phpWiringStore is a Laravel-shaped chain as the PHP extractor emits it: a route
// bound to a controller action, a service the controller's constructor takes, a
// model reached only through a framework method, and a base controller.
func phpWiringStore(extra ...facts.Fact) *facts.Store {
	sym := func(name, kind, file string, rels ...facts.Relation) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file,
			Props:     map[string]any{"symbol_kind": kind, "language": "php", "exported": true},
			Relations: append([]facts.Relation{{Kind: facts.RelDeclares, Target: "app"}}, rels...)}
	}
	rel := func(kind, target string) facts.Relation { return facts.Relation{Kind: kind, Target: target} }
	const (
		controller = `App\Http\Controllers\OrderController`
		base       = `App\Http\Controllers\Controller`
		service    = `App\Services\OrderService`
		model      = `App\Models\Order`
	)
	st := facts.NewStore()
	st.Add(
		facts.Fact{Kind: facts.KindRoute, Name: "/orders", File: "routes/api.php",
			Props:     map[string]any{"method": "POST", "handler": controller + "::store"},
			Relations: []facts.Relation{rel(facts.RelHandledBy, controller+"::store")}},
		facts.Fact{Kind: facts.KindRoute, Name: "/orders/{id}/audit", File: "routes/api.php",
			Props:     map[string]any{"method": "GET", "handler": controller + "::audit"},
			Relations: []facts.Relation{rel(facts.RelHandledBy, base+"::audit")}},
		sym(controller, facts.SymbolClass, "app/Http/Controllers/OrderController.php",
			rel(facts.RelImplements, base), rel(facts.RelInjects, service)),
		sym(controller+"::__construct", facts.SymbolMethod, "app/Http/Controllers/OrderController.php"),
		sym(controller+"::store", facts.SymbolMethod, "app/Http/Controllers/OrderController.php",
			rel(facts.RelCalls, service+"::place"), rel(facts.RelCalls, "place")),
		sym(base, facts.SymbolClass, "app/Http/Controllers/Controller.php"),
		sym(base+"::audit", facts.SymbolMethod, "app/Http/Controllers/Controller.php"),
		sym(service, facts.SymbolClass, "app/Services/OrderService.php"),
		sym(service+"::place", facts.SymbolMethod, "app/Services/OrderService.php",
			rel(facts.RelCalls, model+"::query")),
		sym(model, facts.SymbolClass, "app/Models/Order.php",
			rel(facts.RelImplements, `Illuminate\Database\Eloquent\Model`)),
		sym(`App\Services\Unused`, facts.SymbolClass, "app/Services/Unused.php"),
		sym(`App\Services\Unused::run`, facts.SymbolMethod, "app/Services/Unused.php"),
	)
	st.Add(extra...)
	st.BuildGraph()
	return st
}

func TestPHPWiringIsNotDeadCode(t *testing.T) {
	reported := map[string]bool{}
	for _, o := range Detect(phpWiringStore()) {
		reported[o.Name] = true
	}
	for name, why := range map[string]string{
		`App\Http\Controllers\OrderController`:              "a route's handler names it",
		`App\Http\Controllers\OrderController::store`:       "a route is bound to it",
		`App\Http\Controllers\OrderController::__construct`: "PHP calls it",
		`App\Http\Controllers\Controller`:                   "a class extends it",
		`App\Http\Controllers\Controller::audit`:            "a route is bound to it through the subclass",
		`App\Services\OrderService`:                         "a constructor takes it",
		`App\Services\OrderService::place`:                  "it is called",
		`App\Models\Order`:                                  "a framework method is called on it",
	} {
		if reported[name] {
			t.Errorf("%s is reported as dead code, but %s", name, why)
		}
	}
	// The change adds references and must not hide what nothing uses.
	for _, name := range []string{`App\Services\Unused`, `App\Services\Unused::run`} {
		if !reported[name] {
			t.Errorf("%s is used by nothing and is no longer reported", name)
		}
	}
}

func TestScopedReceiverRefs(t *testing.T) {
	for target, want := range map[string]string{
		`App\Models\Order::query`: `App\Models\Order`,
		`Message::Error`:          `Message`,
		`Ns::Class.method`:        `Ns::Class`,
	} {
		got := constReceiverRefs(target)
		if len(got) == 0 || got[0] != want {
			t.Errorf("constReceiverRefs(%q) = %v, want %q first", target, got, want)
		}
	}
	for _, target := range []string{`$class::make`, `crate::util::parse`, `find`, `::`} {
		if got := constReceiverRefs(target); got != nil {
			t.Errorf("constReceiverRefs(%q) = %v, want none", target, got)
		}
	}
}

func TestPHPMagicMethodIsGatedOnTheLanguage(t *testing.T) {
	if !isPHPMagicMethod("app/A.php", `App\A::__construct`) || !isPHPMagicMethod("app/A.php", `App\A::__invoke`) {
		t.Error("a PHP constructor and __invoke are magic methods")
	}
	if isPHPMagicMethod("app/A.php", `App\A::construct`) || isPHPMagicMethod("src/a.cpp", `A::__construct`) {
		t.Error("an ordinary method, or the same name outside PHP, is not")
	}
}

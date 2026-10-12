package phpextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func laravelClass(name string, parents ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"symbol_kind": facts.SymbolClass}}
	for _, p := range parents {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: p})
	}
	return f
}

func laravelMethod(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Props: map[string]any{"symbol_kind": facts.SymbolMethod}}
}

func laravelRoute(path, handler string) facts.Fact {
	return facts.Fact{Kind: facts.KindRoute, Name: path, Props: map[string]any{
		"framework": "laravel", "method": "GET", "handler": handler}}
}

// boundHandler binds the routes among ff and returns the named route's handler prop
// and its handled_by targets.
func boundHandler(t *testing.T, ff []facts.Fact, path string) (string, []string) {
	t.Helper()
	bindLaravelHandlers(ff)
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.Name != path {
			continue
		}
		var targets []string
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelHandledBy {
				targets = append(targets, rel.Target)
			}
		}
		return f.PropString("handler"), targets
	}
	t.Fatalf("no route %s", path)
	return "", nil
}

func TestBindLaravelHandlers_ResolvesTheControllerClass(t *testing.T) {
	const want = `App\Http\Controllers\OrderController::show`
	for name, written := range map[string]string{
		"imported":             want,
		"string handler":       `OrderController::show`,
		"namespace suffix":     `Controllers\OrderController::show`,
		"leading backslash":    `\App\Http\Controllers\OrderController::show`,
		"string under a group": `Admin\UserController::index`,
	} {
		ff := []facts.Fact{
			laravelClass(`App\Http\Controllers\OrderController`),
			laravelMethod(`App\Http\Controllers\OrderController::show`),
			laravelClass(`App\Http\Controllers\Admin\UserController`),
			laravelMethod(`App\Http\Controllers\Admin\UserController::index`),
			// A second UserController elsewhere: the controller namespace wins.
			laravelClass(`App\Legacy\UserController`),
			laravelMethod(`App\Legacy\UserController::index`),
			laravelRoute("/x", written),
		}
		wantHandler := want
		if name == "string under a group" {
			wantHandler = `App\Http\Controllers\Admin\UserController::index`
		}
		handler, targets := boundHandler(t, ff, "/x")
		if handler != wantHandler {
			t.Errorf("%s: handler = %q, want %q", name, handler, wantHandler)
		}
		if len(targets) != 1 || targets[0] != wantHandler {
			t.Errorf("%s: handled_by = %v, want [%s]", name, targets, wantHandler)
		}
	}
}

func TestBindLaravelHandlers_AmbiguousOrUnknownClassIsLeftAlone(t *testing.T) {
	ff := []facts.Fact{
		laravelClass(`Billing\Http\ReportController`),
		laravelMethod(`Billing\Http\ReportController::index`),
		laravelClass(`Sales\Http\ReportController`),
		laravelMethod(`Sales\Http\ReportController::index`),
		laravelRoute("/ambiguous", `ReportController::index`),
		laravelRoute("/vendor", `Vendor\Pkg\ThingController::go`),
	}
	for _, path := range []string{"/ambiguous", "/vendor"} {
		written := map[string]string{"/ambiguous": `ReportController::index`, "/vendor": `Vendor\Pkg\ThingController::go`}[path]
		handler, targets := boundHandler(t, ff, path)
		if handler != written || len(targets) != 0 {
			t.Errorf("%s: handler = %q, handled_by = %v; want the handler as written and no edge", path, handler, targets)
		}
	}
}

func TestBindLaravelHandlers_InheritedActionBindsToItsDeclaration(t *testing.T) {
	base := laravelClass(`App\Http\Controllers\Controller`, `App\Concerns\LeavesTrail`)
	trait := laravelClass(`App\Concerns\LeavesTrail`)
	trait.Props["trait"] = true
	ff := []facts.Fact{
		// The symbol walk leaves a same-namespace parent bare.
		laravelClass(`App\Http\Controllers\OrderController`, `Controller`),
		base,
		laravelMethod(`App\Http\Controllers\Controller::audit`),
		trait,
		laravelMethod(`App\Concerns\LeavesTrail::trail`),
		laravelRoute("/audit", `OrderController::audit`),
		laravelRoute("/trail", `OrderController::trail`),
		laravelRoute("/framework", `OrderController::middleware`),
	}
	for path, want := range map[string]string{
		"/audit":     `App\Http\Controllers\Controller::audit`,
		"/trail":     `App\Concerns\LeavesTrail::trail`,
		"/framework": "",
	} {
		handler, targets := boundHandler(t, ff, path)
		if owner := `App\Http\Controllers\OrderController::`; len(handler) <= len(owner) || handler[:len(owner)] != owner {
			t.Errorf("%s: handler = %q, want it to keep the class the route names", path, handler)
		}
		switch {
		case want == "" && len(targets) != 0:
			t.Errorf("%s: handled_by = %v, want none for a method no class here declares", path, targets)
		case want != "" && (len(targets) != 1 || targets[0] != want):
			t.Errorf("%s: handled_by = %v, want [%s]", path, targets, want)
		}
	}
}

func TestBindLaravelHandlers_LeavesOtherFrameworksAlone(t *testing.T) {
	route := laravelRoute("/x", `OrderController::show`)
	route.Props["framework"] = "symfony"
	ff := []facts.Fact{
		laravelClass(`App\Http\Controllers\OrderController`),
		laravelMethod(`App\Http\Controllers\OrderController::show`),
		route,
	}
	if handler, targets := boundHandler(t, ff, "/x"); handler != `OrderController::show` || len(targets) != 0 {
		t.Errorf("handler = %q, handled_by = %v; want an untouched route", handler, targets)
	}
}

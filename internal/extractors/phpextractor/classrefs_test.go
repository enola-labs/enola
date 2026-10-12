package phpextractor

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// relationsOf returns the targets of one relation kind on the named symbol.
func relationsOf(t *testing.T, ff []facts.Fact, name, kind string) []string {
	t.Helper()
	for _, f := range ff {
		if f.Kind != facts.KindSymbol || f.Name != name {
			continue
		}
		var out []string
		for _, rel := range f.Relations {
			if rel.Kind == kind {
				out = append(out, rel.Target)
			}
		}
		sort.Strings(out)
		return out
	}
	t.Fatalf("no symbol %s", name)
	return nil
}

func wantTargets(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %q, want %q", what, got, want)
		}
	}
}

// TestClassReferencesResolveLikePHP: a class name no import covers is relative to
// the namespace it is written in, for a parent, an interface, a trait, a static
// call and a `new`.
func TestClassReferencesResolveLikePHP(t *testing.T) {
	src := `<?php
namespace App\Http\Controllers;

use App\Contracts\Auditable;
use Vendor\Support\Macroable;

class OrderController extends Controller implements Auditable, Responds
{
    use Macroable, LogsActivity;

    public function store($class)
    {
        Helper::run();
        \Globals::boot();
        parent::store($class);
        self::make();
        $class::make();
        return new Presenter(new \DateTime());
    }
}
`
	ff := extractFileAST([]byte(src), "app/Http/Controllers/OrderController.php")
	const class = `App\Http\Controllers\OrderController`
	wantTargets(t, "implements", relationsOf(t, ff, class, facts.RelImplements),
		`App\Http\Controllers\Controller`, `App\Contracts\Auditable`, `App\Http\Controllers\Responds`,
		`Vendor\Support\Macroable`, `App\Http\Controllers\LogsActivity`)
	wantTargets(t, "calls", relationsOf(t, ff, class+"::store", facts.RelCalls),
		`App\Http\Controllers\Helper::run`, `Globals::boot`,
		class+"::store", class+"::make", `$class::make`)
	wantTargets(t, "instantiates", relationsOf(t, ff, class+"::store", facts.RelInstantiates),
		`App\Http\Controllers\Presenter`, `DateTime`)
}

// TestGlobalNamespaceClassReferencesStayBare: with no namespace there is nothing to
// place a name under.
func TestGlobalNamespaceClassReferencesStayBare(t *testing.T) {
	src := `<?php
class Legacy extends Base { public function run() { Helper::go(); } }
`
	ff := extractFileAST([]byte(src), "lib/Legacy.php")
	wantTargets(t, "implements", relationsOf(t, ff, "Legacy", facts.RelImplements), "Base")
	wantTargets(t, "calls", relationsOf(t, ff, "Legacy::run", facts.RelCalls), "Helper::go")
}

// TestConstructorParametersAreInjected: each class a constructor takes is a
// dependency of the class, promoted or not, once.
func TestConstructorParametersAreInjected(t *testing.T) {
	src := `<?php
namespace App\Services;

use App\Repositories\OrderRepository;

class OrderService
{
    public function __construct(
        private OrderRepository $orders,
        ?PartnerGateway $gateway,
        OrderRepository $again,
        int $retries,
        self $parent,
        Clock|Timer $either,
    ) {
    }

    public function place(Mailer $mailer) {}
}

class Plain {}
`
	ff := extractFileAST([]byte(src), "app/Services/OrderService.php")
	wantTargets(t, "injects", relationsOf(t, ff, `App\Services\OrderService`, facts.RelInjects),
		`App\Repositories\OrderRepository`, `App\Services\PartnerGateway`)
	wantTargets(t, "injects", relationsOf(t, ff, `App\Services\Plain`, facts.RelInjects))
}

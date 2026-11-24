package phpextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestParameterTypesAreTypeUses: a class a method or function takes as a parameter
// is named by it, when the repository declares the class. A FormRequest nothing
// else names is how a Laravel controller action asks the container to validate the
// request, and a union type names each of its classes. Builtins, self, the
// method's own class and a framework class it does not declare name nothing.
func TestParameterTypesAreTypeUses(t *testing.T) {
	src := `<?php
namespace App\Http\Controllers;

use App\Http\Requests\StoreOrderRequest;
use App\Models\Order;

class OrderController extends Controller
{
    public function store(StoreOrderRequest $request, \Illuminate\Http\Request $raw, ?Order $order, int $n, self $same, OrderController $own)
    {
        return $n;
    }

    public function either(Card|\Billing\Iban $method, array $opts = [], mixed $m = null, object $o = null, iterable $it = [], callable $c = null, parent $p = null, bool|float|string|null $s = null, ...$rest) {}
}

function helper(Support\Clock $clock): void {}
`
	ff := extractFileAST([]byte(src), "app/Http/Controllers/OrderController.php")
	bindParameterTypes(ff, map[string]bool{`App\Http\Controllers\OrderController`: true,
		`App\Http\Requests\StoreOrderRequest`: true, `App\Models\Order`: true, `App\Http\Controllers\Card`: true,
		`Billing\Iban`: true, `App\Http\Controllers\Support\Clock`: true})
	for _, f := range ff {
		if _, ok := f.Props[propParamTypes]; ok {
			t.Fatalf("%s keeps %s", f.Name, propParamTypes)
		}
	}
	wantTargets(t, "store names", relationsOf(t, ff, `App\Http\Controllers\OrderController::store`, facts.RelNames),
		`App\Http\Requests\StoreOrderRequest`, `App\Models\Order`)
	wantTargets(t, "either names", relationsOf(t, ff, `App\Http\Controllers\OrderController::either`, facts.RelNames),
		`App\Http\Controllers\Card`, `Billing\Iban`)
	wantTargets(t, "helper names", relationsOf(t, ff, `App\Http\Controllers\helper`, facts.RelNames),
		`App\Http\Controllers\Support\Clock`)
}

package phpextractor

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// boundCalls runs the symbol walk over each source and the cross-file pass over the
// result, and returns the calls edges of the named symbol.
func boundCalls(t *testing.T, name string, sources ...string) []string {
	t.Helper()
	var all []facts.Fact
	for i, src := range sources {
		all = append(all, extractFileAST([]byte(src), "app/f"+string(rune('a'+i))+".php")...)
	}
	bindReceiverCalls(all)
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.Name != name {
			continue
		}
		if _, left := f.Props[propReceiverCalls]; left {
			t.Errorf("%s still carries %s after binding", name, propReceiverCalls)
		}
		var out []string
		for _, rel := range f.Relations {
			if rel.Kind == facts.RelCalls {
				out = append(out, rel.Target)
			}
		}
		sort.Strings(out)
		return out
	}
	t.Fatalf("no symbol %s", name)
	return nil
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %q, want %q", got, want)
		}
	}
}

const receiverService = `<?php
namespace App\Services;
class OrderService extends BaseService {
    public function find(int $id) { return $id; }
    public function create() { return 1; }
}
abstract class BaseService {
    public function log() {}
}
`

func TestReceiverCalls_PropertyTypes(t *testing.T) {
	for name, class := range map[string]string{
		"promoted": `class C {
    public function __construct(private readonly OrderService $orders) {}
    public function show() { return $this->orders->find(1); }
}`,
		"typed property": `class C {
    private ?OrderService $orders;
    public function show() { return $this->orders->find(1); }
}`,
		"constructor assignment": `class C {
    private $orders;
    public function show() { return $this->orders->find(1); }
    public function __construct(OrderService $orders) { $this->orders = $orders; }
}`,
	} {
		src := "<?php\nnamespace App\\Http\\Controllers;\nuse App\\Services\\OrderService;\n" + class
		got := boundCalls(t, `App\Http\Controllers\C::show`, src, receiverService)
		if len(got) != 2 || got[0] != `App\Services\OrderService::find` || got[1] != "find" {
			t.Errorf("%s: calls = %q, want the bare name and the service's method", name, got)
		}
	}
}

func TestReceiverCalls_ParameterThisAndInheritedMethod(t *testing.T) {
	src := `<?php
namespace App\Http\Controllers;
use App\Services\OrderService;
class C {
    public function show(OrderService $orders, $untyped, \Vendor\Client $client) {
        $orders->create();
        $orders->log();
        $untyped->find();
        $client->send();
        $orders->missing();
        return $this->helper();
    }
    private function helper() {}
}`
	wantCalls(t, boundCalls(t, `App\Http\Controllers\C::show`, src, receiverService),
		"create", "log", "find", "send", "missing", "helper",
		`App\Services\OrderService::create`,
		`App\Services\BaseService::log`,
		`App\Http\Controllers\C::helper`)
}

// TestReceiverCalls_SameNamespaceTypeNeedsNoImport: a class name no import covers is
// relative to the namespace it is written in.
func TestReceiverCalls_SameNamespaceTypeNeedsNoImport(t *testing.T) {
	src := `<?php
namespace App\Services;
class Checkout {
    public function __construct(private OrderService $orders) {}
    public function run() { return $this->orders->create(); }
}`
	wantCalls(t, boundCalls(t, `App\Services\Checkout::run`, src, receiverService),
		"create", `App\Services\OrderService::create`)
}

func TestReceiverCalls_InterfaceReachesItsOnlyImplementer(t *testing.T) {
	contracts := `<?php
namespace App\Contracts;
interface Orders { public function byId(int $id); }
interface Payments { public function charge(); }
`
	impls := `<?php
namespace App\Repositories;
use App\Contracts\Orders;
use App\Contracts\Payments;
class EloquentOrders implements Orders { public function byId(int $id) {} }
class Stripe implements Payments { public function charge() {} }
class Paypal implements Payments { public function charge() {} }
`
	src := `<?php
namespace App\Services;
use App\Contracts\Orders;
use App\Contracts\Payments;
class Checkout {
    public function __construct(private Orders $orders, private Payments $payments) {}
    public function run() { $this->orders->byId(1); return $this->payments->charge(); }
}`
	wantCalls(t, boundCalls(t, `App\Services\Checkout::run`, src, contracts, impls),
		"byId", "charge",
		`App\Contracts\Orders::byId`,
		`App\Repositories\EloquentOrders::byId`,
		// Two classes implement Payments, so only the interface's method is named.
		`App\Contracts\Payments::charge`)
}

// TestReceiverCalls_ObjectCreatedInPlace: `(new Action)->execute()` states its
// receiver's class in the expression itself.
func TestReceiverCalls_ObjectCreatedInPlace(t *testing.T) {
	src := `<?php
namespace App\Http\Controllers;
use App\Services\OrderService;
class C {
    public function destroy() {
        (new OrderService)->create();
        (new OrderService())->find(1);
        (new \Vendor\Mailer)->send();
        return (new $dynamic)->run();
    }
}`
	wantCalls(t, boundCalls(t, `App\Http\Controllers\C::destroy`, src, receiverService),
		"create", "find", "send", "run",
		`App\Services\OrderService::create`,
		`App\Services\OrderService::find`)
}

// TestReceiverCalls_FrameworkMethodOnAModel: a route-bound model is touched through
// methods Eloquent declares, not the repository. The same call on a plain class of
// the repository names nothing.
func TestReceiverCalls_FrameworkMethodOnAModel(t *testing.T) {
	models := `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Order extends Model { public function total() {} }
`
	src := `<?php
namespace App\Http\Controllers;
use App\Models\Order;
use App\Services\OrderService;
class C {
    public function destroy(Order $order, OrderService $orders) {
        $order->total();
        $order->delete();
        $orders->delete();
    }
}`
	wantCalls(t, boundCalls(t, `App\Http\Controllers\C::destroy`, src, models, receiverService),
		"total", "delete", `App\Models\Order::total`, `App\Models\Order::delete`)
}

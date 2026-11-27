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
	bindReceiverCalls(all, bindRelationReads(all))
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

// TestNullsafeMemberCallIsACall: `$obj?->m()` calls m as `$obj->m()` does, bare and
// through a declared receiver type.
func TestNullsafeMemberCallIsACall(t *testing.T) {
	got := boundCalls(t, `App\Services\Signer::sign`, `<?php
namespace App\Services;
class Summary {
    public function isValid() { return true; }
}
class Signer {
    public function sign(?Summary $parent, $other) {
        if ($parent?->isValid()) { return $other?->name(); }
        return $this?->sign(null, null);
    }
}
`)
	wantCalls(t, got, "isValid", `App\Services\Summary::isValid`, "name", "sign")
	ff := extractFileAST([]byte("<?php\nclass Tree { public function walk($n) { return $this?->walk($n->parent); } }\n"), "x.php")
	if !recursiveBySuffix(t, ff, "Tree::walk") {
		t.Error("Tree::walk calls itself through ?-> and is not flagged recursive_self")
	}
}

// TestReceiverCalls_EloquentQueryResult: a model fetched by a query
// (`Story::find($id)`, `Story::whereProfileId($pid)->firstOrFail()`), directly, through
// a local or as an element of a query's collection, is that model, and a method call
// on it runs the method the model, a parent or a trait declares. A collection or a
// query is no model, a class that is no model has no query, and a framework method
// (`save`) or a name no class declares binds nothing.
func TestReceiverCalls_EloquentQueryResult(t *testing.T) {
	got := boundCalls(t, `App\Http\Resources\AdminReport::toArray`, `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
trait HasSeen {
    public function markSeen() {}
}
abstract class BaseStory extends Model {
    public function archive() {}
}
class Story extends BaseStory {
    use HasSeen;
    public function toAdminEntity(): array { return []; }
    public function toSummary(): array { return []; }
    public function toList(): array { return []; }
    public function toQuery(): array { return []; }
    public function toEach(): array { return []; }
}
class Plain {
    public function describe() {}
}
`, `<?php
namespace App\Http\Resources;
use App\Models\Story;
use App\Models\Plain;
class AdminReport {
    public function toArray($id, $pid) {
        $story = Story::find($id);
        $res = $story->toAdminEntity();
        Story::whereProfileId($pid)->firstOrFail()->toSummary();
        $story->archive();
        $story->markSeen();
        $story->save();
        $story->missing();
        foreach (Story::where('active', true)->get() as $s) {
            $s->toEach();
        }
        Story::where('a', 1)->get()->toList();
        Story::where('a', 1)->toQuery();
        Plain::first()->describe();
        return $res;
    }
}
`)
	wantRelationCalls(t, got,
		[]string{`App\Models\Story::toAdminEntity`, `App\Models\Story::toSummary`, `App\Models\BaseStory::archive`,
			`App\Models\HasSeen::markSeen`, `App\Models\Story::toEach`},
		[]string{`App\Models\Story::toList`, `App\Models\Story::toQuery`, `App\Models\Plain::describe`,
			`App\Models\Story::save`, `App\Models\Story::missing`})
}

// TestReceiverCalls_ChainAsDeepAsAnEloquentResult: a declared-return chain is read
// to the depth eloquentResult reads one, so a fluent chain of eleven calls ending in
// an interface reaches the interface's method and its only implementer.
func TestReceiverCalls_ChainAsDeepAsAnEloquentResult(t *testing.T) {
	got := boundCalls(t, `App\Support\Chain::go`, `<?php
namespace App\Support;
interface Runner { public function run(); }
class Engine implements Runner { public function run() { return 1; } }
class Criteria {
    public function add(): Criteria { return $this; }
    public function runner(): Runner { return new Engine(); }
}
class Chain {
    public function go(Criteria $c) {
        return $c->add()->add()->add()->add()->add()->add()->add()->add()->add()->add()->runner()->run();
    }
}
`)
	wantRelationCalls(t, got, []string{`App\Support\Runner::run`, `App\Support\Engine::run`, `App\Support\Criteria::runner`}, nil)
}

// TestReceiverCalls_OnARelationReadInPlace: a call on a relation read with no local
// in between, `$story->author->foo()`, runs on the relation's model, as it does
// through a local. A to-many relation is a collection, a framework method binds
// nothing, and an untyped `$this->prop` keeps its member call.
func TestReceiverCalls_OnARelationReadInPlace(t *testing.T) {
	models := `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Story extends Model {
    public function author() { return $this->belongsTo(User::class); }
    public function comments() { return $this->hasMany(User::class); }
    public function authorName() { return $this->author->displayName(); }
}
class User extends Model {
    public function foo() { return 1; }
    public function bar() { return 1; }
    public function displayName() { return ''; }
}
`
	got := boundCalls(t, `App\Http\StoryController::show`, models, `<?php
namespace App\Http;
use App\Models\Story;
class StoryController {
    private $svc;
    public function show(Story $story) {
        $story->author->foo();
        $story?->author->bar();
        $story->comments->foo();
        $story->author->save();
        $this->svc->handle();
        return 1;
    }
}
`)
	wantRelationCalls(t, got, []string{`App\Models\User::foo`, `App\Models\User::bar`, `App\Models\Story::author`},
		[]string{`App\Models\User::save`})
	if n := countCalls(got, `App\Models\User::foo`); n != 1 {
		t.Errorf("User::foo bound %d times", n)
	}
	got = boundCalls(t, `App\Models\Story::authorName`, models)
	wantRelationCalls(t, got, []string{`App\Models\User::displayName`}, nil)
}

func countCalls(got []string, name string) int {
	n := 0
	for _, g := range got {
		if g == name {
			n++
		}
	}
	return n
}

// TestReceiverCalls_MemberCallOnUntypedPropertyIsKept: the in-place relation read
// leaves an untyped `$this->prop` call to the inherited-calls binder.
func TestReceiverCalls_MemberCallOnUntypedPropertyIsKept(t *testing.T) {
	all := extractFileAST([]byte(`<?php
namespace App\Http;
class C {
    private $svc;
    public function show() { $this->svc->handle(); }
}
`), "app/c.php")
	for _, f := range all {
		if f.Name == `App\Http\C::show` {
			if got := f.PropStrings(facts.PropMemberCalls); len(got) != 1 || got[0] != "svc.handle" {
				t.Fatalf("member calls = %q", got)
			}
			return
		}
	}
	t.Fatal("no C::show")
}

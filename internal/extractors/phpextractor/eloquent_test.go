package phpextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// eloquentFrom runs the symbol walk over each file and the cross-file pass over the
// result, as Extract does.
func eloquentFrom(files map[string]string) []facts.Fact {
	var all []facts.Fact
	for _, rel := range []string{"a.php", "b.php", "c.php", "d.php"} {
		if src, ok := files[rel]; ok {
			all = append(all, extractFileAST([]byte(src), "app/Models/"+rel)...)
		}
	}
	return eloquentFacts(all)
}

func modelTables(ff []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindStorage && f.PropString("storage_kind") == "model" {
			out[f.Name] = f.PropString("table") + " (" + f.PropString("table_source") + ")"
		}
	}
	return out
}

func TestEloquentModels_TableIsDeclaredOrDerived(t *testing.T) {
	got := modelTables(eloquentFrom(map[string]string{
		"a.php": `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
class Order extends Model { protected $table = 'shop_orders'; }
class OrderLine extends Model {}
class Category extends Model {}
class Address extends \Illuminate\Database\Eloquent\Model {}
`,
		"b.php": `<?php
namespace App\Models;
use Illuminate\Foundation\Auth\User as Authenticatable;
class User extends Authenticatable {}
`,
	}))
	want := map[string]string{
		`App\Models\Order`:     "shop_orders (declared)",
		`App\Models\OrderLine`: "order_lines (derived)",
		`App\Models\Category`:  "categories (derived)",
		`App\Models\Address`:   "addresses (derived)",
		`App\Models\User`:      "users (derived)",
	}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for name, table := range want {
		if got[name] != table {
			t.Errorf("%s table = %q, want %q", name, got[name], table)
		}
	}
}

// TestEloquentModels_ThroughABaseClassInAnotherFile: the base class is abstract, so
// it backs no table, and what it declares passes to its subclasses.
func TestEloquentModels_ThroughABaseClassInAnotherFile(t *testing.T) {
	got := modelTables(eloquentFrom(map[string]string{
		"a.php": `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
abstract class BaseModel extends Model {}
abstract class LegacyModel extends BaseModel { protected $table = 'legacy_rows'; }
`,
		"b.php": `<?php
namespace App\Models;
class Invoice extends BaseModel {}
class LegacyInvoice extends LegacyModel {}
class Plain {}
class Service extends Plain {}
trait HasNotes { protected $table = 'notes'; }
`,
	}))
	want := map[string]string{
		`App\Models\Invoice`:       "invoices (derived)",
		`App\Models\LegacyInvoice`: "legacy_rows (declared)",
	}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for name, table := range want {
		if got[name] != table {
			t.Errorf("%s table = %q, want %q", name, got[name], table)
		}
	}
}

func TestEloquentAssociations(t *testing.T) {
	ff := eloquentFrom(map[string]string{
		"a.php": `<?php
namespace App\Models;
use Illuminate\Database\Eloquent\Model;
use App\Billing\Invoice;
class Order extends Model {
    public function customer() { return $this->belongsTo(Customer::class); }
    public function invoices() { return $this->hasMany(Invoice::class, 'order_id'); }
    public function tags() { return $this->belongsToMany('App\Models\Tag'); }
    public function owner() { return $this->morphTo(); }
    public function total() { return $this->lines->sum('price'); }
}
class Customer extends Model {}
class Tag extends Model {}
`,
		"b.php": `<?php
namespace App\Services;
class Report {
    public function rows() { return $this->hasMany(Row::class); }
}
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindAssociation {
			got[f.Name] = f.PropString("model") + " " + f.PropString("macro") + " " + f.PropString("target")
		}
	}
	want := map[string]string{
		`App\Models\Order::customer`: `App\Models\Order belongsTo App\Models\Customer`,
		`App\Models\Order::invoices`: `App\Models\Order hasMany App\Billing\Invoice`,
		`App\Models\Order::tags`:     `App\Models\Order belongsToMany App\Models\Tag`,
	}
	if len(got) != len(want) {
		t.Fatalf("associations = %v, want %v", got, want)
	}
	for name, assoc := range want {
		if got[name] != assoc {
			t.Errorf("%s = %q, want %q", name, got[name], assoc)
		}
	}
}

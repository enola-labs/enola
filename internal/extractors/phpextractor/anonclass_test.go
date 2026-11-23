package phpextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestAnonymousMigrationClassIsNamedByItsFile: `return new class extends Migration`
// declares a class with its base, and its members are named after it, so two
// migration files no longer share one `::up`.
func TestAnonymousMigrationClassIsNamedByItsFile(t *testing.T) {
	src := `<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('flights', function ($table) {});
    }

    public function down(): void
    {
        Schema::dropIfExists('flights');
    }
};
`
	const class = "class@anonymous:database/migrations/2024_01_01_000000_create_flights_table"
	ff := extractFileAST([]byte(src), "database/migrations/2024_01_01_000000_create_flights_table.php")
	wantTargets(t, "implements", relationsOf(t, ff, class, facts.RelImplements), `Illuminate\Database\Migrations\Migration`)
	wantTargets(t, "calls", relationsOf(t, ff, class+"::up", facts.RelCalls), `Illuminate\Support\Facades\Schema::create`)
	relationsOf(t, ff, class+"::down", facts.RelCalls)
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && (f.Name == "::up" || f.Name == "::down") {
			t.Fatalf("member named after an empty owner: %s", f.Name)
		}
	}
}

// TestSecondAnonymousClassInAFileTakesASuffix: two anonymous classes in one file
// keep apart.
func TestSecondAnonymousClassInAFileTakesASuffix(t *testing.T) {
	src := `<?php
$a = new class { public function run() {} };
$b = new class { public function run() {} };
`
	ff := extractFileAST([]byte(src), "config/handlers.php")
	relationsOf(t, ff, "class@anonymous:config/handlers::run", facts.RelCalls)
	relationsOf(t, ff, "class@anonymous:config/handlers$1::run", facts.RelCalls)
}

// TestClassAttributeNamesAClass: `#[ObservedBy(OrderObserver::class)]` is the
// model's reference to its observer.
func TestClassAttributeNamesAClass(t *testing.T) {
	src := `<?php
namespace App\Models;

use App\Observers\OrderObserver;
use Illuminate\Database\Eloquent\Attributes\ObservedBy;
use Illuminate\Database\Eloquent\Model;

#[ObservedBy([OrderObserver::class])]
class Order extends Model
{
    protected $casts = ['state' => OrderState::class];
}
`
	ff := extractFileAST([]byte(src), "app/Models/Order.php")
	wantTargets(t, "names", relationsOf(t, ff, `App\Models\Order`, facts.RelNames),
		`App\Observers\OrderObserver`, `App\Models\OrderState`)
}

// TestFractalIncludeListsAreRecorded: the include lists name the include methods
// Fractal calls; a keyed entry or a non-string is not one.
func TestFractalIncludeListsAreRecorded(t *testing.T) {
	src := `<?php
namespace App\Transformer;

use League\Fractal\TransformerAbstract;

class AccountTransformer extends TransformerAbstract
{
    protected array $defaultIncludes = ['posts', 'relationship'];
    protected $availableIncludes = ['posts', 'avatar', 'x' => 'keyed', self::OTHER];

    public function transform($account) { return []; }
}
`
	ff := extractFileAST([]byte(src), "app/Transformer/AccountTransformer.php")
	for _, f := range ff {
		if f.Name != `App\Transformer\AccountTransformer` || f.Kind != facts.KindSymbol {
			continue
		}
		got, _ := f.Props[propFractalIncludes].([]string)
		wantTargets(t, propFractalIncludes, got, "avatar", "posts", "relationship")
		return
	}
	t.Fatal("no transformer class")
}

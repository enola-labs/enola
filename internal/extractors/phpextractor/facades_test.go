package phpextractor

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A static call on one of the application's facades reaches the method of the class
// its key is bound to, in either way a provider binds it. A framework facade is left.
func TestFacadeCalls_ReachTheBoundClass(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"app/Support/Facades/Preferences.php": `<?php
namespace App\Support\Facades;

use Illuminate\Support\Facades\Facade;

class Preferences extends Facade
{
    protected static function getFacadeAccessor(): string
    {
        return 'preferences';
    }
}
`,
		"app/Facades/Activity.php": `<?php
namespace App\Facades;

use Illuminate\Support\Facades\Facade;

class Activity extends Facade
{
    protected static function getFacadeAccessor()
    {
        return 'activity';
    }
}
`,
		"app/Providers/AppServiceProvider.php": `<?php
namespace App\Providers;

use App\Support\Preferences;
use App\Activity\ActivityLogger;
use Illuminate\Support\ServiceProvider;

class AppServiceProvider extends ServiceProvider
{
    public array $singletons = [
        'activity' => ActivityLogger::class,
    ];

    public function register(): void
    {
        $this->app->bind('preferences', static fn (): Preferences => new Preferences());
    }
}
`,
		"app/Support/Preferences.php": `<?php
namespace App\Support;

class Preferences
{
    public function mark(): void {}
}
`,
		"app/Activity/ActivityLogger.php": `<?php
namespace App\Activity;

class ActivityLogger
{
    public function add(string $type): void {}
}
`,
		"app/Http/Controllers/HomeController.php": `<?php
namespace App\Http\Controllers;

use App\Support\Facades\Preferences;
use App\Facades\Activity;
use Illuminate\Support\Facades\Log;

class HomeController
{
    public function index()
    {
        Preferences::mark();
        Activity::add('view');
        Log::debug('home');
    }
}
`,
	}
	rels := make([]string, 0, len(files))
	for rel, content := range files {
		writeFile(t, dir, rel, content)
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), dir, rels)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == `App\Http\Controllers\HomeController::index` {
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls {
					calls = append(calls, r.Target)
				}
			}
		}
	}
	for _, want := range []string{`App\Support\Preferences::mark`, `App\Activity\ActivityLogger::add`, `Illuminate\Support\Facades\Log::debug`} {
		if !slices.Contains(calls, want) {
			t.Errorf("index calls %v, want %q", calls, want)
		}
	}
}

// A PHP declaration's doc is its docblock up to the first tag, or the run of line
// comments above it.
func TestDoc_PHPDocblock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "app/Http/Controllers/BookController.php", `<?php
namespace App\Http\Controllers;

/**
 * Books of the shelf.
 */
class BookController
{
    /**
     * Store a newly created book.
     *
     * @throws ValidationException
     */
    #[Route('/books')]
    public function store() {}

    // Show the book.
    public function show() {}

    public function index() {}
}
`)
	ff, err := New().Extract(context.Background(), dir, []string{"app/Http/Controllers/BookController.php"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			got[f.Name] = f.PropString(facts.PropDoc)
		}
	}
	for name, doc := range map[string]string{
		`App\Http\Controllers\BookController`:        "Books of the shelf.",
		`App\Http\Controllers\BookController::store`: "Store a newly created book.",
		`App\Http\Controllers\BookController::show`:  "Show the book.",
		`App\Http\Controllers\BookController::index`: "",
	} {
		if got[name] != doc {
			t.Errorf("%s doc = %q, want %q (have %v)", name, got[name], doc, got)
		}
	}
}

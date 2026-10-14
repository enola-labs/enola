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

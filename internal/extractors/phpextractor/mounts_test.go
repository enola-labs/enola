package phpextractor

import (
	"context"
	"sort"
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// laravelRoutesOf runs a whole Laravel repository through Extract and returns its
// server routes as "METHOD path -> handler".
func laravelRoutesOf(t *testing.T, files map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "composer.json", `{"require":{"laravel/framework":"^11.0"}}`)
	names := []string{"composer.json"}
	for rel, content := range files {
		writeFile(t, dir, rel, content)
		names = append(names, rel)
	}
	sort.Strings(names)
	result, err := New().Extract(context.Background(), dir, names)
	if err != nil {
		t.Fatal(err)
	}
	return routeSet(result)
}

func wantRouteLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("routes = %q\nwant     %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("routes = %q\nwant     %q", got, want)
		}
	}
}

// TestLaravelMounts_ProviderPrefixesReachTheRoutes: the provider says where each
// file is served, and a string handler is still placed under the controllers.
func TestLaravelMounts_ProviderPrefixesReachTheRoutes(t *testing.T) {
	got := laravelRoutesOf(t, map[string]string{
		"app/Providers/RouteServiceProvider.php": `<?php
namespace App\Providers;
use Illuminate\Support\Facades\Route;
class RouteServiceProvider {
    public function boot(): void {
        $this->routes(function () {
            Route::prefix('api')->middleware('api')->namespace($this->namespace)
                ->group(base_path('routes/api_v1.php'));
            Route::prefix('api/v2')->middleware('api')->group(base_path('routes/api_v2.php'));
            Route::middleware('web')->group(base_path('routes/web.php'));
        });
    }
}`,
		"routes/api_v1.php": `<?php
Route::get('country/regions', 'Api\LocationController@regions');`,
		"routes/api_v2.php": `<?php
use App\Http\Controllers\CompanyController;
Route::controller(CompanyController::class)->prefix('/company')->group(function (): void {
    Route::get('', 'index');
});`,
		"routes/web.php":     `<?php Route::get('/', [HomeController::class, 'show']);`,
		"routes/console.php": `<?php Route::get('/loose', [LooseController::class, 'show']);`,
		"app/Http/Controllers/Api/LocationController.php": `<?php
namespace App\Http\Controllers\Api;
class LocationController { public function regions() {} }`,
	})
	wantRouteLines(t, got,
		`GET /api/country/regions -> App\Http\Controllers\Api\LocationController::regions`,
		`GET /api/v2/company -> App\Http\Controllers\CompanyController::index`,
		`GET / -> HomeController::show`,
		// Nothing loads this file, so its route is registered as written.
		`GET /loose -> LooseController::show`,
	)
}

// TestLaravelMounts_WithRouting: Laravel 11 names its route files to withRouting,
// and serves the api file under apiPrefix, "api" unless it says otherwise.
func TestLaravelMounts_WithRouting(t *testing.T) {
	routes := map[string]string{
		"routes/web.php":   `<?php Route::get('/home', [HomeController::class, 'show']);`,
		"routes/api.php":   `<?php Route::get('/users', [UserController::class, 'index']);`,
		"routes/admin.php": `<?php Route::get('/stats', [StatsController::class, 'index']);`,
	}
	for name, tc := range map[string]struct{ bootstrap, api string }{
		"default prefix": {`api: __DIR__.'/../routes/api.php',`, "/api/users"},
		"given prefix":   {`api: __DIR__.'/../routes/api.php', apiPrefix: 'api/v1',`, "/api/v1/users"},
		"empty prefix":   {`api: dirname(__DIR__).'/routes/api.php', apiPrefix: '',`, "/users"},
	} {
		files := map[string]string{"bootstrap/app.php": `<?php
return Application::configure(basePath: dirname(__DIR__))
    ->withRouting(
        web: __DIR__.'/../routes/web.php',
        ` + tc.bootstrap + `
        then: function () {
            Route::prefix('admin')->group(base_path('routes/admin.php'));
        },
    )->create();`}
		for rel, content := range routes {
			files[rel] = content
		}
		got := laravelRoutesOf(t, files)
		want := []string{
			"GET /home -> HomeController::show",
			"GET " + tc.api + " -> UserController::index",
			"GET /admin/stats -> StatsController::index",
		}
		sort.Strings(want)
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("%s: routes = %q, want %q", name, got, want)
		}
	}
}

// TestLaravelMounts_RequireInsideAGroupAndTwice: a routes file required inside a
// group's closure is registered under that group, through every file that leads to
// it, and a file loaded from two places is served under both.
func TestLaravelMounts_RequireInsideAGroupAndTwice(t *testing.T) {
	got := laravelRoutesOf(t, map[string]string{
		"bootstrap/app.php": `<?php
return Application::configure()->withRouting(api: __DIR__.'/../routes/api.php')->create();`,
		"routes/api.php": `<?php
Route::prefix('v1')->group(function () {
    require __DIR__.'/api/orders.php';
});
Route::prefix('v2')->group(function () {
    require __DIR__ . '/api/orders.php';
});
require __DIR__.'/auth.php';
Route::prefix('dynamic')->group(base_path(sprintf('routes/%s.php', $name)));`,
		"routes/api/orders.php": `<?php Route::get('/orders', [OrderController::class, 'index']);`,
		"routes/auth.php":       `<?php Route::post('/login', [LoginController::class, 'store']);`,
	})
	wantRouteLines(t, got,
		"GET /api/v1/orders -> OrderController::index",
		"GET /api/v2/orders -> OrderController::index",
		"POST /api/login -> LoginController::store",
	)
}

func TestRouteFilePath(t *testing.T) {
	w := &laravelRouteWalker{relFile: "bootstrap/app.php"}
	for expr, want := range map[string]string{
		`base_path('routes/api.php')`:            "routes/api.php",
		`__DIR__.'/../routes/web.php'`:           "routes/web.php",
		`dirname(__DIR__).'/routes/web.php'`:     "routes/web.php",
		`base_path().'/routes/x.php'`:            "routes/x.php",
		`(base_path('routes/api/v1.php'))`:       "routes/api/v1.php",
		`base_path(sprintf('routes/%s.php', 1))`: "",
		`__DIR__.'/../../outside/routes/x.php'`:  "",
		`base_path('routes')`:                    "",
		`$path`:                                  "",
	} {
		src := []byte("<?php\n$x = " + expr + ";")
		w.src = src
		tree := parsePHPForTest(t, src)
		// program > expression_statement > assignment_expression > right
		assign := tree.RootNode().NamedChild(1).NamedChild(0)
		if got := w.routeFilePath(assign.ChildByFieldName("right")); got != want {
			t.Errorf("routeFilePath(%s) = %q, want %q", expr, got, want)
		}
		tree.Close()
	}
}

func parsePHPForTest(t *testing.T, src []byte) *sitter.Tree {
	t.Helper()
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		t.Fatal(err)
	}
	return parser.Parse(src, nil)
}

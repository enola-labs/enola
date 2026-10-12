package phpextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// serverRoutes filters the route facts that represent server (inbound) endpoints.
func serverRoutes(ff []facts.Fact) []facts.Fact {
	var out []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["role"] == "server" {
			out = append(out, f)
		}
	}
	return out
}

// routeByMethodPath finds a server route fact by method + path.
func routeByMethodPath(t *testing.T, ff []facts.Fact, method, path string) facts.Fact {
	t.Helper()
	for _, f := range serverRoutes(ff) {
		if f.Props["method"] == method && f.Name == path {
			return f
		}
	}
	t.Fatalf("no %s %s route; got %s", method, path, routeSummary(ff))
	return facts.Fact{}
}

func routeSummary(ff []facts.Fact) string {
	s := ""
	for _, f := range serverRoutes(ff) {
		s += "\n  " + f.Props["method"].(string) + " " + f.Name
		if h, ok := f.Props["handler"]; ok {
			s += " -> " + h.(string)
		}
	}
	return s
}

func TestLaravelRoutes_VerbsAndHandler(t *testing.T) {
	src := `<?php
Route::get('/users', [UserController::class, 'index']);
Route::post('/users', 'UserController@store');
Route::delete('/users/{id}', [UserController::class, 'destroy']);
`
	ff := extractLaravelRoutes([]byte(src), "routes/web.php")
	if got := len(serverRoutes(ff)); got != 3 {
		t.Fatalf("got %d routes, want 3: %s", got, routeSummary(ff))
	}
	get := routeByMethodPath(t, ff, "GET", "/users")
	if get.Props["handler"] != "UserController::index" {
		t.Errorf("GET handler = %v, want UserController::index", get.Props["handler"])
	}
	post := routeByMethodPath(t, ff, "POST", "/users")
	if post.Props["handler"] != "UserController::store" {
		t.Errorf("POST handler = %v, want UserController::store", post.Props["handler"])
	}
	routeByMethodPath(t, ff, "DELETE", "/users/{id}")
}

func TestLaravelRoutes_GroupArrayPrefix(t *testing.T) {
	src := `<?php
Route::group(['prefix' => 'api'], function () {
    Route::get('/users', [UserController::class, 'index']);
});
`
	ff := extractLaravelRoutes([]byte(src), "routes/api.php")
	routeByMethodPath(t, ff, "GET", "/api/users")
}

func TestLaravelRoutes_FluentPrefixGroup(t *testing.T) {
	src := `<?php
Route::prefix('admin')->name('admin.')->group(function () {
    Route::get('/dash', [DashController::class, 'index'])->name('dash');
});
`
	ff := extractLaravelRoutes([]byte(src), "routes/web.php")
	f := routeByMethodPath(t, ff, "GET", "/admin/dash")
	if f.Props["name"] != "dash" {
		t.Errorf("name = %v, want dash", f.Props["name"])
	}
}

func TestLaravelRoutes_NestedGroups(t *testing.T) {
	src := `<?php
Route::prefix('api')->group(function () {
    Route::prefix('v1')->group(function () {
        Route::get('/ping', 'HealthController@ping');
    });
});
`
	ff := extractLaravelRoutes([]byte(src), "routes/api.php")
	routeByMethodPath(t, ff, "GET", "/api/v1/ping")
}

func TestLaravelRoutes_Resource(t *testing.T) {
	src := `<?php
Route::resource('photos', PhotoController::class);
`
	ff := extractLaravelRoutes([]byte(src), "routes/web.php")
	if got := len(serverRoutes(ff)); got != 7 {
		t.Fatalf("got %d resource routes, want 7: %s", got, routeSummary(ff))
	}
	idx := routeByMethodPath(t, ff, "GET", "/photos")
	if idx.Props["handler"] != "PhotoController::index" {
		t.Errorf("index handler = %v, want PhotoController::index", idx.Props["handler"])
	}
	routeByMethodPath(t, ff, "POST", "/photos")
	routeByMethodPath(t, ff, "GET", "/photos/create")
	routeByMethodPath(t, ff, "GET", "/photos/{id}")
	routeByMethodPath(t, ff, "GET", "/photos/{id}/edit")
	routeByMethodPath(t, ff, "PUT", "/photos/{id}")
	routeByMethodPath(t, ff, "DELETE", "/photos/{id}")
}

func TestLaravelRoutes_ApiResource(t *testing.T) {
	src := `<?php
Route::apiResource('books', BookController::class);
`
	ff := extractLaravelRoutes([]byte(src), "routes/api.php")
	if got := len(serverRoutes(ff)); got != 5 {
		t.Fatalf("got %d apiResource routes, want 5: %s", got, routeSummary(ff))
	}
	// No HTML form endpoints.
	for _, f := range serverRoutes(ff) {
		if f.Name == "/books/create" || f.Name == "/books/{id}/edit" {
			t.Errorf("apiResource should not emit %s", f.Name)
		}
	}
}

func TestLaravelRoutes_Match(t *testing.T) {
	src := `<?php
Route::match(['get', 'post'], '/contact', 'ContactController@handle');
`
	ff := extractLaravelRoutes([]byte(src), "routes/web.php")
	if got := len(serverRoutes(ff)); got != 2 {
		t.Fatalf("got %d match routes, want 2: %s", got, routeSummary(ff))
	}
	routeByMethodPath(t, ff, "GET", "/contact")
	routeByMethodPath(t, ff, "POST", "/contact")
}

func TestLaravelRoutes_HandlerClassIsExpandedThroughImports(t *testing.T) {
	src := `<?php
use App\Http\Controllers\OrderController;
use App\Http\Controllers\{PingController, ReportController as Reports};

Route::get('/orders/{id}', [OrderController::class, 'show']);
Route::get('/ping', PingController::class);
Route::get('/reports', [Reports::class, 'index']);
Route::get('/absolute', [\Vendor\ThingController::class, 'go']);
Route::get('/closure', function () { return 1; });
`
	ff := extractLaravelRoutes([]byte(src), "routes/api.php")
	for path, want := range map[string]string{
		"/orders/{id}": `App\Http\Controllers\OrderController::show`,
		"/ping":        `App\Http\Controllers\PingController::__invoke`,
		"/reports":     `App\Http\Controllers\ReportController::index`,
		"/absolute":    `Vendor\ThingController::go`,
	} {
		if got := routeByMethodPath(t, ff, "GET", path).Props["handler"]; got != want {
			t.Errorf("GET %s handler = %v, want %s", path, got, want)
		}
	}
	if h, ok := routeByMethodPath(t, ff, "GET", "/closure").Props["handler"]; ok {
		t.Errorf("a closure route names no handler, got %v", h)
	}
}

func TestLaravelRoutes_ControllerAndNamespaceGroups(t *testing.T) {
	src := `<?php
use App\Http\Controllers\OrderController;

Route::controller(OrderController::class)->prefix('orders')->group(function () {
    Route::get('/', 'index');
    Route::post('/', 'store');
});
Route::group(['controller' => OrderController::class], function () {
    Route::get('/latest', 'latest');
});
Route::namespace('Admin')->group(function () {
    Route::get('/users', 'UserController@index');
    Route::get('/root', '\App\RootController@index');
});
Route::get('/view', 'welcome');
`
	ff := extractLaravelRoutes([]byte(src), "routes/web.php")
	for route, want := range map[[2]string]string{
		{"GET", "/orders"}:  `App\Http\Controllers\OrderController::index`,
		{"POST", "/orders"}: `App\Http\Controllers\OrderController::store`,
		{"GET", "/latest"}:  `App\Http\Controllers\OrderController::latest`,
		{"GET", "/users"}:   `Admin\UserController::index`,
		{"GET", "/root"}:    `App\RootController::index`,
	} {
		if got := routeByMethodPath(t, ff, route[0], route[1]).Props["handler"]; got != want {
			t.Errorf("%s %s handler = %v, want %s", route[0], route[1], got, want)
		}
	}
	if h, ok := routeByMethodPath(t, ff, "GET", "/view").Props["handler"]; ok {
		t.Errorf("a bare string outside a controller group names no handler, got %v", h)
	}
}

// TestLaravelRoutes_ActionArray: the action array names its handler under 'uses' and
// its route name under 'as'. A class already written under the group's namespace is
// not placed under it a second time.
func TestLaravelRoutes_ActionArray(t *testing.T) {
	src := `<?php
Route::group(['namespace' => 'Acme\Http\Controllers', 'prefix' => 'profile'], static function (): void {
    Route::get('/', ['uses' => 'ProfileController@index', 'as' => 'profile.index']);
    Route::post('/tokens', ['uses' => 'Acme\Http\Controllers\Profile\TokenController@store', 'as' => 'tokens.store']);
    Route::get('/passport', ['uses' => '\Vendor\Passport\TokenController@show']);
    Route::get('/inline', ['as' => 'inline', function () { return 1; }]);
});
`
	ff := extractLaravelRoutes([]byte(src), "routes/web.php")
	for route, want := range map[[2]string]string{
		{"GET", "/profile"}:          `Acme\Http\Controllers\ProfileController::index`,
		{"POST", "/profile/tokens"}:  `Acme\Http\Controllers\Profile\TokenController::store`,
		{"GET", "/profile/passport"}: `Vendor\Passport\TokenController::show`,
	} {
		if got := routeByMethodPath(t, ff, route[0], route[1]).Props["handler"]; got != want {
			t.Errorf("%s %s handler = %v, want %s", route[0], route[1], got, want)
		}
	}
	if got := routeByMethodPath(t, ff, "GET", "/profile").Props["name"]; got != "profile.index" {
		t.Errorf("route name = %v, want the action array's 'as'", got)
	}
	if h, ok := routeByMethodPath(t, ff, "GET", "/profile/inline").Props["handler"]; ok {
		t.Errorf("an action array holding a closure names no handler, got %v", h)
	}
}

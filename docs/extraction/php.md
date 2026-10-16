# PHP — what enola extracts

Parsed with tree-sitter. Detected by `composer.json`, a WordPress bootstrap file
(`wp-load.php` / `wp-settings.php` / `wp-config.php`), or any `.php` source at any
depth, so a legacy tree with no package manager is still indexed.

Fixtures: [`php_sample`](../../internal/engine/testdata/repos/php_sample/) (WordPress) ·
[`php_laravel_sample`](../../internal/engine/testdata/repos/php_laravel_sample/) ·
[`php_symfony_sample`](../../internal/engine/testdata/repos/php_symfony_sample/) ·
[`php_multirepo`](../../internal/engine/testdata/repos/php_multirepo/)

## At a glance

| You write | enola stores | Kind |
|---|---|---|
| a source directory | one module, `framework=laravel` / `symfony` where detected | `module` |
| `class`, `interface`, `trait`, `function` | a symbol with `symbol_kind` | `symbol` |
| `use App\Other\Thing;` | a dependency | `dependency` |
| `Route::get('/x', …)` (Laravel) | a server route | `route` |
| `Route::resource('photos', …)` | one route per action the resource registers | `route` |
| `#[Route('/x')]` (Symfony attribute) | a server route with its route name | `route` |
| `config/routes.yaml` | a server route per configured method | `route` |
| `add_action` / `add_filter` (WordPress) | a pseudo-route on the hook name | `route` |
| Guzzle / Laravel `Http::` / Symfony HttpClient | a client route with `role: client` | `route` |
| a class extending an Eloquent model | a model with its table | `storage` |
| `$this->hasMany(Photo::class)` in a model | a relation to the model it names | `association` |

## Routes — Laravel, including resource expansion

```php
Route::prefix('api')->group(function () {
    Route::get('/users', [UserController::class, 'index'])->name('users.index');
    Route::post('/users', [UserController::class, 'store']);
    Route::resource('photos', PhotoController::class);
    Route::get('/profiles/{id}', ProfileController::class);
});
```

Every PHP file under a `routes` directory is read, whatever it is called. Groups compose:
a `prefix` lands on the path, and a `namespace` or a `controller` group on the handler.

The handler is the class as PHP resolves it, through the file's `use` statements:

```
GET  /api/users          handler=App\Http\Controllers\UserController::index     name=users.index
GET  /api/profiles/{id}  handler=App\Http\Controllers\ProfileController::__invoke
```

A controller named alone is invokable, so its handler is `__invoke`. A string action
(`'UserController@index'`, or `['uses' => …]`) is placed under the controller namespace.
When the class and method exist in the repository, the route also carries a `handled_by`
edge to the method. For an action the controller inherits, the edge points at the parent
class or trait that declares it. A class that resolves to nothing, or to two classes,
keeps the handler as written and gets no edge.

Line 10 is one call. It becomes eight facts:

```
GET    /api/photos                action=index     handler=PhotoController::index
POST   /api/photos                action=store     handler=PhotoController::store
GET    /api/photos/create         action=create    handler=PhotoController::create
GET    /api/photos/{photo}        action=show      handler=PhotoController::show
PUT    /api/photos/{photo}        action=update    handler=PhotoController::update
PATCH  /api/photos/{photo}        action=update    handler=PhotoController::update
DELETE /api/photos/{photo}        action=destroy   handler=PhotoController::destroy
GET    /api/photos/{photo}/edit   action=edit      handler=PhotoController::edit
```

All eight keep `routes/api.php:10` as their location, because that is the line you edit to
change any of them. `update` is one action that answers to two verbs. The parameter is
the singular of the resource, as Laravel names it; an irregular plural keeps the name it
was written with. Beyond the plain form:

| You write | enola stores |
|---|---|
| `->only(['index', 'show'])`, `->except(…)`, or the same keys in the options array | only the actions left |
| `Route::apiResource(…)` | no `create` and no `edit` |
| `'albums.songs'` | `/albums/{album}/songs/{song}` |
| `'admin/photos'` | `/admin/photos/{photo}` |
| `->shallow()` on a nested resource | member routes without the parent |
| `->parameters(['users' => 'member'])` | `/users/{member}` |
| `Route::resources([…])`, `Route::apiResources([…])` | each entry, expanded the same way |

`->name()` is preserved as `name` so a `route('users.index')` reference resolves.

### Where a routes file is served

A routes file does not say which prefix it lives under. Whatever loads it does:

```php
// bootstrap/app.php
->withRouting(
    then: function () {
        Route::prefix('internal')->group(base_path('routes/internal.php'));
    },
)
```

```php
// routes/internal.php
Route::get('/profiles/{id}', ProfileController::class);
```

```
GET  /internal/profiles/{id}   routes/internal.php:6
```

The route keeps the file and line it is written on and takes the path it is served at.
Three ways of loading a file are read, from any PHP file in the repository:

| You write | The file's routes get |
|---|---|
| `Route::prefix('api')->group(base_path('routes/api.php'))` | the group's prefix, namespace and controller |
| `->withRouting(web: …, api: …, apiPrefix: 'api/v1')` | nothing for `web`, `apiPrefix` for `api` (`api` when not given) |
| `require __DIR__.'/auth.php';` inside a group's closure | that group's scopes |

A file loaded from two places is served twice, so its routes are stored once per place. A
file loaded by another routes file inherits what that file was loaded under. A file
nothing loads keeps its paths as written.

## Models and relations — Eloquent

```php
class User extends Model
{
    protected $table = 'accounts';

    public function photos()
    {
        return $this->hasMany(Photo::class);
    }
}
```

```
storage      App\Models\User    props: storage_kind=model, table=accounts, table_source=declared
storage      App\Models\Photo   props: storage_kind=model, table=photos,   table_source=derived
association  App\Models\User::photos   props: macro=hasMany, target=App\Models\Photo
```

A model is a concrete class that reaches an Eloquent base class through its parents,
including base classes of your own in other files. Its table is the `$table` it or an
ancestor declares, else the plural Eloquent derives, and `table_source` says which. The
derived form applies the regular English plural only. `morphTo()` names no model, so it
declares no association.

This is what `enola endpoint` walks: from a route to its handler, through the calls the
handler makes, to the models and tables behind them.

## Calls and references

A call on `$this`, on a property whose type the class declares (a typed property, a
promoted constructor parameter, or one the constructor assigns), on a typed parameter, or
on an object created in place resolves to the method it runs:

```
App\Http\Controllers\ProfileController::__invoke --calls--> App\Services\UserDirectory::find
```

The bare method name stays beside it. A receiver typed by an interface reaches the
interface's method, and the implementing method when exactly one class implements it.

Three more edges say that a class is used without calling it:

| You write | Edge |
|---|---|
| a class-typed constructor parameter | `injects`, from the class |
| `Foo::class`, an enum case, a class constant, a static property | `names`, from the method or class that writes it |
| the same at file scope, in a config or bootstrap file | `names`, on a file reference |

Class names resolve as PHP resolves them: through the imports, else relative to the
file's namespace.

## Routes — Symfony, from two sources

Attributes on the controller and YAML in `config/` are both read, and the fact records
which:

```
route  /products/{id}   src/Controller/ProductController.php:13
       props: framework=symfony, name=product_show, handler=App\Controller\ProductController::show
route  /health          config/routes.yaml
       props: framework=symfony, name=health_check, source=symfony-config,
              handler=App\Controller\HealthController::check
route  /old/home        config/routes.yaml     method=GET   ┐ one fact per method,
route  /old/home        config/routes.yaml     method=HEAD  ┘ as the config declares
```

A YAML-declared route has no line number — it is a configuration entry, not a statement —
and `source=symfony-config` says so rather than inventing one.

## Routes — WordPress hooks

WordPress has no router; it has hooks. They are the actual entry points, so they are
recorded as routes with the hook type as the method:

```php
add_action('init', 'acme_setup');
add_filter('the_title', 'acme_format');
```

```
route  ACTION init       inc/hooks.php:6   props: framework=wordpress, hook=add_action, callback=acme_setup
route  FILTER the_title  inc/hooks.php:7   props: framework=wordpress, hook=add_filter, callback=acme_format
```

This is what stops a WordPress plugin's entire callback surface from reading as dead code:
those functions have no PHP caller anywhere. Once every file is read, a registration
is also `handled_by` the symbol its callback names, since a PHP symbol is named by its
qualified name: `'acme_setup'` is the function, `'WP_Embed::autoembed'` the method, and a
callback that names nothing of the repository (a PHP built-in) binds nothing.

## Outbound calls and cross-repo linking

```
route  /billing/invoices   consumer/src/BillingClient.php:16
       props: role=client, framework=guzzle, api=BillingClient, source=php-http-client
route  /billing/invoices   provider/routes/api.php:9
       props: role=server, framework=laravel, handler=InvoiceController::index

service consumer  props: edge_coverage=[{detected: 2, resolved: 2, unresolved: 0}]
                  --depends_on--> provider
```

Guzzle, Laravel's `Http::` facade and Symfony's `HttpClient` are all recognised, each
tagged with its own `framework` so you can tell which client library made the call.

Note the third provider route in that fixture:

```
route  /billing/invoices/{id}   props: role=server, unmatched_by_clients=true
```

The provider serves it; no loaded client calls it. That is the `unused-routes` finding —
a candidate at confidence `0.6`, because the caller may simply be a repository you have
not loaded.

## What is deliberately not extracted

- **Routes registered in a loop** over a config array, or by a plugin discovery pass at
  runtime.
- **A routes file loaded by a computed name** (`base_path(sprintf('routes/%s.php', $type))`),
  or under a prefix that is not a literal. Its routes keep the paths they are written
  with: no prefix is guessed.
- **Non-literal paths and URLs.** `$client->get($endpoint)` with no literal binding is
  not guessed.
- **`__call` / `__callStatic` magic methods** — a call through them resolves to nothing
  rather than to every candidate.
- **Blade and Twig templates** are not yet parsed for helper calls, so a method used only
  from a template can read as unreferenced. (The Ruby extractor does read ERB; PHP does
  not yet.)

---

Measured on real PHP repositories: [BENCHMARKS.md](../BENCHMARKS.md).

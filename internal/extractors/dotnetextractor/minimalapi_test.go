package dotnetextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestMinimalAPI_GroupPrefixComposedFromLocalVariable(t *testing.T) {
	got, _ := routeSet(t, map[string]string{
		"Apis/OrdersApi.cs": `
namespace Acme.Api;

public static class OrdersApi
{
    public static RouteGroupBuilder MapOrdersApiV1(this IEndpointRouteBuilder app)
    {
        var api = app.MapGroup("api/orders");

        api.MapPut("/cancel", CancelOrderAsync);
        api.MapGet("{orderId:int}", GetOrderAsync);
        api.MapGet("/", GetOrdersByUserAsync);

        return api;
    }

    public static void CancelOrderAsync() { }
    public static void GetOrderAsync() { }
    public static void GetOrdersByUserAsync() { }
}
`,
	})
	assertRoutes(t, got, []string{
		"PUT /api/orders/cancel",
		"GET /api/orders/{orderId:int}",
		"GET /api/orders",
	})
}

// TestMinimalAPI_ChainedAndNestedGroups covers the two shapes eShop and the MCP
// SDK actually write: a fluent call after MapGroup, and a group built on a group.
func TestMinimalAPI_ChainedAndNestedGroups(t *testing.T) {
	got, _ := routeSet(t, map[string]string{
		"Apis/CatalogApi.cs": `
namespace Acme.Api;

public static class CatalogApi
{
    public static void MapCatalogApi(this IEndpointRouteBuilder app)
    {
        // A fluent call after MapGroup must not hide the prefix.
        var api = app.MapGroup("api/catalog").HasApiVersion(1, 0);
        api.MapGet("/items", GetAllItems);

        // A group of a group composes both prefixes.
        var admin = api.MapGroup("admin");
        admin.MapDelete("/items/{id:int}", DeleteItem);
    }

    public static void GetAllItems() { }
    public static void DeleteItem() { }
}
`,
	})
	assertRoutes(t, got, []string{
		"GET /api/catalog/items",
		"DELETE /api/catalog/admin/items/{id:int}",
	})
}

// TestMinimalAPI_NonLiteralGroupPrefixDropsItsRoutes is the false-positive guard.
// The MCP C# SDK mounts its whole surface at a caller-supplied pattern:
//
//	public static IEndpointConventionBuilder MapMcp(this IEndpointRouteBuilder endpoints, string pattern = "")
//	{
//	    var mcpGroup = endpoints.MapGroup(pattern);
//	    ...
//	    streamableHttpGroup.MapPost("", handler);
//	}
//
// The real path is whatever the host application mounts it at. Publishing the
// registration path alone would claim an endpoint the library does not serve — and
// since that path is "", every one of them would land on "/", recreating the
// phantom-root collapse conventional routing produced.
func TestMinimalAPI_NonLiteralGroupPrefixDropsItsRoutes(t *testing.T) {
	got, _ := routeSet(t, map[string]string{
		"Api/McpEndpoints.cs": `
namespace Acme.Api;

public static class McpEndpoints
{
    public static void MapMcp(this IEndpointRouteBuilder endpoints, string pattern = "")
    {
        var mcpGroup = endpoints.MapGroup(pattern);
        var streamable = mcpGroup.MapGroup("");

        streamable.MapPost("", HandlePost);
        streamable.MapGet("/sse", HandleSse);
    }

    public static void HandlePost() { }
    public static void HandleSse() { }
}
`,
	})
	if len(got) != 0 {
		t.Errorf("routes under an unresolvable group prefix should be dropped, got %v", got)
	}
}

// TestMinimalAPI_RootBuilderReceiver covers a registration straight on the
// `app`/`endpoints` parameter, with no group at all.
func TestMinimalAPI_RootBuilderReceiver(t *testing.T) {
	got, _ := routeSet(t, map[string]string{
		"Api/Defaults.cs": `
namespace Acme.Api;

public static class Defaults
{
    public static void MapDefaults(this IEndpointRouteBuilder app)
    {
        app.MapGet("/", Redirect);
        app.MapGet("/health", Health);
    }

    public static void Redirect() { }
    public static void Health() { }
}
`,
	})
	assertRoutes(t, got, []string{"GET /", "GET /health"})
}

// TestMinimalAPI_GroupScopeDoesNotLeakBetweenMethods pins the body scoping: a
// group variable bound in one method must not resolve a registration in another,
// which would silently give the second method's routes the first's prefix.
func TestMinimalAPI_GroupScopeDoesNotLeakBetweenMethods(t *testing.T) {
	got, _ := routeSet(t, map[string]string{
		"Api/Two.cs": `
namespace Acme.Api;

public static class Two
{
    public static void MapA(this IEndpointRouteBuilder app)
    {
        var api = app.MapGroup("api/first");
        api.MapGet("/x", HandlerA);
    }

    public static void MapB(this IEndpointRouteBuilder app)
    {
        // Same variable name, never bound in this body: it is the root builder
        // here, not the group from MapA.
        api.MapGet("/y", HandlerB);
    }

    public static void HandlerA() { }
    public static void HandlerB() { }
}
`,
	})
	assertRoutes(t, got, []string{"GET /api/first/x", "GET /y"})
}

// TestMinimalAPI_NonRouteMapCallsAreIgnored is the over-reach control. The
// string-literal-first-argument rule is what keeps MapControllers, MapHub<T> and
// MapRazorPages — which take no path — out of the route set, without needing a
// list of method names to exclude.
func TestMinimalAPI_NonRouteMapCallsAreIgnored(t *testing.T) {
	got, _ := routeSet(t, map[string]string{
		"Api/Wiring.cs": `
namespace Acme.Api;

public static class Wiring
{
    public static void MapAll(this IEndpointRouteBuilder app)
    {
        app.MapControllers();
        app.MapRazorPages();
        app.MapHub<ChatHub>("/hub");
        app.MapGet(BuildPath(), Handler);
        app.MapGet("/real", Handler);
    }

    public static void Handler() { }
    public static string BuildPath() => "/computed";
}
`,
	})
	// MapHub<T>("/hub") is a SignalR hub, not a minimal-API endpoint, but it is
	// indistinguishable here by argument shape — it is excluded because MapHub is
	// not in the verb table. A computed path is excluded because it is not a literal.
	assertRoutes(t, got, []string{"GET /real"})
}

func TestMinimalAPI_HandlerBindingAndLambdas(t *testing.T) {
	_, all := routeSet(t, map[string]string{
		"Api/Mixed.cs": `
namespace Acme.Api;

public static class Mixed
{
    public static void Map(this IEndpointRouteBuilder app)
    {
        app.MapGet("/named", GetNamed);
        app.MapGet("/inline", () => Results.Ok());
    }

    public static void GetNamed() { }
}
`,
	})
	var named, inline *facts.Fact
	for i := range all {
		if all[i].Kind != facts.KindRoute {
			continue
		}
		switch all[i].Name {
		case "/named":
			named = &all[i]
		case "/inline":
			inline = &all[i]
		}
	}
	if named == nil || inline == nil {
		t.Fatal("expected both routes")
	}
	want := "Api.Mixed.GetNamed"
	if named.Props["handler"] != want {
		t.Errorf("handler = %v, want %q", named.Props["handler"], want)
	}
	if !hasRel(named, facts.RelHandledBy, want) {
		t.Errorf("missing handled_by; got %v", named.Relations)
	}
	// A lambda is given a symbol of its own, which the route points at; one that
	// names no declared symbol would be fabricated, so the target must exist.
	lambda, _ := inline.Props["handler"].(string)
	if lambda != "Api.Mixed.handler:GET:9" || !hasRel(inline, facts.RelHandledBy, lambda) {
		t.Errorf("lambda route handler = %q, relations %v, want its lambda symbol", lambda, inline.Relations)
	}
	declared := false
	for _, f := range all {
		declared = declared || (f.Kind == facts.KindSymbol && f.Name == lambda)
	}
	if !declared {
		t.Errorf("the lambda route's handler %q is no symbol", lambda)
	}
}

// TestMinimalAPI_TopLevelStatements covers a Program.cs with no enclosing class,
// where the whole file is one implicit body.
func TestMinimalAPI_TopLevelStatements(t *testing.T) {
	got, _ := routeSet(t, map[string]string{
		"Program.cs": `
var builder = WebApplication.CreateBuilder(args);
var app = builder.Build();

var api = app.MapGroup("/api");
api.MapGet("/ping", () => "pong");
app.MapPost("/echo", () => "echo");

app.Run();
`,
	})
	assertRoutes(t, got, []string{"GET /api/ping", "POST /echo"})
}

// A lambda handler has no declaration of its own, so it is given a symbol, named
// by the route's verb and line, that carries the lambda's calls; the route is
// handled by it. A method-group handler keeps its method.
func TestMinimalAPI_LambdaHandlerIsASymbol(t *testing.T) {
	_, all := routeSet(t, map[string]string{
		"Host/Startup.cs": `
namespace Acme.Host;

public static class Endpoints
{
    public static void Map(IEndpointRouteBuilder app)
    {
        app.MapGet("/request-logs", () =>
        {
            return RequestLog.List();
        });
        app.MapPost("/upload/{filename}", async context =>
        {
            await Uploader.Save(context);
        });
        app.MapGet("/named", GetNamed);
    }

    public static void GetNamed() { }
}

public static class RequestLog { public static object List() { return null; } }
public static class Uploader { public static Task Save(object c) { return null; } }
`,
	})
	handlers := map[string]string{}
	symbols := map[string]facts.Fact{}
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = f
		}
		if f.Kind == facts.KindRoute {
			for _, r := range f.Relations {
				if r.Kind == facts.RelHandledBy {
					handlers[f.PropString("method")+" "+f.Name] = r.Target
				}
			}
		}
	}
	for route, call := range map[string]string{
		"GET /request-logs":       "RequestLog.List",
		"POST /upload/{filename}": "Uploader.Save",
	} {
		h := handlers[route]
		s, ok := symbols[h]
		if !ok {
			t.Errorf("%s handled_by %q, want a symbol for its lambda (have %v)", route, h, handlers)
			continue
		}
		if !s.PropBool("framework_registered") {
			t.Errorf("%s: lambda symbol %s is not framework_registered", route, h)
		}
		found := false
		for _, r := range s.Relations {
			if r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, call) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: lambda symbol %s relations = %v, want a call to %s", route, h, s.Relations, call)
		}
	}
	if !strings.HasSuffix(handlers["GET /named"], "Endpoints.GetNamed") {
		t.Errorf("GET /named handled_by %q, want the method group", handlers["GET /named"])
	}
}

// A handler named by a local function of the registering method is that function,
// given one symbol however many routes it serves, carrying its calls.
func TestMinimalAPI_LocalFunctionHandler(t *testing.T) {
	_, all := routeSet(t, map[string]string{
		"Debugger/Startup.cs": `
namespace Acme.Debugger;

public class Startup
{
    public void Configure(IApplicationBuilder app)
    {
        app.UseRouter(router =>
        {
            router.MapGet("/json", RewriteArray);
            router.MapGet("/devtools/page/{pageId}", ConnectProxy);
            router.MapGet("/devtools/browser/{pageId}", ConnectProxy);
        });

        async Task RewriteArray(HttpContext context)
        {
            await Proxy.Forward(context);
        }

        async Task ConnectProxy(HttpContext context)
        {
            await Proxy.Connect(context);
        }
    }
}

public static class Proxy
{
    public static Task Forward(object c) { return null; }
    public static Task Connect(object c) { return null; }
}
`,
	})
	handlers := map[string]string{}
	symbols := map[string]facts.Fact{}
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = f
		}
		if f.Kind == facts.KindRoute {
			for _, r := range f.Relations {
				if r.Kind == facts.RelHandledBy {
					handlers[f.Name] = r.Target
				}
			}
		}
	}
	for route, want := range map[string]string{
		"/json":                      "Debugger.Startup.Configure.RewriteArray",
		"/devtools/page/{pageId}":    "Debugger.Startup.Configure.ConnectProxy",
		"/devtools/browser/{pageId}": "Debugger.Startup.Configure.ConnectProxy",
	} {
		if handlers[route] != want {
			t.Errorf("%s handled_by %q, want %q (have %v)", route, handlers[route], want, handlers)
			continue
		}
		calls := 0
		for _, r := range symbols[want].Relations {
			if r.Kind == facts.RelCalls {
				calls++
			}
		}
		if calls == 0 {
			t.Errorf("%s relations = %v, want the local function's calls", want, symbols[want].Relations)
		}
	}
	n := 0
	for name := range symbols {
		if strings.HasSuffix(name, ".ConnectProxy") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ConnectProxy has %d symbols, want one for the routes it serves", n)
	}
}

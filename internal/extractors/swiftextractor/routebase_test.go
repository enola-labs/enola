package swiftextractor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const routeBaseSrc = `import Foundation

open class Request<Response> {
    public init(baseURL: String = Config.base,
                endpoint: String,
                method: HTTPMethod = .get,
                params: [String: Any] = [:]) {
        self.init(baseURL: baseURL, endpoint: endpoint, method: method, params: params)
    }

    public convenience init(host: Host, endpoint: String, gatewayEndpointPrefix: String? = nil, method: HTTPMethod = .get) {
        self.init(baseURL: "", endpoint: endpoint, method: method)
    }
}
`

const subclassSrc = `import Networking

extension Paths {
    final class AppVersion: Request<Version> {
        // GET
        init() {
            super.init(
                endpoint: "app_version",
                params: [:]
            )
        }
    }

    final class Follow: Request<Empty> {
        init(userID: Int) {
            super.init(endpoint: "users/\(userID)/follow", method: .post)
        }
    }

    final class Dynamic: Request<Empty> {
        init(path: String) {
            super.init(endpoint: path)
        }
    }
}

extension Paths {
    static var settings: Request<Settings> {
        .init(endpoint: "settings", params: [:])
    }

    static func link(token: String) -> Request<Empty> {
        let params = ["token": token]
        return .init(
            endpoint: "accounts/link",
            method: .post,
            params: params
        )
    }

    static func unrelated() -> Other {
        .init(endpoint: "not_a_request")
    }
}

final class Unrelated: NSObject {
    init() { super.init() }
}
`

const generatedSrc = `struct ItemsAPI {
    static func listRoute(page: Int? = nil) -> Request<ItemsResponse> {
        let host: Host = .gateway
        let endpoint = Paths.normalize("/items")
        return Request(
            host: host,
            endpoint: endpoint,
            gatewayEndpointPrefix: "/catalogue",
            method: .get
        )
    }

    static func markRoute(id: Int) -> Request<Empty> {
        var endpoint = Paths.normalize("/api/v1/items/{id}/mark")
        endpoint = endpoint.replacingOccurrences(of: "{id}", with: "\(id)")
        return Request<Empty>(host: .core, endpoint: endpoint, method: .put)
    }
}
`

func TestRouteBaseRequests(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Networking/Request.swift":         routeBaseSrc,
		"Feature/Paths+AppVersion.swift":   subclassSrc,
		"Feature/Generated/ItemsAPI.swift": generatedSrc,
		"Feature/Tests/FakeTests.swift":    `final class Fake: Request<X> { init() { super.init(endpoint: "fixture") } }`,
	}
	var rels []string
	for rel, src := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	got := map[string]bool{}
	bases := collectRouteBases(dir, rels)
	for _, rel := range rels {
		src, _ := os.ReadFile(filepath.Join(dir, rel))
		for _, f := range routeBaseFacts(string(src), rel, filepath.Dir(rel), bases) {
			got[f.PropString("method")+" "+f.Name] = true
		}
	}
	want := []string{
		"GET app_version",
		"POST users/{}/follow",
		"GET catalogue/items",
		"PUT /api/v1/items/{id}/mark",
		"GET settings",
		"POST accounts/link",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing route %q; got %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d routes %v, want %d", len(got), got, len(want))
	}
}

func TestNoRouteBaseNoRoutes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "A.swift")
	src := `final class A: B { init() { super.init(endpoint: "x") } }`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if fs := routeBaseFacts(src, "A.swift", ".", collectRouteBases(dir, []string{"A.swift"})); len(fs) != 0 {
		t.Errorf("got %d routes without a base type", len(fs))
	}
}

func TestIsSwiftTestFile(t *testing.T) {
	for path, want := range map[string]bool{
		"Tests/A.swift":                        true,
		"App/CoreTests/Fixtures/Gen/API.swift": true,
		"App/AppUITests/Flow.swift":            true,
		"Templates/Service.xctemplate/X.swift": true,
		"App/Core/LatestTests.swift":           true,
		"App/Core/API/Requests.swift":          false,
		"App/TestsHelpers/Requests.swift":      false,
	} {
		if got := isSwiftTestFile(path); got != want {
			t.Errorf("isSwiftTestFile(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRouteBaseRequests_CallerIsTheEnclosingDeclaration(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Networking/Request.swift": routeBaseSrc,
		"Feature/ItemsAPI.swift":   generatedSrc,
	})
	callers := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropSource) == facts.RouteSourceSwiftEndpoint {
			callers[f.Name] = f.PropString(facts.PropCaller)
		}
	}
	for path, want := range map[string]string{
		"catalogue/items":         "ItemsAPI.listRoute",
		"/api/v1/items/{id}/mark": "ItemsAPI.markRoute",
	} {
		if got := callers[path]; !strings.HasSuffix(got, want) {
			t.Errorf("caller of %s = %q, want suffix %q", path, got, want)
		}
	}
}

// Each feature module generates its own copy of a client; a chained static call on it
// binds to the copy in the caller's module, and no source text becomes a call target.
func TestStaticChainedCall_BindsTheNearestDeclaration(t *testing.T) {
	gen := `struct OrdersAPI {
    static func getOrderRoute(id: Int) -> Request<Order> {
        let endpoint = Paths.normalize("/orders/{id}")
        return Request(host: .core, endpoint: endpoint)
    }
}
`
	ff := extractRepo(t, map[string]string{
		"Networking/Request.swift":          routeBaseSrc,
		"Billing/Generated/OrdersAPI.swift": gen,
		"Orders/Generated/OrdersAPI.swift":  gen,
		"Orders/Scenes/OrderInteractor.swift": `final class OrderInteractor {
    func load(id: Int) {
        OrdersAPI
            .getOrderRoute(id: id)
            .result(completion: { _ in })
    }

    func cancel(id: Int) {
        Paths.Orders.cancel(id: id).result(completion: { _ in })
    }
}
`,
		"Orders/Routes/Paths+Orders.swift": `extension Paths {
    enum Orders {
        static func cancel(id: Int) -> Request<Empty> {
            .init(endpoint: "orders/\\(id)/cancel", method: .post)
        }
    }
}
`,
	})
	var load facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, "OrderInteractor.load") {
			load = f
		}
	}
	got := map[string]bool{}
	for _, r := range load.Relations {
		if r.Kind == facts.RelCalls {
			if strings.ContainsAny(r.Target, "\n( ") {
				t.Errorf("source text as call target: %q", r.Target)
			}
			got[r.Target] = true
		}
	}
	if !got["Orders/Generated.OrdersAPI.getOrderRoute"] {
		t.Errorf("load does not call its module's OrdersAPI.getOrderRoute; calls %v", got)
	}
	if got["Billing/Generated.OrdersAPI.getOrderRoute"] {
		t.Errorf("load calls another module's copy")
	}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, "OrderInteractor.cancel") {
			if !f.HasRelation(facts.RelCalls, "Orders/Routes.Paths.Orders.cancel") {
				t.Errorf("cancel does not call Paths.Orders.cancel: %v", f.Relations)
			}
		}
	}
}

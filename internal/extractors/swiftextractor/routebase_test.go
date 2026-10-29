package swiftextractor

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
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
	for _, f := range extractRouteBaseFacts(dir, rels, filepath.Dir) {
		got[f.PropString("method")+" "+f.Name] = true
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
	if fs := extractRouteBaseFacts(dir, []string{"A.swift"}, filepath.Dir); len(fs) != 0 {
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

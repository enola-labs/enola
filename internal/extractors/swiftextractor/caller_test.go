package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A URLSession request names the method whose body builds it, including when the
// request is sent from a closure that method passes to the session.
func TestURLSessionRoutes_CallerIsTheEnclosingMethod(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"App/Network/OrdersService.swift": `import Foundation

final class OrdersService {
    let baseURL: URL
    let session: URLSession

    func list() async throws -> [Order] {
        var request = URLRequest(url: baseURL.appendingPathComponent("api/orders"))
        request.httpMethod = "GET"
        let (data, _) = try await session.data(for: request)
        return try JSONDecoder().decode([Order].self, from: data)
    }

    func cancel(id: String, done: @escaping () -> Void) {
        session.dataTask(with: URLRequest(url: baseURL.appendingPathComponent("api/orders/cancel"))) { _, _, _ in
            done()
        }.resume()
    }
}
`,
	})
	symbols := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
	}
	callers := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropAny(facts.PropRole) == facts.RoleClient {
			callers[f.Name] = f.PropString(facts.PropCaller)
		}
	}
	for path, want := range map[string]string{
		"api/orders":        "App/Network.OrdersService.list",
		"api/orders/cancel": "App/Network.OrdersService.cancel",
	} {
		if got := callers[path]; got != want || !symbols[got] {
			t.Errorf("%s caller = %q, want the emitted symbol %q", path, got, want)
		}
	}
}

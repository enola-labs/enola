package swiftextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A request builder that interpolates a base defined elsewhere yields `{}/order`;
// the `// @endpoint` comment above it names the route, parameters included. A
// comment does not reach past the declaration it annotates.
func TestEndpointAnnotationNamesAnInterpolatedRoute(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Shop/API/Route.swift": `public class Route<T> {
    public init(endpoint: String, method: HTTPMethod = .get, params: [String: Any] = [:]) {}
}
`,
		"Sources/Shop/API/Shipment.swift": `extension Shipment {
    // @endpoint PUT /transactions/{transaction_id}/shipment/order
    func order() -> Route<Empty> {
        .init(
            endpoint: "\(endpoint)/order",
            method: .put
        )
    }

    // @endpoint GET /transactions/{transaction_id}/shipment
    func details() -> Route<Shipment> {
        .init(endpoint: "transactions/\(id)/shipment")
    }

    func label() -> Route<Label> {
        .init(endpoint: "\(endpoint)/label")
    }
}
`,
	})
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.File == "Sources/Shop/API/Shipment.swift" {
			got[f.Name] = f.PropString("method")
		}
	}
	want := map[string]string{
		"transactions/{transaction_id}/shipment/order": "PUT",
		"transactions/{transaction_id}/shipment":       "GET",
		"{}/label":                                     "GET",
	}
	for name, verb := range want {
		if got[name] != verb {
			t.Errorf("route %q method = %q, want %q (routes %v)", name, got[name], verb, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
}

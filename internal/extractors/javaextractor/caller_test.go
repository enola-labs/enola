package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A RestTemplate call names the method whose body makes it, a lambda inside that
// method included; a Feign route names the interface method declaring it.
func TestJavaClientCalls_NameTheirCaller(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/OrdersClient.java": `package app;

import org.springframework.web.client.RestTemplate;

public class OrdersClient {
    private final RestTemplate rest = new RestTemplate();

    public Order find(long id) {
        return rest.getForObject("/api/orders/{id}", Order.class, id);
    }

    public void cancelAll(java.util.List<Long> ids) {
        ids.forEach(id -> rest.postForObject("/api/orders/{id}/cancel", null, Void.class, id));
    }
}
`,
		"app/InventoryClient.java": `package app;

import org.springframework.cloud.openfeign.FeignClient;
import org.springframework.web.bind.annotation.GetMapping;

@FeignClient(name = "inventory")
public interface InventoryClient {
    @GetMapping("/api/inventory/items")
    java.util.List<Item> items();
}
`,
	})
	symbols := map[string]bool{}
	callers := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
		if f.Kind == facts.KindRoute && f.PropAny(facts.PropRole) == facts.RoleClient {
			callers[f.Name] = f.PropString(facts.PropCaller)
		}
	}
	for path, want := range map[string]string{
		"/api/orders/{id}":        "app.OrdersClient.find",
		"/api/orders/{id}/cancel": "app.OrdersClient.cancelAll",
		"/api/inventory/items":    "app.InventoryClient.items",
	} {
		if got := callers[path]; got != want || !symbols[got] {
			t.Errorf("%s caller = %q, want the emitted symbol %q (callers: %v)", path, got, want, callers)
		}
	}
}

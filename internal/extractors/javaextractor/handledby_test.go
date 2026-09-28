package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A mapped controller method is the route's handler; the route names the method's
// symbol fact, which the same walk emits under that exact name.
func TestSpring_RoutesHandledByTheMappedMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"web/ItemController.java": `package web;

import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
@RequestMapping("/api/items")
public class ItemController {
    @GetMapping("/{id}")
    public Item find(long id) { return null; }
}
`,
	})
	symbols := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
	}
	routes := factsByKind(ff, facts.KindRoute)
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %d", len(routes))
	}
	var target string
	for _, r := range routes[0].Relations {
		if r.Kind == facts.RelHandledBy {
			target = r.Target
		}
	}
	if target == "" || target != routes[0].Props["handler"] || !symbols[target] {
		t.Fatalf("handled_by = %q (handler %v), want the emitted method symbol", target, routes[0].Props["handler"])
	}
}

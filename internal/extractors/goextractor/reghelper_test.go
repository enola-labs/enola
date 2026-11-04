package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A service that registers every route through a helper of its own: the helper takes
// the router, the method, the path and an operation id. Each call is a route at the
// path its constant spells, built from another constant, and its handler is a method
// on a value a constructor builds, in a local or in place.
func TestRoutesRegisteredThroughAHelper(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.22\n",
		"web/register.go": `package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func RegisterEndpoint(r chi.Router, method, pattern, operationID string, h http.HandlerFunc) {
	r.Method(method, pattern, h)
}
`,
		"orders/handlers/handlers.go": `package handlers

import "net/http"

type ListOrders struct{}

func NewListOrders() *ListOrders { return &ListOrders{} }
func (h *ListOrders) Handle(w http.ResponseWriter, r *http.Request) {}

type CancelOrder struct{}

func NewCancelOrder() *CancelOrder { return &CancelOrder{} }
func (h *CancelOrder) Handle(w http.ResponseWriter, r *http.Request) {}
`,
		"orders/router.go": `package orders

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"example.com/shop/orders/handlers"
	"example.com/shop/web"
)

const (
	OrdersURL      = "/app/v1/orders"
	CancelOrderURL = OrdersURL + "/{order_id}/cancel"
)

func NewRouter() chi.Router {
	r := chi.NewRouter()
	list := handlers.NewListOrders()
	web.RegisterEndpoint(r, http.MethodGet, OrdersURL, "listOrders", list.Handle)
	web.RegisterEndpoint(r, http.MethodPost, CancelOrderURL, "cancelOrder", handlers.NewCancelOrder().Handle)
	return r
}
`,
	})
	got := map[string]facts.Fact{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute {
			got[f.PropString("method")+" "+f.Name] = f
		}
	}
	for key, want := range map[string][2]string{
		"GET /app/v1/orders":                    {"listOrders", "orders/handlers.ListOrders.Handle"},
		"POST /app/v1/orders/{order_id}/cancel": {"cancelOrder", "orders/handlers.CancelOrder.Handle"},
	} {
		f, ok := got[key]
		if !ok {
			t.Errorf("no route %s; have %v", key, keysOf(got))
			continue
		}
		if f.PropString("operationId") != want[0] {
			t.Errorf("%s operationId = %q, want %q", key, f.PropString("operationId"), want[0])
		}
		if f.PropString(facts.PropHandlerTarget) != want[1] {
			t.Errorf("%s handler_target = %q, want %q", key, f.PropString(facts.PropHandlerTarget), want[1])
		}
	}
}

func keysOf(m map[string]facts.Fact) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

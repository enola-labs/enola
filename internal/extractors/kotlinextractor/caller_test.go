package kotlinextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Retrofit route is declared by an interface method, so that method is its caller:
// the annotation and the declaration start on one row, and the route names the
// method's own symbol.
func TestRetrofitRoutes_CallerIsTheInterfaceMethod(t *testing.T) {
	repo := t.TempDir()
	rel := "app/src/main/java/com/example/api/OrdersApi.kt"
	src := `package com.example.api

import retrofit2.http.GET
import retrofit2.http.POST

interface OrdersApi {
    @GET("/api/orders")
    suspend fun list(): List<Order>

    @POST("/api/orders/{id}/cancel")
    suspend fun cancel(id: String)
}
`
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ff, err := New().Extract(context.Background(), repo, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	symbols := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
	}
	want := map[string]string{"/api/orders": "list", "/api/orders/{id}/cancel": "cancel"}
	seen := 0
	for _, f := range ff {
		method, ok := want[f.Name]
		if f.Kind != facts.KindRoute || !ok {
			continue
		}
		seen++
		caller := f.PropString(facts.PropCaller)
		if !symbols[caller] || filepath.Ext(caller) != "."+method {
			t.Errorf("%s caller = %q, want the emitted OrdersApi.%s symbol", f.Name, caller, method)
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d Retrofit routes", seen, len(want))
	}
}

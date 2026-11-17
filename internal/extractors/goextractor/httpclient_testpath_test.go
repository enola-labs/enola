package goextractor

import (
	"go/parser"
	"go/token"
	"testing"
)

// A test harness polls the service it started; the calls are not the
// application's requests. The same code outside a test tree is.
func TestHTTPClient_TestHarnessIsNotAClient(t *testing.T) {
	src := `package env

import "net/http"

func ready(base string) { http.Get(base + "/healthz/ready") }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "env.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := extractHTTPClientFacts(fset, f, "test/integration/env.go", "test/integration", nil); len(got) != 0 {
		t.Fatalf("a test harness emitted client routes: %+v", got)
	}
	if got := extractHTTPClientFacts(fset, f, "internal/probe/env.go", "internal/probe", nil); len(got) != 1 {
		t.Fatalf("the same call in application code: %d routes, want 1", len(got))
	}
}

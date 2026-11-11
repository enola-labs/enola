package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeRepo writes files under parent/name and returns the repository's path.
func writeRepo(t *testing.T, parent, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// endpoint_impact on a route another service calls names, under that caller, the
// calling service's own endpoints whose handlers reach the call: what stops working
// there when the route changes.
func TestEndpointImpact_CallerEndpoints(t *testing.T) {
	parent := t.TempDir()
	stock := writeRepo(t, parent, "stock", map[string]string{
		"go.mod": "module example.com/stock\n\ngo 1.22\n",
		"main.go": `package main

import "net/http"

func books(w http.ResponseWriter, r *http.Request) {}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /books", books)
	_ = http.ListenAndServe(":8080", mux)
}
`,
	})
	shelf := writeRepo(t, parent, "shelf", map[string]string{
		"go.mod": "module example.com/shelf\n\ngo 1.22\n",
		"client/client.go": `package client

import "net/http"

const stockBase = "http://stock:8080"

type Stock struct{}

func (s *Stock) Books() error {
	_, err := http.Post(stockBase+"/books", "application/json", nil)
	return err
}
`,
		"main.go": `package main

import (
	"net/http"

	"example.com/shelf/client"
)

type lister interface{ Books() error }

type handler struct{ stock lister }

func (h handler) list(w http.ResponseWriter, r *http.Request) { _ = h.stock.Books() }

func health(w http.ResponseWriter, r *http.Request) {}

func main() {
	h := handler{stock: &client.Stock{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /shelf", h.list)
	mux.HandleFunc("GET /health", health)
	_ = http.ListenAndServe(":8080", mux)
}
`,
	})
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_paths": []string{stock, shelf}}); res.IsError {
		t.Fatal(text(res))
	}
	raw := text(s.call(t, "endpoint_impact", map[string]any{"endpoint": "POST /books"}))
	var got struct {
		Callers []struct {
			File      string   `json:"file"`
			Repo      string   `json:"repo"`
			Symbols   []string `json:"calls_from"`
			Endpoints []struct {
				Method, Path, Precision string
			} `json:"endpoints"`
		} `json:"callers"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("endpoint_impact: %v\n%.400s", err, raw)
	}
	if len(got.Callers) != 1 {
		t.Fatalf("callers = %+v, want the shelf client\n%s", got.Callers, raw)
	}
	c := got.Callers[0]
	if c.Repo != "shelf" || len(c.Symbols) != 1 || c.Symbols[0] != "example.com/shelf/client.Stock.Books" {
		t.Errorf("caller = %+v, want shelf's client.Stock.Books", c)
	}
	var paths []string
	for _, e := range c.Endpoints {
		paths = append(paths, e.Method+" "+e.Path)
	}
	if len(paths) != 1 || paths[0] != "GET /shelf" {
		t.Errorf("caller endpoints = %v, want only GET /shelf (reached through the lister interface)", paths)
	}
}

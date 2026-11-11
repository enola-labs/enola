package server_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// traversed runs traverse in full mode and returns its nodes by name.
func traversed(t *testing.T, s *session, args map[string]any) map[string]struct{ ThroughInterface bool } {
	t.Helper()
	args["output_mode"] = "full"
	raw := text(s.call(t, "traverse", args))
	var full struct {
		Nodes []struct {
			Name             string `json:"name"`
			ThroughInterface bool   `json:"through_interface"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(raw), &full); err != nil {
		t.Fatalf("full output: %v\n%.300s", err, raw)
	}
	out := map[string]struct{ ThroughInterface bool }{}
	for _, n := range full.Nodes {
		out[n.Name] = struct{ ThroughInterface bool }{n.ThroughInterface}
	}
	return out
}

// Who calls a method that is only ever called through an interface: the walk goes
// on from the method to the interface method it implements and to that method's
// callers, here two interfaces deep, the second through a promoted method.
func TestTraverse_ReverseCrossesInterfaces(t *testing.T) {
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": writeComposedHandlerRepo(t)}); res.IsError {
		t.Fatal(text(res))
	}
	nodes := traversed(t, s, map[string]any{
		"start":     "example.com/library/internal/app/catalog.Service.List",
		"direction": "reverse",
		"max_depth": 6,
	})
	for name, through := range map[string]bool{
		"example.com/library/internal/http.Lister.List":           true,
		"example.com/library/internal/http.listHandler.ListBooks": false,
		"example.com/library/internal/http.Books.ListBooks":       true,
		"example.com/library/internal/http.Gateway.Books":         false,
	} {
		n, ok := nodes[name]
		if !ok {
			t.Errorf("reverse walk does not reach %s; got %v", name, nodes)
			continue
		}
		if n.ThroughInterface != through {
			t.Errorf("%s: through_interface = %v, want %v", name, n.ThroughInterface, through)
		}
	}
}

// What a handler depends on goes on past the interface it calls, to the
// implementation behind it.
func TestTraverse_ForwardCrossesInterfaces(t *testing.T) {
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": writeComposedHandlerRepo(t)}); res.IsError {
		t.Fatal(text(res))
	}
	nodes := traversed(t, s, map[string]any{
		"start":     "example.com/library/internal/http.Gateway.Books",
		"direction": "forward",
		"max_depth": 6,
	})
	for _, name := range []string{
		"example.com/library/internal/http.listHandler.ListBooks",
		"example.com/library/internal/app/catalog.Service.List",
		"example.com/library/internal/shelving.Order",
	} {
		if _, ok := nodes[name]; !ok {
			t.Errorf("forward walk does not reach %s; got %v", name, nodes)
		}
	}
	if !nodes["example.com/library/internal/http.listHandler.ListBooks"].ThroughInterface {
		t.Error("listHandler.ListBooks is reached through Books but not marked through_interface")
	}
}

// How a handler reaches code it calls only through interfaces: the path goes on
// from each interface method to the implementation behind it.
func TestFindPath_CrossesInterfaces(t *testing.T) {
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": writeComposedHandlerRepo(t)}); res.IsError {
		t.Fatal(text(res))
	}
	raw := text(s.call(t, "find_path", map[string]any{
		"from": "example.com/library/internal/http.Gateway.Books",
		"to":   "example.com/library/internal/shelving.Order",
	}))
	var res struct {
		Found bool `json:"found"`
		Path  []struct {
			Name             string `json:"name"`
			ThroughInterface bool   `json:"through_interface"`
		} `json:"path"`
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("find_path output: %v\n%.300s", err, raw)
	}
	if !res.Found {
		t.Fatalf("no path from the handler through two interfaces to shelving.Order:\n%s", raw)
	}
	through := map[string]bool{}
	for _, n := range res.Path {
		through[n.Name] = n.ThroughInterface
	}
	for _, name := range []string{
		"example.com/library/internal/http.listHandler.ListBooks",
		"example.com/library/internal/app/catalog.Service.List",
	} {
		if !through[name] {
			t.Errorf("%s should be on the path, reached through an interface; path: %+v", name, res.Path)
		}
	}
}

// A route reaches the table its handler's service queries: through the interface
// the handler calls, to the repository method whose query names the table.
func TestFindPath_RouteToTable(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"go.mod": "module example.com/library\n\ngo 1.22\n",
		"internal/store/store.go": `package store

import (
	"database/sql"
	"fmt"
)

const loansTable = "loans"

type Repo struct{ db *sql.DB }

func (r *Repo) Open() (*sql.Rows, error) {
	return r.db.Query(fmt.Sprintf("SELECT id FROM %s WHERE returned = 0", loansTable))
}
`,
		"internal/http/http.go": `package http

import (
	"database/sql"
	"net/http"
)

type Loans interface{ Open() (*sql.Rows, error) }

type Handler struct{ loans Loans }

func (h Handler) List(w http.ResponseWriter, r *http.Request) { _, _ = h.loans.Open() }

func Register(mux *http.ServeMux, h Handler) { mux.HandleFunc("GET /loans", h.List) }
`,
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := startInMemory(t)
	if res := s.call(t, "generate_snapshot", map[string]any{"repo_path": dir}); res.IsError {
		t.Fatal(text(res))
	}
	raw := text(s.call(t, "find_path", map[string]any{
		"from": "example.com/library/internal/http.Handler.List",
		"to":   "kind:storage loans",
	}))
	var res struct {
		Found bool `json:"found"`
		Path  []struct {
			Name string `json:"name"`
		} `json:"path"`
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("find_path output: %v\n%.300s", err, raw)
	}
	if !res.Found {
		t.Fatalf("no path from the handler to the loans table:\n%s", raw)
	}
	if last := res.Path[len(res.Path)-1].Name; last != "loans" {
		t.Errorf("path ends at %s, want the loans table", last)
	}
}

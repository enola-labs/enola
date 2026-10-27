package archbudget

// TestImportBudget is the ratchet for the two numbers this codebase grades its
// own structure by: how many of the module's packages each hub imports (its
// efferent coupling, Ce), and how long the longest import chain is.
//
// Those are the numbers a refactor of the hubs is meant to move, and the ones
// nothing else holds still. The declared layer order (enola-intent.yaml) says
// which DIRECTION an import may run; it has no opinion on how many there are, so
// a hub can grow an import a week without crossing a layer.
//
// The budgets are exact, not ceilings:
//   - over budget fails: the change added coupling to a hub. Remove the import,
//     or raise the number in the same change and say why in review.
//   - under budget fails too: the change removed coupling, so lower the number in
//     the same change. That is what makes it a ratchet. A ceiling left above the
//     real value is slack the next change spends without anyone seeing it.
//
// pendingCuts are imports that exist today and are scheduled to go. Each must
// still exist; when one is cut, move it to forbiddenEdges, which must not. An
// edge therefore cannot be cut and quietly come back.
//
// The graph is read with `go list`, so it is the graph the compiler sees on this
// host: non-test imports of every package under the module root.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/enola-labs/enola/"

// hubBudgets is the number of this module's packages each hub imports.
//
// pkg/bootstrap is the composition root and is deliberately absent: it imports
// every extractor, explainer and binder by design, so its count rises with each
// language added and says nothing about coupling.
//
// pkg/command is the same kind of package for the command line: its count is the
// union of what its commands need. It keeps a budget anyway, as a ceiling on that
// union, because splitting it into family packages would put one more layer
// between every command and the engine.
var hubBudgets = map[string]int{
	"internal/engine": 15,
	"internal/server": 19,
	"pkg/command":     32,
	"pkg/check":       11,
	"internal/config": 4,
	"pkg/dashboard":   6,
	"pkg/explain":     7,
	"cmd/enola":       14,
}

// maxChainBudget is the number of packages on the longest import chain. Today:
// cmd/enola -> pkg/command -> pkg/bootstrap -> internal/server ->
// internal/conformance -> internal/diff -> internal/explainers/constraints ->
// internal/explainers/common -> internal/facts -> internal/factpath.
const maxChainBudget = 10

type edge struct{ from, to string }

// pendingCuts exist today and are scheduled to be removed.
var pendingCuts = []edge{}

// forbiddenEdges were cut and must not return.
var forbiddenEdges = []edge{
	// The config validates a provider list against internal/providerspec. Reaching
	// the package that runs providers put it, and the rubydex engine behind it, on
	// every chain that passes through config.
	{"internal/config", "internal/providers"},
	// A view and a report take the slice of the engine they read as an interface.
	// Importing the composition root put every extractor the binary carries
	// behind both of them.
	{"pkg/dashboard", "pkg/bootstrap"},
	{"pkg/explain", "pkg/bootstrap"},
	// The engine hands a written snapshot to whoever is wired to it; recording it
	// as a revision, and the diff that summarizes it, belong to the history.
	{"internal/engine", "internal/history"},
	{"internal/engine", "pkg/history"},
	{"internal/engine", "internal/diff"},
}

// importGraph returns, for every package in the module, the module packages it
// imports, both as paths relative to the module root.
func importGraph(t *testing.T) map[string][]string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.mod above the test directory")
		}
		root = parent
	}

	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}} {{join .Imports " "}}`, "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}

	graph := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], modulePath) {
			continue
		}
		pkg := strings.TrimPrefix(fields[0], modulePath)
		seen := map[string]bool{}
		for _, imp := range fields[1:] {
			if rel := strings.TrimPrefix(imp, modulePath); rel != imp && !seen[rel] {
				seen[rel] = true
				graph[pkg] = append(graph[pkg], rel)
			}
		}
		sort.Strings(graph[pkg])
		if _, ok := graph[pkg]; !ok {
			graph[pkg] = nil
		}
	}
	return graph
}

// longestChain returns the longest import chain in the graph. Go refuses import
// cycles, so the graph is a DAG and the recursion terminates.
func longestChain(graph map[string][]string) []string {
	memo := map[string][]string{}
	var from func(pkg string) []string
	from = func(pkg string) []string {
		if chain, ok := memo[pkg]; ok {
			return chain
		}
		var best []string
		for _, dep := range graph[pkg] {
			if chain := from(dep); len(chain) > len(best) {
				best = chain
			}
		}
		chain := append([]string{pkg}, best...)
		memo[pkg] = chain
		return chain
	}

	pkgs := make([]string, 0, len(graph))
	for pkg := range graph {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	var longest []string
	for _, pkg := range pkgs {
		if chain := from(pkg); len(chain) > len(longest) {
			longest = chain
		}
	}
	return longest
}

func hasEdge(graph map[string][]string, e edge) bool {
	for _, dep := range graph[e.from] {
		if dep == e.to {
			return true
		}
	}
	return false
}

func TestImportBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list over the module")
	}
	graph := importGraph(t)

	hubs := make([]string, 0, len(hubBudgets))
	for hub := range hubBudgets {
		hubs = append(hubs, hub)
	}
	sort.Strings(hubs)
	for _, hub := range hubs {
		imports, ok := graph[hub]
		if !ok {
			t.Errorf("%s has a budget but is not a package in this module; remove or rename its entry", hub)
			continue
		}
		budget := hubBudgets[hub]
		switch got := len(imports); {
		case got > budget:
			t.Errorf("%s imports %d of this module's packages, budget %d. It now imports:\n  %s",
				hub, got, budget, strings.Join(imports, "\n  "))
		case got < budget:
			t.Errorf("%s imports %d of this module's packages, under its budget of %d: lower hubBudgets[%q] to %d",
				hub, got, budget, hub, got)
		}
	}

	chain := longestChain(graph)
	switch got := len(chain); {
	case got > maxChainBudget:
		t.Errorf("the longest import chain is %d packages, budget %d:\n  %s",
			got, maxChainBudget, strings.Join(chain, " ->\n  "))
	case got < maxChainBudget:
		t.Errorf("the longest import chain is %d packages, under its budget of %d: lower maxChainBudget to %d",
			got, maxChainBudget, got)
	}

	for _, e := range pendingCuts {
		if !hasEdge(graph, e) {
			t.Errorf("%s no longer imports %s: move this edge from pendingCuts to forbiddenEdges", e.from, e.to)
		}
	}
	for _, e := range forbiddenEdges {
		if hasEdge(graph, e) {
			t.Errorf("%s imports %s again; that edge was cut and must not return", e.from, e.to)
		}
	}
}

// The helpers are what the verdict rests on, so they are checked against a graph
// small enough to read.
func TestLongestChain(t *testing.T) {
	graph := map[string][]string{
		"a": {"b", "c"},
		"b": {"d"},
		"c": nil,
		"d": {"e"},
		"e": nil,
		"x": {"e"},
	}
	if got, want := fmt.Sprint(longestChain(graph)), "[a b d e]"; got != want {
		t.Errorf("longestChain = %s, want %s", got, want)
	}
	if !hasEdge(graph, edge{"a", "c"}) || hasEdge(graph, edge{"c", "a"}) || hasEdge(graph, edge{"nope", "a"}) {
		t.Error("hasEdge must follow direction and tolerate an unknown package")
	}
	if got := longestChain(map[string][]string{}); len(got) != 0 {
		t.Errorf("an empty graph has no chain; got %v", got)
	}
}

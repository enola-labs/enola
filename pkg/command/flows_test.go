package command

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// flowsRepo writes a program whose main serves orders through three packages,
// beside a second program that only prints.
func flowsRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for path, body := range map[string]string{
		"go.mod":            "module example.com/shop\n\ngo 1.21\n",
		"cmd/shop/main.go":  "package main\n\nimport \"example.com/shop/order\"\n\nfunc main() {\n\torder.Place()\n}\n",
		"cmd/hello/main.go": "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n",
		"order/order.go":    "package order\n\nimport (\n\t\"example.com/shop/pay\"\n\t\"example.com/shop/store\"\n)\n\n// Place takes an order.\nfunc Place() {\n\tpay.Charge()\n\tstore.Save()\n}\n",
		"pay/pay.go":        "package pay\n\nfunc Charge() {\n\tsign()\n}\n\nfunc sign() {}\n",
		"store/store.go":    "package store\n\nfunc Save() {}\n",
	} {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func runFlowsIn(t *testing.T, repo string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	restore := chdir(t, t.TempDir())
	defer restore()
	var out, errOut bytes.Buffer
	code = testRunner().runFlows(context.Background(), append(args, repo), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestFlows_ListsTheEntriesThatReachSomething(t *testing.T) {
	stdout, _, code := runFlowsIn(t, flowsRepo(t))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"1 flow from 2 entries (2 main); 1 reach nothing the graph follows",
		"cmd/shop.main",
		"reach: symbols the calls from the entry reach.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing should contain %q; got:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "cmd/hello.main") {
		t.Errorf("a main that calls nothing in the graph is not a flow; got:\n%s", stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line ends in spaces: %q", line)
		}
	}
}

func TestFlows_ShowDescribesOneFlow(t *testing.T) {
	stdout, _, code := runFlowsIn(t, flowsRepo(t), "--show", "shop")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"cmd/shop.main\n",
		"5 symbols, 4 modules, depth 3",
		"The program starts at shop.main. It hands off to order.Place, which calls",
		"Stages\n  order.Place",
		"    pay.Charge",
		"Modules\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("detail should contain %q; got:\n%s", want, stdout)
		}
	}
}

func TestFlows_JSONIsOneDocumentWithStages(t *testing.T) {
	stdout, _, code := runFlowsIn(t, flowsRepo(t), "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var doc struct {
		Roots  map[string]int `json:"roots"`
		Leaves int            `json:"leaves"`
		Total  int            `json:"total"`
		Flows  []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Summary string `json:"summary"`
			Entry   struct {
				ID   string `json:"id"`
				File string `json:"file"`
			} `json:"entry"`
			Stages []struct {
				Name   string `json:"name"`
				ID     string `json:"id"`
				Parent string `json:"parent"`
			} `json:"stages"`
		} `json:"flows"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout must be one JSON document and nothing else: %v\n%s", err, stdout)
	}
	if doc.Roots["main"] != 2 || doc.Leaves != 1 || doc.Total != 1 || len(doc.Flows) != 1 {
		t.Fatalf("roots %v, leaves %d, total %d, %d flows", doc.Roots, doc.Leaves, doc.Total, len(doc.Flows))
	}
	f := doc.Flows[0]
	if len(f.ID) != 32 || len(f.Entry.ID) != 32 || f.Entry.File != "cmd/shop/main.go" || f.Summary == "" {
		t.Errorf("flow = %+v", f)
	}
	if len(f.Stages) == 0 || f.Stages[0].Name != "order.Place" || f.Stages[0].Parent != "" {
		t.Fatalf("stages = %+v", f.Stages)
	}
	if len(f.Stages) < 2 || f.Stages[1].Parent != f.Stages[0].ID {
		t.Errorf("the second stage should sit under the first: %+v", f.Stages)
	}
}

func TestFlows_ThroughTellsNoFlowFromNoSymbol(t *testing.T) {
	repo := flowsRepo(t)

	stdout, _, code := runFlowsIn(t, repo, "--through", "pay.sign")
	if code != 0 || !strings.Contains(stdout, "1 flow reaches pay.sign") || !strings.Contains(stdout, "cmd/shop.main") {
		t.Errorf("exit %d; got:\n%s", code, stdout)
	}

	// hello's main is a symbol, and no flow reaches it: an answer, exit 0.
	stdout, _, code = runFlowsIn(t, repo, "--through", "cmd/hello.main")
	if code != 0 || !strings.Contains(stdout, "No flow reaches cmd/hello.main.") {
		t.Errorf("exit %d; got:\n%s", code, stdout)
	}

	// No such symbol is a question that could not be asked.
	stdout, stderr, code := runFlowsIn(t, repo, "--through", "no.such.symbol")
	if code != 2 || stdout != "" || !strings.Contains(stderr, `no symbol is named "no.such.symbol"`) {
		t.Errorf("exit %d, stdout %q, stderr:\n%s", code, stdout, stderr)
	}
}

func TestFlows_RefusesWhatItCannotPick(t *testing.T) {
	repo := flowsRepo(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown flow":     {[]string{"--show", "no-such-flow"}, `no flow is named "no-such-flow"`},
		"two modes":        {[]string{"--show", "shop", "--through", "pay.sign"}, "pass one"},
		"top out of range": {[]string{"--top", "0"}, "--top 0 is outside 1-500"},
	} {
		stdout, stderr, code := runFlowsIn(t, repo, tc.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr:\n%s", name, code, stdout, stderr)
		}
	}
}

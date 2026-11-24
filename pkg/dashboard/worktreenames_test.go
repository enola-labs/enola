package dashboard

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestPageNamesWorktreesByDirectory: worktrees of one repository share the
// remote-derived label, so every place the page names a repo shows the checkout
// directory, with the label as a secondary hint and the path on hover. The
// picker's option value stays the label, which keys the metrics data.
func TestPageNamesWorktreesByDirectory(t *testing.T) {
	view := packageMetricsView(sampleMultiRepo())
	view.RepoNames = map[string]string{"alpha": "alpha-wt2"}
	data := pageData{
		HasReceipt: true,
		Receipt:    &facts.Receipt{},
		HasGraph:   true,
		Graph: &facts.GraphReceipt{Repos: []facts.GraphRepoEntry{
			{Label: "alpha", Path: "/src/alpha-wt2"},
			{Label: "beta", Path: "/src/beta"},
		}},
		PackageMetrics: view,
		Instances: []instanceRow{{
			PID: 1, Binary: "enola", Repos: "alpha-wt2", RepoPaths: "/src/alpha-wt2",
		}},
	}
	tmpl, err := baseTmpl.Clone()
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	body := b.String()

	for _, want := range []string{
		`<td title="/src/alpha-wt2">alpha-wt2</td>`,                                   // session row, path on hover
		`<td title="/src/alpha-wt2">alpha-wt2 <span class="note">· alpha</span></td>`, // graph table, label as hint
		`<td title="/src/beta">beta</td>`,                                             // no hint when they agree
		`<option value="alpha" selected>alpha-wt2</option>`,                           // picker shows dir, keys by label
		`<option value="beta">beta</option>`,                                          // no name recorded: label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

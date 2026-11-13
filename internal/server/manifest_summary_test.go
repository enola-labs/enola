package server

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestManifestSummaryShowsWhatTheSourceWindowDoesNot(t *testing.T) {
	app := facts.Fact{Kind: facts.KindSymbol, Name: "apps/web.Application.web-eu1", Props: map[string]any{
		"k8s_kind":            "Application",
		"language":            "kubernetes",
		"destination_cluster": "eu1",
		"chart_version":       "1.42.0",
		"replicas":            6,
		// A store read back from JSON holds lists as []any.
		"images":      []any{"acme/web:1.1"},
		"value_files": []string{"apps/web/values.yaml", "apps/web/eu1-values.yaml"},
	}}
	want := "k8s_kind: Application\n" +
		"destination_cluster: eu1\n" +
		"chart_version: 1.42.0\n" +
		"images: acme/web:1.1\n" +
		"replicas: 6\n" +
		"value_files: apps/web/values.yaml, apps/web/eu1-values.yaml\n"
	if got := manifestSummary(app); got != want {
		t.Errorf("manifestSummary =\n%s\nwant\n%s", got, want)
	}
	code := facts.Fact{Kind: facts.KindSymbol, Name: "pkg.F", Props: map[string]any{"language": "go", "images": []string{"x"}}}
	if got := manifestSummary(code); got != "" {
		t.Errorf("a non-manifest fact got %q", got)
	}
}

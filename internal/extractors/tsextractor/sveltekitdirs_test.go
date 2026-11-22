package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A SvelteKit app in a workspace member (its own svelte.config.js), under a root
// that is not SvelteKit, is still SvelteKit: its $lib imports resolve, its
// +page files are routes, and its components carry framework sveltekit. Files
// outside the member stay plain Svelte.
func TestExtract_SvelteKitAppInWorkspaceMember(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"package.json":                            `{"name":"root","private":true,"devDependencies":{"typescript":"^5.0.0"}}`,
		"app/package.json":                        `{"devDependencies":{"@sveltejs/kit":"^2.0.0"}}`,
		"app/svelte.config.js":                    `export default {}`,
		"app/src/lib/components/CrateList.svelte": `<ol></ol>`,
		"app/src/routes/crates/+page.svelte": `<script lang="ts">
  import CrateList from '$lib/components/CrateList.svelte'
</script>

<CrateList></CrateList>
`,
		"other/Widget.svelte": `<div></div>`,
	}
	var rel []string
	for p, c := range files {
		abs := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(p) == ".svelte" {
			rel = append(rel, p)
		}
	}
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	page, ok := findFact(ff, "app/src/routes/crates.CratesPage")
	if !ok {
		t.Fatalf("expected page component; got %v", factNames(ff))
	}
	if page.Props["framework"] != "sveltekit" {
		t.Errorf("page framework = %v, want sveltekit", page.Props["framework"])
	}
	if !page.HasRelation(facts.RelCalls, "app/src/lib/components.CrateList") {
		t.Errorf("<CrateList> through $lib in the member did not reference the component; relations: %+v", page.Relations)
	}
	route := false
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == "/crates" && f.PropString("framework") == "sveltekit" {
			route = true
			if !f.HasRelation(facts.RelHandledBy, "app/src/routes/crates.CratesPage") {
				t.Errorf("route /crates is not handled by its page; relations: %+v", f.Relations)
			}
		}
	}
	if !route {
		t.Errorf("no SvelteKit route /crates for the member's +page.svelte; got %v", factNames(ff))
	}
	if w, ok := findFact(ff, "other.Widget"); !ok || w.Props["framework"] != "svelte" {
		t.Errorf("a component outside the member should stay framework svelte; got %v", w.Props)
	}
}

package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func setupSvelteProject(t *testing.T, files map[string]string, sveltekit bool) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgJSON := `{"dependencies": {"svelte": "^5.0.0"}}`
	if sveltekit {
		pkgJSON = `{"dependencies": {"svelte": "^5.0.0", "@sveltejs/kit": "^2.0.0"}}`
		if err := os.WriteFile(filepath.Join(dir, "svelte.config.js"), []byte(`export default {}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	for relPath, content := range files {
		absPath := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func extractSvelte(t *testing.T, files map[string]string, sveltekit bool) []facts.Fact {
	t.Helper()
	dir := setupSvelteProject(t, files, sveltekit)

	var relFiles []string
	for f := range files {
		relFiles = append(relFiles, f)
	}

	ext := New()
	result, err := ext.Extract(context.Background(), dir, relFiles)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return result
}

// --- SFC script block extraction ---

func TestExtractSvelteScriptBlocks_Instance(t *testing.T) {
	src := []byte(`<script lang="ts">
  let count = 0
</script>

<button on:click={() => count++}>{count}</button>
`)
	blocks := extractSvelteScriptBlocks(src)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].IsModule {
		t.Error("expected IsModule = false")
	}
	if blocks[0].Lang != "ts" {
		t.Errorf("lang = %q, want ts", blocks[0].Lang)
	}
}

func TestExtractSvelteScriptBlocks_Module(t *testing.T) {
	src := []byte(`<script module>
  export const prerender = true
</script>

<script lang="ts">
  let name = 'world'
</script>

<h1>Hello {name}</h1>
`)
	blocks := extractSvelteScriptBlocks(src)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if !blocks[0].IsModule {
		t.Error("first block should be module")
	}
	if blocks[1].IsModule {
		t.Error("second block should not be module")
	}
}

func TestExtractSvelteScriptBlocks_ContextModule(t *testing.T) {
	src := []byte(`<script context="module">
  export const prerender = true
</script>
`)
	blocks := extractSvelteScriptBlocks(src)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if !blocks[0].IsModule {
		t.Error("expected IsModule = true for context=\"module\"")
	}
}

func TestExtractSvelteScriptBlocks_NoScript(t *testing.T) {
	src := []byte(`<h1>Hello</h1>`)
	blocks := extractSvelteScriptBlocks(src)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(blocks))
	}
}

// --- Framework detection ---

func TestDetectSvelte(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"dependencies":{"svelte":"^5.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectSvelte(dir) {
		t.Error("expected detectSvelte = true")
	}
}

func TestDetectSvelteKit_Config(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "svelte.config.js"), []byte(`export default {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectSvelteKit(dir) {
		t.Error("expected detectSvelteKit = true")
	}
}

func TestDetectSvelteKit_PkgDep(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"devDependencies":{"@sveltejs/kit":"^2.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectSvelteKit(dir) {
		t.Error("expected detectSvelteKit = true")
	}
}

func TestStaticSvelteKitAliases_LiteralsOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svelte.config.js")
	config := `const docs = 'https://kit.svelte.dev';
	const unrelated = { alias: { wrong: "./wrong" } };
	export default {
		kit: {
			alias: {
				src: "./src",
				"@ui": "./packages/ui/src",
				dynamic: path.resolve("src/dynamic"),
				...sharedAliases
			}
		}
	}`
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	got := staticSvelteKitAliases(path)
	if got["src"] != "./src" || got["@ui"] != "./packages/ui/src" {
		t.Fatalf("literal aliases = %v", got)
	}
	if _, ok := got["dynamic"]; ok {
		t.Fatalf("dynamic alias must be skipped: %v", got)
	}
	if _, ok := got["wrong"]; ok {
		t.Fatalf("alias outside kit must be skipped: %v", got)
	}
}

func TestSvelteKitAliasFallbacks_TsconfigWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "svelte.config.js"), []byte(`export default { kit: { alias: { src: "./fallback", ui: "./src/ui" } } }`), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []tsAliasRoot{{dir: "", aliases: map[string]tsAlias{
		"src/": {replacement: "generated/src/"},
	}}}
	got := withSvelteKitAliasFallbacks(dir, roots)[0].aliases
	if got["src/"].replacement != "generated/src/" {
		t.Fatalf("config overrode tsconfig: %+v", got["src/"])
	}
	if got["ui/"].replacement != "src/ui/" {
		t.Fatalf("literal fallback missing: %+v", got["ui/"])
	}
	if got["$lib/"].replacement != "src/lib/" {
		t.Fatalf("$lib fallback missing: %+v", got["$lib/"])
	}
}

func TestSvelteKitVirtualImports(t *testing.T) {
	for _, path := range []string{"$app/navigation", "$env/static/private", "$service-worker"} {
		if !isSvelteKitVirtualImport(path) {
			t.Errorf("%q should be virtual", path)
		}
	}
	for _, path := range []string{"$lib/Button.svelte", "$application/x", "svelte"} {
		if isSvelteKitVirtualImport(path) {
			t.Errorf("%q should not be virtual", path)
		}
	}
}

// --- SvelteKit route detection ---

func TestDetectSvelteKitRoute(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantRoute  string
		wantType   string
		wantMethod string
	}{
		{"root page", "src/routes/+page.svelte", "/", "page", "GET"},
		{"nested page", "src/routes/about/+page.svelte", "/about", "page", "GET"},
		{"dynamic param", "src/routes/blog/[slug]/+page.svelte", "/blog/[slug]", "page", "GET"},
		{"layout", "src/routes/+layout.svelte", "/", "layout", "GET"},
		{"api route", "src/routes/api/users/+server.ts", "/api/users", "server", "ALL"},
		{"route group stripped", "src/routes/(app)/dashboard/+page.svelte", "/dashboard", "page", "GET"},
		{"error page", "src/routes/+error.svelte", "/", "error", "GET"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectSvelteKitRoute(tt.file)
			if got == nil {
				t.Fatal("expected route fact, got nil")
			}
			if got.Name != tt.wantRoute {
				t.Errorf("route = %q, want %q", got.Name, tt.wantRoute)
			}
			if got.Props["type"] != tt.wantType {
				t.Errorf("type = %v, want %s", got.Props["type"], tt.wantType)
			}
			if got.Props["method"] != tt.wantMethod {
				t.Errorf("method = %v, want %s", got.Props["method"], tt.wantMethod)
			}
			if got.Props["framework"] != "sveltekit" {
				t.Errorf("framework = %v, want sveltekit", got.Props["framework"])
			}
		})
	}
}

func TestDetectSvelteKitRoute_ServerLoad(t *testing.T) {
	got := detectSvelteKitRoute("src/routes/+page.server.ts")
	if got != nil {
		t.Error("+page.server.ts should not emit a route (it's a load function, not a route)")
	}
}

func TestDetectSvelteKitRoute_NonRoute(t *testing.T) {
	got := detectSvelteKitRoute("src/lib/components/Button.svelte")
	if got != nil {
		t.Error("non-route .svelte file should return nil")
	}
}

// --- Full extraction ---

func TestExtract_SvelteSFC(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/lib/Counter.svelte": `<script lang="ts">
  let count = $state(0)

  function increment() {
    count++
  }
</script>

<button onclick={increment}>{count}</button>
`,
	}, false)

	comp, ok := findFact(ff, "src/lib.Counter")
	if !ok {
		t.Fatalf("expected component fact src/lib.Counter; got %v", factNames(ff))
	}
	if comp.Props["web_component"] != "component" {
		t.Errorf("web_component = %v, want component", comp.Props["web_component"])
	}
	if comp.Props["framework"] != "svelte" {
		t.Errorf("framework = %v, want svelte", comp.Props["framework"])
	}

	inc, ok := findFact(ff, "src/lib.increment")
	if !ok {
		t.Fatalf("expected fact src/lib.increment; got %v", factNames(ff))
	}
	if inc.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("increment symbol_kind = %v, want function", inc.Props["symbol_kind"])
	}
}

func TestExtract_SvelteSFC_TemplateOnly(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/lib/Divider.svelte": `<hr/>`,
	}, false)

	comp, ok := findFact(ff, "src/lib.Divider")
	if !ok {
		t.Fatalf("expected component fact src/lib.Divider; got %v", factNames(ff))
	}
	if comp.Props["web_component"] != "component" {
		t.Errorf("web_component = %v, want component", comp.Props["web_component"])
	}
}

func TestExtract_SvelteKitPage(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/routes/about/+page.svelte": `<script lang="ts">
  const title = 'About'
</script>

<h1>{title}</h1>
`,
	}, true)

	comp, ok := findFact(ff, "src/routes/about.AboutPage")
	if !ok {
		t.Fatalf("expected component fact; got %v", factNames(ff))
	}
	if comp.Props["framework"] != "sveltekit" {
		t.Errorf("framework = %v, want sveltekit", comp.Props["framework"])
	}

	routes := findFactsByKind(ff, facts.KindRoute)
	var found bool
	for _, r := range routes {
		if r.Name == "/about" && r.Props["framework"] == "sveltekit" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sveltekit route /about; routes: %v", routes)
	}
}

func TestExtract_SvelteKit_LibAlias(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/routes/+page.svelte": `<script lang="ts">
  import { formatName } from '$lib/utils'
</script>

<h1>{formatName('world')}</h1>
`,
		"src/lib/utils.ts": `export function formatName(s: string) { return s }`,
	}, true)

	deps := findFactsByKind(ff, facts.KindDependency)
	found := false
	for _, d := range deps {
		for _, r := range d.Relations {
			if r.Target == "src/lib/utils" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected $lib/utils to resolve to src/lib/utils")
	}
}

// --- Markup reference pass (KindFileRef) ---
//
// A Svelte SFC's template is never fed to the tree-sitter script parser, so a
// function referenced only from markup — an event-handler attribute, a mustache
// call expression, or a bind:/use: directive — has no incoming edge and reads as
// dead to find_orphans. These assert the markup pass folds such references in via
// a KindFileRef fact, exactly like the TS extractor's JSX file-ref pass.

func TestExtractSvelteMarkupRefs_EventHandlerAttribute(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/Button.svelte": `<script lang="ts">
  function handleClick() {
    console.log('clicked');
  }
</script>

<button on:click={handleClick}>Click</button>
`,
	}, false)

	targets := fileRefTargets(ff, "src/Button.svelte")
	if !hasTarget(targets, "handleClick") {
		t.Errorf("Button.svelte file_ref should reference handleClick via on:click; got %v", targets)
	}
}

func TestExtractSvelteMarkupRefs_Svelte5EventAttribute(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/Nav.svelte": `<script lang="ts">
  function closeDrawer() {
    isOpen = false;
  }
  let isOpen = false;
</script>

<button onclick={closeDrawer}>Close</button>
`,
	}, false)

	targets := fileRefTargets(ff, "src/Nav.svelte")
	if !hasTarget(targets, "closeDrawer") {
		t.Errorf("Nav.svelte file_ref should reference closeDrawer via onclick; got %v", targets)
	}
}

func TestExtractSvelteMarkupRefs_MustacheCallExpression(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/Entry.svelte": `<script lang="ts">
  const formatDate = (d: string) => d;
  export let date = '';
</script>

<span>{formatDate(date)}</span>
`,
	}, false)

	targets := fileRefTargets(ff, "src/Entry.svelte")
	if !hasTarget(targets, "formatDate") {
		t.Errorf("Entry.svelte file_ref should reference formatDate via mustache call; got %v", targets)
	}
}

func TestExtractSvelteMarkupRefs_BindDirectiveShorthand(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/Dialog.svelte": `<script lang="ts">
  export let close: () => void;
</script>

<DialogContainer bind:close>
</DialogContainer>
`,
	}, false)

	targets := fileRefTargets(ff, "src/Dialog.svelte")
	if !hasTarget(targets, "close") {
		t.Errorf("Dialog.svelte file_ref should reference close via bind:close shorthand; got %v", targets)
	}
}

func TestExtractSvelteMarkupRefs_UseActionDirective(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/Tooltip.svelte": `<script lang="ts">
  function tooltip(node: HTMLElement) {
    return {};
  }
</script>

<div use:tooltip>Hover me</div>
`,
	}, false)

	targets := fileRefTargets(ff, "src/Tooltip.svelte")
	if !hasTarget(targets, "tooltip") {
		t.Errorf("Tooltip.svelte file_ref should reference tooltip via use:tooltip; got %v", targets)
	}
}

// A style block must never leak identifiers into the markup ref pass (CSS
// selectors/properties share no namespace with script symbols, but a naive scan
// could still tokenize them).
func TestExtractSvelteMarkupRefs_StyleBlockNotScanned(t *testing.T) {
	ff := extractSvelte(t, map[string]string{
		"src/Card.svelte": `<script lang="ts">
  function unusedHelper() {
    return 1;
  }
</script>

<div class="scroll">hi</div>

<style>
  .scroll {
    overflow: scroll;
  }
</style>
`,
	}, false)

	targets := fileRefTargets(ff, "src/Card.svelte")
	if hasTarget(targets, "scroll") {
		t.Errorf("Card.svelte file_ref should not pick up CSS tokens from <style>; got %v", targets)
	}
}

func TestIsSvelteFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"src/App.svelte", true},
		{"src/app.SVELTE", true},
		{"src/app.ts", false},
		{"src/app.vue", false},
	}
	for _, tt := range tests {
		if got := isSvelteFile(tt.path); got != tt.want {
			t.Errorf("isSvelteFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func setupVueProject(t *testing.T, files map[string]string, nuxt bool) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgJSON := `{"dependencies": {"vue": "^3.0.0"}}`
	if nuxt {
		pkgJSON = `{"dependencies": {"vue": "^3.0.0", "nuxt": "^3.0.0"}}`
		if err := os.WriteFile(filepath.Join(dir, "nuxt.config.ts"), []byte(`export default defineNuxtConfig({})`), 0o644); err != nil {
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

func extractVue(t *testing.T, files map[string]string, nuxt bool) []facts.Fact {
	t.Helper()
	dir := setupVueProject(t, files, nuxt)

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

func TestExtractVueScriptBlocks_Setup(t *testing.T) {
	src := []byte(`<template>
  <div>{{ msg }}</div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
const msg = ref('hello')
</script>
`)
	blocks := extractVueScriptBlocks(src)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if !blocks[0].IsSetup {
		t.Error("expected IsSetup = true")
	}
	if blocks[0].Lang != "ts" {
		t.Errorf("lang = %q, want ts", blocks[0].Lang)
	}
	if blocks[0].StartLine != 4 {
		t.Errorf("StartLine = %d, want 4", blocks[0].StartLine)
	}
}

func TestExtractVueScriptBlocks_NoSetup(t *testing.T) {
	src := []byte(`<script lang="ts">
import { defineComponent } from 'vue'
export default defineComponent({ name: 'MyComp' })
</script>
`)
	blocks := extractVueScriptBlocks(src)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].IsSetup {
		t.Error("expected IsSetup = false")
	}
	if blocks[0].Lang != "ts" {
		t.Errorf("lang = %q, want ts", blocks[0].Lang)
	}
}

func TestExtractVueScriptBlocks_Multiple(t *testing.T) {
	src := []byte(`<script lang="ts">
export interface Props { title: string }
</script>

<script setup lang="ts">
const props = defineProps<Props>()
</script>
`)
	blocks := extractVueScriptBlocks(src)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].IsSetup {
		t.Error("first block should not be setup")
	}
	if !blocks[1].IsSetup {
		t.Error("second block should be setup")
	}
}

func TestExtractVueScriptBlocks_NoScript(t *testing.T) {
	src := []byte(`<template><div>Hello</div></template>`)
	blocks := extractVueScriptBlocks(src)
	if len(blocks) != 0 {
		t.Fatalf("expected 0 blocks, got %d", len(blocks))
	}
}

// --- Framework detection ---

func TestDetectVue(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"dependencies":{"vue":"^3.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectVue(dir) {
		t.Error("expected detectVue = true")
	}
}

func TestDetectNuxt_Config(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nuxt.config.ts"), []byte(`export default {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectNuxt(dir) {
		t.Error("expected detectNuxt = true")
	}
}

func TestDetectNuxt_PkgDep(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"devDependencies":{"nuxt":"^3.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectNuxt(dir) {
		t.Error("expected detectNuxt = true")
	}
}

// --- Nuxt route detection ---

func TestDetectNuxtRoute(t *testing.T) {
	tests := []struct {
		name      string
		file      string
		wantRoute string
	}{
		{"index page", "pages/index.vue", "/"},
		{"about page", "pages/about.vue", "/about"},
		{"nested page", "pages/users/index.vue", "/users"},
		{"dynamic param", "pages/users/[id].vue", "/users/[id]"},
		{"deep nested", "pages/blog/posts/[slug].vue", "/blog/posts/[slug]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectNuxtRoute(tt.file)
			if got == nil {
				t.Fatal("expected route fact, got nil")
			}
			if got.Name != tt.wantRoute {
				t.Errorf("route = %q, want %q", got.Name, tt.wantRoute)
			}
			if got.Props["framework"] != "nuxt" {
				t.Errorf("framework = %v, want nuxt", got.Props["framework"])
			}
			if got.Props["router"] != "pages" {
				t.Errorf("router = %v, want pages", got.Props["router"])
			}
		})
	}
}

func TestDetectNuxtRoute_NonPage(t *testing.T) {
	if got := detectNuxtRoute("src/components/Button.vue"); got != nil {
		t.Error("non-page .vue file should return nil")
	}
	if got := detectNuxtRoute("pages/about.css"); got != nil {
		t.Error("unsupported page extension should return nil")
	}
}

// --- Full extraction: Vue SFC ---

func TestExtract_VueSFC_ScriptSetup(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/Counter.vue": `<template>
  <button @click="increment">{{ count }}</button>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const count = ref(0)

function increment() {
  count.value++
}
</script>
`,
	}, false)

	// Component fact
	comp, ok := findFact(ff, "src/components.Counter")
	if !ok {
		t.Fatalf("expected component fact src/components.Counter; got %v", factNames(ff))
	}
	if comp.Props["web_component"] != "component" {
		t.Errorf("web_component = %v, want component", comp.Props["web_component"])
	}
	if comp.Props["framework"] != "vue" {
		t.Errorf("framework = %v, want vue", comp.Props["framework"])
	}
	if comp.Props["vue_setup"] != true {
		t.Errorf("vue_setup = %v, want true", comp.Props["vue_setup"])
	}

	// Declarations from script block
	inc, ok := findFact(ff, "src/components.increment")
	if !ok {
		t.Fatalf("expected fact src/components.increment; got %v", factNames(ff))
	}
	if inc.Props["symbol_kind"] != facts.SymbolFunc {
		t.Errorf("increment symbol_kind = %v, want function", inc.Props["symbol_kind"])
	}

	// Import dependency
	deps := findFactsByKind(ff, facts.KindDependency)
	hasVueImport := false
	for _, d := range deps {
		for _, r := range d.Relations {
			if r.Target == "vue" {
				hasVueImport = true
			}
		}
	}
	if !hasVueImport {
		t.Error("expected import dependency on 'vue'")
	}
}

func TestExtract_VueSFC_DefineComponent(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/Modal.vue": `<template>
  <div class="modal"><slot/></div>
</template>

<script lang="ts">
import { defineComponent } from 'vue'

export default defineComponent({
  name: 'Modal',
  props: { visible: Boolean },
})
</script>
`,
	}, false)

	comp, ok := findFact(ff, "src/components.Modal")
	if !ok {
		t.Fatalf("expected component fact src/components.Modal; got %v", factNames(ff))
	}
	if comp.Props["web_component"] != "component" {
		t.Errorf("web_component = %v, want component", comp.Props["web_component"])
	}
	if comp.Props["framework"] != "vue" {
		t.Errorf("framework = %v, want vue", comp.Props["framework"])
	}
	if comp.Props["vue_setup"] == true {
		t.Error("vue_setup should not be true for non-setup script")
	}
}

func TestExtract_VueSFC_TemplateOnly(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/Divider.vue": `<template><hr/></template>`,
	}, false)

	comp, ok := findFact(ff, "src/components.Divider")
	if !ok {
		t.Fatalf("expected component fact src/components.Divider; got %v", factNames(ff))
	}
	if comp.Props["web_component"] != "component" {
		t.Errorf("web_component = %v, want component", comp.Props["web_component"])
	}
}

func TestExtract_VueSFC_LineOffset(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/App.vue": `<template>
  <div>hello</div>
</template>

<script setup lang="ts">
function greet() { return 'hi' }
</script>
`,
	}, false)

	f, ok := findFact(ff, "src/components.greet")
	if !ok {
		t.Fatalf("expected fact src/components.greet; got %v", factNames(ff))
	}
	// greet is on line 6 of the .vue file (1-based), script block starts at line 5
	if f.Line < 5 {
		t.Errorf("line = %d, expected >= 5 (script block offset should be applied)", f.Line)
	}
}

func TestExtract_VueTemplateReferencesScriptAndImportedComponents(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/UserCard.vue": `<template><article>User</article></template>`,
		"src/components/Dashboard.vue": `<script setup lang="ts">
import UserCard from './UserCard.vue'
function save() {}
const title = 'Dashboard'
</script>
<template>
  <UserCard :title="title" @submit="save" />
  <p>{{ title }}</p>
</template>`,
	}, false)

	dashboard, ok := findFact(ff, "src/components.Dashboard")
	if !ok {
		t.Fatalf("expected Dashboard component; got %v", factNames(ff))
	}
	for _, target := range []string{"src/components.UserCard", "src/components.save"} {
		if !dashboard.HasRelation(facts.RelCalls, target) {
			t.Errorf("Dashboard template lost reference to %s; relations: %+v", target, dashboard.Relations)
		}
	}
}

func TestExtract_VueTemplateResolvesRenamedDefaultImport(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/UserCard.vue": `<template><article>User</article></template>`,
		"src/components/Dashboard.vue": `<script setup lang="ts">
import Card from './UserCard.vue'
</script>
<template><Card /></template>`,
	}, false)
	dashboard, ok := findFact(ff, "src/components.Dashboard")
	if !ok {
		t.Fatal("expected Dashboard component")
	}
	if !dashboard.HasRelation(facts.RelCalls, "src/components.UserCard") {
		t.Errorf("renamed default import did not resolve to its file component: %+v", dashboard.Relations)
	}
	if dashboard.HasRelation(facts.RelCalls, "src/components.Card") {
		t.Errorf("renamed default import invented a Card declaration: %+v", dashboard.Relations)
	}
}

func TestExtract_VueTemplateResolvesNamedImportAlias(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/cards.ts": `export function UserCard() {}`,
		"src/components/Dashboard.vue": `<script setup lang="ts">
import { UserCard as Card } from './cards'
</script>
<template><Card /></template>`,
	}, false)
	dashboard, ok := findFact(ff, "src/components.Dashboard")
	if !ok || !dashboard.HasRelation(facts.RelCalls, "src/components.UserCard") {
		t.Fatalf("named import alias did not resolve to exported symbol: %+v", dashboard.Relations)
	}
}

func TestExtract_NuxtTemplateResolvesAutoImportedComponent(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"app/components/UserCard.vue": `<template><article>User</article></template>`,
		"app/pages/index.vue":         `<template><UserCard /></template>`,
	}, true)
	page, ok := findFact(ff, "app/pages.PagesIndex")
	if !ok {
		t.Fatalf("expected page component; got %v", factNames(ff))
	}
	if !page.HasRelation(facts.RelCalls, "app/components.UserCard") {
		t.Errorf("Nuxt auto-imported component was not resolved: %+v", page.Relations)
	}
}

func TestExtract_NuxtTemplateSkipsAmbiguousAutoImportedComponent(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"app/components/admin/Card.vue": `<template><article>Admin</article></template>`,
		"app/components/user/Card.vue":  `<template><article>User</article></template>`,
		"app/pages/index.vue":           `<template><Card /></template>`,
	}, true)
	page, ok := findFact(ff, "app/pages.PagesIndex")
	if !ok {
		t.Fatal("expected page component")
	}
	for _, r := range page.Relations {
		if r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, ".Card") {
			t.Errorf("ambiguous Nuxt basename was guessed: %+v", page.Relations)
		}
	}
}

func TestExtract_VueCompilerMacros(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/Field.vue": `<script setup lang="ts">
defineProps<{ label: string }>()
defineEmits<{ change: [value: string] }>()
defineSlots<{ default(): unknown }>()
defineModel<string>()
</script>
<template><slot /></template>`,
	}, false)
	field, ok := findFact(ff, "src.Field")
	if !ok {
		t.Fatal("expected Field component")
	}
	for _, prop := range []string{"vue_props", "vue_emits", "vue_slots", "vue_model"} {
		if field.Props[prop] != true {
			t.Errorf("%s = %v, want true; props: %+v", prop, field.Props[prop], field.Props)
		}
	}
	wants := map[string][]string{
		"vue_prop_names":  {"label"},
		"vue_emit_names":  {"change"},
		"vue_slot_names":  {"default"},
		"vue_model_names": {"modelValue"},
	}
	for prop, want := range wants {
		if got, _ := field.Props[prop].([]string); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", prop, got, want)
		}
	}
	declared, _ := field.Props["vue_contract_types"].([]string)
	for _, want := range []string{
		"props={ label: string }",
		"emits={ change: [value: string] }",
		"slots={ default(): unknown }",
		"model:modelValue=string",
	} {
		if !hasTarget(declared, want) {
			t.Errorf("vue_contract_types missing %q: %v", want, declared)
		}
	}
}

func TestExtract_VueCompilerMacroRuntimeAndReferencedContracts(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/Action.vue": `<script setup lang="ts">
interface Props { label: string; disabled?: boolean }
defineProps<Props>()
defineEmits(['save', 'cancel'])
defineModel<boolean>('checked')
defineExpose({ focus, reset })
function focus() {}
function reset() {}
</script><template><button>{{ label }}</button></template>`,
	}, false)
	action, ok := findFact(ff, "src.Action")
	if !ok {
		t.Fatal("expected Action component")
	}
	wants := map[string][]string{
		"vue_prop_names":    {"disabled", "label"},
		"vue_emit_names":    {"cancel", "save"},
		"vue_model_names":   {"checked"},
		"vue_exposed_names": {"focus", "reset"},
	}
	for prop, want := range wants {
		if got, _ := action.Props[prop].([]string); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", prop, got, want)
		}
	}
}

func TestExtract_VueCompilerMacrosIgnoreCommentsAndStrings(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/Plain.vue": `<script setup lang="ts">
// defineProps<{ fake: true }>()
const example = "defineEmits(['fake'])"
</script>
<template><p>{{ example }}</p></template>`,
	}, false)
	plain, ok := findFact(ff, "src.Plain")
	if !ok {
		t.Fatal("expected Plain component")
	}
	if _, exists := plain.Props["vue_macros"]; exists {
		t.Errorf("comment/string manufactured Vue macros: %+v", plain.Props)
	}
}

func TestExtract_VueTemplateIgnoresTextNativeTagsAndStyle(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/Card.vue": `<script setup lang="ts">
function scroll() {}
</script>
<template><div class="scroll">scroll</div></template>
<style>.scroll { overflow: scroll }</style>`,
	}, false)

	card, ok := findFact(ff, "src.Card")
	if !ok {
		t.Fatal("expected Card component")
	}
	if card.HasRelation(facts.RelCalls, "src.scroll") {
		t.Errorf("plain text/CSS invented a template call: %+v", card.Relations)
	}
}

// --- Composable classification ---

func TestExtract_VueComposable(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/composables/useAuth.ts": `export function useAuth() { return null }
export const useUser = () => null`,
	}, false)

	for _, name := range []string{"src/composables.useAuth", "src/composables.useUser"} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("expected fact %s", name)
		}
		if f.Props["web_component"] != "composable" {
			t.Errorf("%s web_component = %v, want composable", name, f.Props["web_component"])
		}
		if f.Props["framework"] != "vue" {
			t.Errorf("%s framework = %v, want vue", name, f.Props["framework"])
		}
	}
}

func TestExtract_NuxtComposable(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/composables/useFetch.ts": `export function useFetch() { return null }`,
	}, true)

	f, ok := findFact(ff, "src/composables.useFetch")
	if !ok {
		t.Fatalf("expected fact src/composables.useFetch; got %v", factNames(ff))
	}
	if f.Props["web_component"] != "composable" {
		t.Errorf("web_component = %v, want composable", f.Props["web_component"])
	}
	if f.Props["framework"] != "nuxt" {
		t.Errorf("framework = %v, want nuxt", f.Props["framework"])
	}
}

func TestExtract_NuxtApolloOperationsInVueScriptSetup(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"pages/books.vue": `<template><div>{{ data }}</div></template>

<script setup lang="ts">
import gql from 'graphql-tag'

const { data } = await useAsyncQuery(gql` + "`" + `
  query BooksPage {
    books { id title }
  }
` + "`" + `)

const { mutate } = useMutation(gql` + "`" + `
  mutation AddBook($title: String!) {
    addBook(title: $title) { id }
  }
` + "`" + `)
</script>
`,
	}, true)

	want := map[string]int{"Query.books": 7, "Mutation.addBook": 13}
	got := map[string]int{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.Props[facts.PropRouteType] != facts.RouteTypeGraphQL {
			continue
		}
		got[f.Name] = f.Line
		if f.Props[facts.PropRole] != facts.RoleClient || f.Props[facts.PropSource] != facts.RouteSourceGraphQLTag {
			t.Errorf("%s props = %v", f.Name, f.Props)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GraphQL routes = %v, want %v", got, want)
	}
}

func TestExtract_NuxtApolloIgnoresUntaggedVueTemplateStrings(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"pages/docs.vue": `<script setup lang="ts">
const example = ` + "`" + `query Fake { fake }` + "`" + `
</script>
<template><pre>{{ example }}</pre></template>`,
	}, true)

	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props[facts.PropRouteType] == facts.RouteTypeGraphQL {
			t.Fatalf("untagged example emitted GraphQL route: %+v", f)
		}
	}
}

// --- Nuxt page extraction ---

func TestExtract_NuxtPage(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"pages/about.vue": `<template><h1>About</h1></template>

<script setup lang="ts">
const title = 'About'
</script>
`,
	}, true)

	// Component fact
	comp, ok := findFact(ff, "pages.About")
	if !ok {
		t.Fatalf("expected component fact pages.About; got %v", factNames(ff))
	}
	if comp.Props["framework"] != "nuxt" {
		t.Errorf("framework = %v, want nuxt", comp.Props["framework"])
	}

	// Route fact
	routes := findFactsByKind(ff, facts.KindRoute)
	var found bool
	for _, r := range routes {
		if r.Name == "/about" && r.Props["framework"] == "nuxt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected nuxt route /about; routes: %v", routes)
	}
}

func TestDetectNuxtRoute_ModernConventions(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{"app/pages/(marketing)/about.vue", "/about"},
		{"app/pages/users/[id].ts", "/users/[id]"},
		{"app/pages/parent/child@sidebar.vue", "/parent/child"},
		{"app/pages/account.client.vue", "/account"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := detectNuxtRoute(tt.file)
			if got == nil || got.Name != tt.want {
				t.Fatalf("detectNuxtRoute(%q) = %#v, want %q", tt.file, got, tt.want)
			}
		})
	}
}

func TestExtract_NuxtTypeScriptPage(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"app/pages/status.ts": `export default defineComponent({ render: () => null })`,
	}, true)
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == "/status" {
			return
		}
	}
	t.Fatalf("expected route for a TypeScript Nuxt page; got %v", factNames(ff))
}

// --- Vue Router config detection ---

func TestExtract_VueRouterConfig(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/router/index.ts": `import { createRouter, createWebHistory } from 'vue-router'

const router = createRouter({
  history: createWebHistory(),
  routes: [],
})

export default router`,
	}, false)

	routes := findFactsByKind(ff, facts.KindRoute)
	var found bool
	for _, r := range routes {
		if r.Props["type"] == "router_config" && r.Props["framework"] == "vue" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected vue router_config route fact; routes: %v", routes)
	}
}

func TestExtract_VueRouterRecords(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/views/HomeView.vue":    `<template><h1>Home</h1></template>`,
		"src/views/UserView.vue":    `<template><h1>User</h1></template>`,
		"src/views/ProfileView.vue": `<template><h1>Profile</h1></template>`,
		"src/router/index.ts": `import { createRouter, createWebHistory } from 'vue-router'
import Home from '../views/HomeView.vue'

const children = [
  { path: 'profile', component: () => import('../views/ProfileView.vue') },
]
const routes = [
  { path: '/', component: Home },
  { path: '/users/:id', component: () => import('../views/UserView.vue'), children },
]
createRouter({ history: createWebHistory(), routes })`,
	}, false)
	wants := map[string]string{
		"/":                  "src/views.HomeView",
		"/users/:id":         "src/views.UserView",
		"/users/:id/profile": "src/views.ProfileView",
	}
	for path, handler := range wants {
		var found bool
		for _, f := range ff {
			if f.Kind == facts.KindRoute && f.Name == path && f.HasRelation(facts.RelHandledBy, handler) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing Vue Router route %s handled by %s", path, handler)
		}
	}
}

func TestExtract_NuxtAutoImportedComposable(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"app/composables/useAuth.ts": `export function useAuth() { return { user: null } }`,
		"app/pages/index.vue": `<script setup lang="ts">
const auth = useAuth()
</script><template><p>{{ auth.user }}</p></template>`,
	}, true)
	targets := fileRefTargets(ff, "app/pages/index.vue")
	if !hasTarget(targets, "app/composables.useAuth") {
		t.Errorf("top-level Nuxt auto-import call was not resolved: %v", targets)
	}
	if hasTarget(targets, "app/pages.useAuth") {
		t.Errorf("dangling same-directory composable target survived: %v", targets)
	}
}

func TestExtract_NuxtAmbiguousAutoImportedComposableIsNotGuessed(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"layers/a/composables/useAuth.ts": `export function useAuth() {}`,
		"layers/b/composables/useAuth.ts": `export function useAuth() {}`,
		"app/pages/index.vue":             `<script setup lang="ts">useAuth()</script><template><p /></template>`,
	}, true)
	targets := fileRefTargets(ff, "app/pages/index.vue")
	if hasTarget(targets, "layers/a/composables.useAuth") || hasTarget(targets, "layers/b/composables.useAuth") {
		t.Errorf("ambiguous Nuxt composable was guessed: %v", targets)
	}
}

// --- isVueFile ---

func TestIsVueFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"src/App.vue", true},
		{"src/app.VUE", true},
		{"src/app.ts", false},
		{"src/app.tsx", false},
	}
	for _, tt := range tests {
		if got := isVueFile(tt.path); got != tt.want {
			t.Errorf("isVueFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// --- isTypeScriptFile includes .vue ---

func TestIsTypeScriptFile_Vue(t *testing.T) {
	if !isTypeScriptFile("src/App.vue") {
		t.Error("isTypeScriptFile should return true for .vue files")
	}
}

// --- what a component references reaches the component ---

func TestExtract_VueSetupReferencesHangOffTheComponent(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"app/composables/paginator.ts": `export function usePaginator() { return {} }`,
		"app/components/Pager.vue": `<script setup lang="ts">
const p = usePaginator()
</script><template><div /></template>`,
		"app/pages/index.vue": `<template><Pager /></template>`,
	}, true)
	comp, ok := findFact(ff, "app/components.Pager")
	if !ok {
		t.Fatal("no component symbol for Pager.vue")
	}
	if !hasRelation(comp, facts.RelCalls, "app/composables.usePaginator") {
		t.Errorf("component does not reach the composable its setup calls: %v", comp.Relations)
	}
	page, _ := findFact(ff, "app/pages.PagesIndex")
	if !hasRelation(page, facts.RelCalls, "app/components.Pager") {
		t.Errorf("page does not reach the component it renders: %v", page.Relations)
	}
	// The file_ref stays: the dead-code detector reads top-level references from it.
	if !hasTarget(fileRefTargets(ff, "app/components/Pager.vue"), "app/composables.usePaginator") {
		t.Error("file_ref lost its reference")
	}
}

func TestExtract_VueOptionsScriptReferencesStayOffTheComponent(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/util.ts":  `export function helper() {}`,
		"src/Card.vue": "<script lang=\"ts\">\nimport { helper } from './util'\nhelper()\nexport default {}\n</script><template><div /></template>",
	}, false)
	comp, ok := findFact(ff, "src.Card")
	if !ok {
		t.Fatal("no component symbol for Card.vue")
	}
	if hasRelation(comp, facts.RelCalls, "src.helper") {
		t.Error("module scope of a plain <script> block was attributed to the component")
	}
}

// --- Pinia ---

func TestExtract_PiniaStores(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/stores/user.ts": `import { defineStore } from 'pinia'
import { useApi } from '../composables/api'
export const useUserStore = defineStore('user', () => {
  const api = useApi()
  return { api }
})`,
		"src/stores/requests.ts": `import { defineStore } from 'pinia'
export const useRequestsStore = defineStore({
  id: 'requests',
  state: () => ({ queue: [] }),
  actions: { push(id: string) { this.queue.push(id) } },
})`,
		"src/composables/api.ts": `export function useApi() { return {} }`,
		"src/composables/session.ts": `import { useUserStore } from '../stores/user'
export function useSession() { return useUserStore() }`,
	}, false)
	for _, name := range []string{"src/stores.useUserStore", "src/stores.useRequestsStore"} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("no symbol %s", name)
		}
		if f.Props["web_component"] != "store" || f.Props["framework"] != "pinia" {
			t.Errorf("%s: web_component=%v framework=%v, want store/pinia", name, f.Props["web_component"], f.Props["framework"])
		}
		if f.Props["symbol_kind"] != facts.SymbolVariable {
			t.Errorf("%s: symbol_kind=%v, want it left a variable", name, f.Props["symbol_kind"])
		}
	}
	user, _ := findFact(ff, "src/stores.useUserStore")
	if !hasRelation(user, facts.RelCalls, "src/composables.useApi") {
		t.Errorf("setup store does not reach what its setup function calls: %v", user.Relations)
	}
	if _, ok := findFact(ff, "src/stores.useRequestsStore.actions.push"); !ok {
		t.Error("options store lost its action member symbol")
	}
	session, _ := findFact(ff, "src/composables.useSession")
	if session.Props["web_component"] == "store" {
		t.Error("a composable that returns a store was classified as a store")
	}
}

func TestExtract_NuxtAutoImportedStore(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"app/stores/cart.ts":      `export const useCartStore = defineStore('cart', () => ({}))`,
		"app/components/Cart.vue": `<script setup lang="ts">const cart = useCartStore()</script><template><div /></template>`,
	}, true)
	comp, _ := findFact(ff, "app/components.Cart")
	if !hasRelation(comp, facts.RelCalls, "app/stores.useCartStore") {
		t.Errorf("auto-imported store under stores/ was not resolved: %v", comp.Relations)
	}
}

// --- app.component() registrations ---

func TestExtract_VueGlobalComponentRegistration(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/components/register.ts": `import VTable from './v-table/v-table.vue'
import VIcon from './v-icon.vue'
export function registerComponents(app) {
  app.component('VTable', VTable)
  app.component('v-icon', VIcon)
}`,
		"src/components/v-table/v-table.vue": `<template><table /></template>`,
		"src/components/v-icon.vue":          `<template><i /></template>`,
		"src/layouts/tabular.vue":            `<template><VTable /><v-icon /></template>`,
	}, false)
	layout, ok := findFact(ff, "src/layouts.Tabular")
	if !ok {
		t.Fatal("no component symbol for tabular.vue")
	}
	for _, want := range []string{"src/components/v-table.VTable", "src/components.VIcon"} {
		if !hasRelation(layout, facts.RelCalls, want) {
			t.Errorf("template does not reach globally registered %s: %v", want, layout.Relations)
		}
	}
}

func TestExtract_VueGlobalComponentRegistrationIsNotGuessed(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/a/register.ts": `import Btn from './btn.vue'
app.component('VButton', Btn)`,
		"src/b/register.ts": `import Btn from './btn.vue'
app.component('VButton', Btn)
const Other = make()
app.component('VOther', Other)`,
		"src/a/btn.vue": `<template><button /></template>`,
		"src/b/btn.vue": `<template><button /></template>`,
		"src/page.vue":  `<template><VButton /><VOther /></template>`,
	}, false)
	page, _ := findFact(ff, "src.SrcPage")
	for _, r := range page.Relations {
		if r.Kind == facts.RelCalls {
			t.Errorf("ambiguous or non-.vue registration was guessed: %s", r.Target)
		}
	}
}

func TestVueTemplateContent_NestedTemplates(t *testing.T) {
	src := []byte(`<script setup>const a = 1</script>
<template>
  <List>
    <template #header><Head /></template>
    <template v-if="a"><template v-for="x in xs"><Row /></template></template>
    <template v-if="pending" />
    <template v-else-if="n > 0"><Count /></template>
  </List>
  <Pager />
</template>
<style>.x{}</style>`)
	got := string(vueTemplateContent(src))
	for _, want := range []string{"<Head />", "<Row />", "<Count />", "<Pager />"} {
		if !strings.Contains(got, want) {
			t.Errorf("root template lost %s after a nested <template>: %q", want, got)
		}
	}
	if strings.Contains(got, "<style>") {
		t.Errorf("root template ran past its own close: %q", got)
	}
	if vueTemplateContent([]byte(`<template><div><template v-if="a"></div>`)) != nil {
		t.Error("an unbalanced template should produce nothing")
	}
}

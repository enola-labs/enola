package tsextractor

import (
	"slices"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// pageHandlers returns route path -> the sorted handled_by targets of each page route.
func pageHandlers(ff []facts.Fact) map[string][]string {
	out := map[string][]string{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.PropString("type") != "page" {
			continue
		}
		targets := []string{}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				targets = append(targets, r.Target)
			}
		}
		sort.Strings(targets)
		out[f.Name] = targets
	}
	return out
}

// A Nuxt page route is served by the component its file declares.
func TestFileRoutes_NuxtPageBoundToItsComponent(t *testing.T) {
	got := pageHandlers(extractAll(t, map[string]string{
		"package.json": `{"dependencies": {"nuxt": "^3.0.0", "vue": "^3.0.0"}}`,
		"pages/home.vue": `<script setup lang="ts">
const route = useRoute()
function refresh() {}
</script>

<template><div /></template>
`,
		"pages/users/[id].vue": `<script setup lang="ts">
const id = useRoute().params.id
</script>

<template><div /></template>
`,
	}, false))
	for path, want := range map[string][]string{
		"/home":       {"pages.Home"},
		"/users/[id]": {"pages/users.Id"},
	} {
		if !slices.Equal(got[path], want) {
			t.Errorf("%s handled_by = %v, want %v (have %v)", path, got[path], want, got)
		}
	}
}

// A SvelteKit page is served by its component and by the load beside it.
func TestFileRoutes_SvelteKitPageBoundToComponentAndLoad(t *testing.T) {
	got := pageHandlers(extractAllSvelteKit(t, map[string]string{
		"src/routes/article/[slug]/+page.svelte": `<script>
	export let data;
</script>

<h1>{data.article.title}</h1>
`,
		"src/routes/article/[slug]/+page.server.js": `export async function load({ params }) {
	return { article: await fetchArticle(params.slug) };
}

export const actions = {
	deleteArticle: async () => {}
};
`,
		"src/routes/about/+page.svelte": `<h1>About</h1>
`,
	}))
	if want := []string{"src/routes/article/[slug].SlugPage", "src/routes/article/[slug].load"}; !slices.Equal(got["/article/[slug]"], want) {
		t.Errorf("/article/[slug] handled_by = %v, want %v", got["/article/[slug]"], want)
	}
	if len(got["/about"]) != 1 {
		t.Errorf("/about handled_by = %v, want its component alone", got["/about"])
	}
}

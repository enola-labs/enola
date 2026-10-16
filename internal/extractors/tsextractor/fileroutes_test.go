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

// A SvelteKit form action is a POST to its page, served by the action: the default
// action at the page's path, a named one at `?/<name>`, the URL its form posts to.
// It is the page's own form that posts it, so it is no server route of the index.
func TestFileRoutes_SvelteKitFormActionsArePostRoutes(t *testing.T) {
	ff := extractAllSvelteKit(t, map[string]string{
		"src/routes/editor/+page.svelte": `<form method="POST"><button>Publish</button></form>
`,
		"src/routes/editor/+page.js": `export const ssr = false;
`,
		"src/routes/editor/+page.server.js": `export async function load({ locals }) {}

export const actions = {
	default: async ({ request }) => {
		await request.formData();
	}
};
`,
		"src/routes/(app)/article/[slug]/+page.svelte": `<form method="POST" action="?/deleteArticle"><button>Delete</button></form>
`,
		"src/routes/(app)/article/[slug]/+page.server.ts": `export const actions = {
	deleteArticle: async () => {},
	toggleFavorite: async ({ request }) => {}
};
`,
		"src/routes/about/+page.svelte": `<h1>About</h1>
`,
	})
	got := map[string][]string{}
	count := map[string]int{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.PropString("type") != "action" {
			continue
		}
		key := f.PropString("method") + " " + f.Name
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				got[key] = append(got[key], r.Target)
			}
		}
		if _, ok := got[key]; !ok {
			got[key] = nil
		}
		count[key]++
	}
	want := map[string][]string{
		"POST /editor":                         {"src/routes/editor.actions.default"},
		"POST /article/[slug]?/deleteArticle":  {"src/routes/(app)/article/[slug].actions.deleteArticle"},
		"POST /article/[slug]?/toggleFavorite": {"src/routes/(app)/article/[slug].actions.toggleFavorite"},
	}
	for key, targets := range want {
		if !slices.Equal(got[key], targets) {
			t.Errorf("%s handled_by = %v, want %v (have %v)", key, got[key], targets, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("action routes = %v, want exactly %v", got, want)
	}
	for key, n := range count {
		if n != 1 {
			t.Errorf("%s emitted %d times, want once", key, n)
		}
	}
}

// A Next.js page is served by its component, the one its file default-exports even
// beside generateMetadata, and an App Router route.ts serves one route per verb it
// exports, each handled by that function.
func TestFileRoutes_NextJSPagesAndRouteHandlers(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/app/waitlist/page.tsx": `export const dynamic = "force-dynamic";

export default function WaitlistPage() {
	return <div />;
}
`,
		"src/app/(app)/datasets/[id]/page.tsx": `export async function generateMetadata() {
	return { title: "Dataset" };
}

export default function DatasetPage() {
	return <div />;
}
`,
		"src/app/api/visualize/route.ts": `export async function GET(request: Request) {
	return Response.json({});
}

export async function POST(request: Request) {
	return Response.json({});
}
`,
		"src/app/api/health/route.ts": `export const dynamic = "force-dynamic";
`,
		"src/pages/about.tsx": `export default function About() {
	return <div />;
}
`,
	}, true)
	got := map[string][]string{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.PropString("framework") != "nextjs" {
			continue
		}
		key := f.PropString("method") + " " + f.Name
		targets := []string{}
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				targets = append(targets, r.Target)
			}
		}
		got[key] = targets
	}
	want := map[string][]string{
		"GET /waitlist":       {"src/app/waitlist.WaitlistPage"},
		"GET /datasets/[id]":  {"src/app/(app)/datasets/[id].DatasetPage"},
		"GET /api/visualize":  {"src/app/api/visualize.GET"},
		"POST /api/visualize": {"src/app/api/visualize.POST"},
		"ALL /api/health":     {},
		"GET /about":          {"src/pages.About"},
	}
	for key, targets := range want {
		have, ok := got[key]
		if !ok {
			t.Errorf("no route %s; have %v", key, got)
			continue
		}
		if !slices.Equal(have, targets) {
			t.Errorf("%s handled_by = %v, want %v", key, have, targets)
		}
	}
	if _, ok := got["ALL /api/visualize"]; ok {
		t.Errorf("route.ts with verbs keeps its ALL route; have %v", got)
	}
}

// In a workspace the package that depends on next is the Next.js app, wherever it
// sits: web/'s pages are routes although the root package.json declares no next,
// and a sibling package's app/ directory is not. A page that re-exports its screen
// (`export { default } from "@/views/..."`) is handled by that module's default
// export, and a test's page object under tests/e2e/pages is no route. A Next.js app
// under examples/ or example-apps/ is a demo of the repository, not one of its apps.
func TestFileRoutes_NextJSPackageInAWorkspace(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"package.json":     `{"name": "onyx", "private": true, "workspaces": ["web", "desktop"]}`,
		"web/package.json": `{"name": "web", "dependencies": {"next": "16.3.3", "react": "19.0.0"}}`,
		"web/src/app/settings/page.tsx": `export default function SettingsPage() {
	return <div />;
}
`,
		"web/src/app/api/health/route.ts": `export async function GET() {
	return Response.json({});
}
`,
		"web/src/app/admin/agents/page.tsx": `export { default } from "@/views/admin/AgentsPage";
`,
		"web/src/views/admin/AgentsPage.tsx": `export function AgentRow() {
	return <tr />;
}

export default function AgentsPage() {
	return <AgentRow />;
}
`,
		"web/src/app/admin/groups/page.tsx": `export { default } from "../../../views/admin/GroupsPage";
`,
		"web/src/views/admin/GroupsPage.tsx": `const GroupsPage = () => {
	return <div />;
};

export default GroupsPage;
`,
		"web/tests/e2e/pages/AdminAgentsPage.ts": `export class AdminAgentsPage {
	open() {}
}
`,
		"examples/with-nextjs/package.json": `{"name": "with-nextjs", "dependencies": {"next": "14.0.0"}}`,
		"examples/with-nextjs/src/app/page.tsx": `export default function Demo() {
	return <div />;
}
`,
		"example-apps/credential-sync/package.json": `{"name": "credential-sync", "dependencies": {"next": "14.0.0"}}`,
		"example-apps/credential-sync/app/api/sync/route.ts": `export async function POST() {
	return Response.json({});
}
`,
		"desktop/package.json": `{"name": "desktop", "dependencies": {"electron": "30.0.0"}}`,
		"desktop/src/app/page.tsx": `export default function Shell() {
	return <div />;
}
`,
	}, false)
	got := map[string][]string{}
	for _, f := range ff {
		for _, p := range []string{propDefaultExport, propDefaultFrom} {
			if f.PropAny(p) != nil {
				t.Errorf("%s keeps the working prop %s", f.Name, p)
			}
		}
		if f.Kind != facts.KindRoute || f.PropString("framework") != "nextjs" {
			continue
		}
		key := f.PropString("method") + " " + f.Name
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				got[key] = append(got[key], r.Target)
			}
		}
		if _, ok := got[key]; !ok {
			got[key] = nil
		}
	}
	want := map[string][]string{
		"GET /settings":     {"web/src/app/settings.SettingsPage"},
		"GET /api/health":   {"web/src/app/api/health.GET"},
		"GET /admin/agents": {"web/src/views/admin.AgentsPage"},
		"GET /admin/groups": {"web/src/views/admin.GroupsPage"},
	}
	for key, targets := range want {
		if !slices.Equal(got[key], targets) {
			t.Errorf("%s handled_by = %v, want %v (have %v)", key, got[key], targets, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("Next.js routes = %v, want exactly %v", got, want)
	}
}

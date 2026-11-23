package tsextractor

import "testing"

// Laravel Mix declares its aliases with mix.alias({...}), or hands webpack a
// resolve.alias through mix.webpackConfig; webpack applies them to every import.
func TestLaravelMixAliases(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"webpack.mix.js": `let mix = require('laravel-mix');
const path = require("path");
mix.js('resources/js/app.js', 'public/js').vue({ version: 2 });
mix.alias({
    '@': path.join(__dirname, 'resources/components'),
    '~': path.join(__dirname, 'resources/js/components'),
});
mix.webpackConfig({ resolve: { alias: { '#lib': path.resolve(__dirname, 'resources/lib') } } });
`,
		"resources/components/groups/Loader.vue":    "<template><div /></template>\n<script>\nexport default {}\n</script>\n",
		"resources/js/components/partials/Card.vue": "<template><div /></template>\n<script>\nexport default {}\n</script>\n",
		"resources/lib/util.js":                     "export function util() {}\n",
		"resources/components/Page.vue": `<template><loader-component /></template>
<script>
import LoaderComponent from '@/groups/Loader.vue';
import StatusCard from '~/partials/Card.vue';
import { util } from '#lib/util';
export default { components: { 'loader-component': LoaderComponent, StatusCard } }
</script>
`,
	}, false)
	for line, want := range map[int]string{
		3: "resources/components/groups/Loader.vue",
		4: "resources/js/components/partials/Card.vue",
		5: "resources/lib/util",
	} {
		dep := depAt(t, ff, "resources/components/Page.vue", line)
		if got := depTarget(dep); got != want {
			t.Errorf("line %d: import resolves to %q, want %q (%v)", line, got, want, dep.Props)
		}
		if dep.Props["resolved_via"] != aliasViaMix {
			t.Errorf("line %d: resolved_via %v, want %s", line, dep.Props["resolved_via"], aliasViaMix)
		}
	}
	if !hasTarget(fileRefTargets(ff, "resources/components/Page.vue"), "resources/components/groups.Loader") {
		t.Errorf("a component imported through a Mix alias under another name is not used: %v", fileRefTargets(ff, "resources/components/Page.vue"))
	}
}

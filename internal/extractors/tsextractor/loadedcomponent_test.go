package tsextractor

import "testing"

// A component registered from a require() of its file is used: the file loads
// it and hands its default export to the framework.
func TestLoadedComponent_RequireOfVueFile(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/app.js":                        "Vue.component('compose-classic', require('./components/ComposeClassic.vue').default);\n",
		"src/components/ComposeClassic.vue": "<template><div /></template>\n<script>\nexport default { name: 'compose' }\n</script>\n",
	}, false)
	if !hasTarget(fileRefTargets(ff, "src/app.js"), "src/components.ComposeClassic") {
		t.Errorf("a required .vue file is a use of its component; got %v", fileRefTargets(ff, "src/app.js"))
	}
}

// A route table's lazy component, `() => import('./Home.vue')`, is a use of the
// component, and so is an extensionless import() that resolves to a .vue file.
func TestLoadedComponent_DynamicImportOfVueFile(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/router.js": "const Home = () => import(/* webpackChunkName: \"home\" */ './views/Home.vue');\n" +
			"export const routes = [{ path: '/', component: Home }, { path: '/about', component: () => import('./views/About') }];\n",
		"src/views/Home.vue":  "<template><div /></template>\n<script>\nexport default {}\n</script>\n",
		"src/views/About.vue": "<template><div /></template>\n<script>\nexport default {}\n</script>\n",
	}, false)
	targets := fileRefTargets(ff, "src/router.js")
	for _, want := range []string{"src/views.Home", "src/views.About"} {
		if !hasTarget(targets, want) {
			t.Errorf("a dynamically imported .vue file is a use of %s; got %v", want, targets)
		}
	}
}

// A require() run for its side effects names no export of the module it loads.
func TestLoadedComponent_RequireOfScriptIsNotAComponentUse(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/app.js":       "require('./bootstrap');\n",
		"src/bootstrap.js": "export default function Bootstrap() {}\n",
	}, false)
	if targets := fileRefTargets(ff, "src/app.js"); hasTarget(targets, "src.Bootstrap") {
		t.Errorf("a side-effect require is no use of the default export: %v", targets)
	}
}

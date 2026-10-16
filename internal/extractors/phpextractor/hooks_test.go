package phpextractor

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// routesByName indexes route facts by name.
func routesByName(result []facts.Fact) map[string]facts.Fact {
	m := make(map[string]facts.Fact)
	for _, f := range result {
		if f.Kind == facts.KindRoute {
			m[f.Name] = f
		}
	}
	return m
}

func TestExtractHooks_ActionsAndFilters(t *testing.T) {
	src := `<?php
add_action('init', 'my_setup');
add_filter('the_content', 'my_filter', 10, 1);
do_action('my_custom_hook', $arg);
$out = apply_filters('the_title', $title);
register_rest_route('ns/v1', '/items', ['methods' => 'GET']);
`
	routes := routesByName(extractHooks([]byte(src), "wp-content/plugins/acme/acme.php"))

	tests := []struct {
		name     string
		method   string
		callback string
	}{
		{"ACTION init", "ACTION", "my_setup"},
		{"FILTER the_content", "FILTER", "my_filter"},
		{"ACTION my_custom_hook", "ACTION", ""},
		{"FILTER the_title", "FILTER", ""},
		{"REST ns/v1", "REST", ""},
	}
	for _, tc := range tests {
		r, ok := routes[tc.name]
		if !ok {
			t.Errorf("missing route %q; got %v", tc.name, keysRoutes(routes))
			continue
		}
		if r.Props["method"] != tc.method {
			t.Errorf("%s method = %v, want %s", tc.name, r.Props["method"], tc.method)
		}
		if r.Props["framework"] != "wordpress" {
			t.Errorf("%s framework = %v, want wordpress", tc.name, r.Props["framework"])
		}
		if tc.callback != "" && r.Props["callback"] != tc.callback {
			t.Errorf("%s callback = %v, want %s", tc.name, r.Props["callback"], tc.callback)
		}
		if !hasRel(r, facts.RelDeclares, "wp-content/plugins/acme") {
			t.Errorf("%s should declare its module dir; relations %v", tc.name, r.Relations)
		}
	}
}

func TestExtractHooks_DynamicHookNameSkipped(t *testing.T) {
	src := `<?php
add_action($dynamic, 'cb');
add_action("prefix_{$suffix}", 'cb2');
`
	routes := extractHooks([]byte(src), "plugin.php")
	if len(routes) != 0 {
		t.Errorf("dynamic hook names should be skipped; got %v", routes)
	}
}

func keysRoutes(m map[string]facts.Fact) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A hook registration is handled by the function its string callback names, or the
// method of a 'Class::method' callback; a callback naming no symbol of the
// repository binds nothing, and a hook point (do_action, apply_filters) has no
// handler.
func TestWordPressHookRegistrationsAreHandled(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"wp-settings.php": "<?php\n",
		"wp-includes/default-filters.php": `<?php
add_action('init', 'wp_widgets_init');
add_filter('the_content', 'WP_Embed::autoembed');
add_action('shutdown', 'not_declared_here');
do_action('init');
`,
		"wp-includes/widgets.php": `<?php
function wp_widgets_init() {
	register_sidebar_widgets();
}
`,
		"wp-includes/class-wp-embed.php": `<?php
class WP_Embed {
	public static function autoembed($content) {
		return $content;
	}
}
`,
	}
	rels := make([]string, 0, len(files))
	for rel, content := range files {
		writeFile(t, dir, rel, content)
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), dir, rels)
	if err != nil {
		t.Fatal(err)
	}
	handled := map[string][]string{}
	for _, f := range ff {
		if f.Kind != facts.KindRoute || f.PropString("framework") != "wordpress" {
			continue
		}
		key := f.Name + " " + f.PropString("hook")
		handled[key] = nil
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				handled[key] = append(handled[key], r.Target)
			}
		}
	}
	want := map[string][]string{
		"ACTION init add_action":        {"wp_widgets_init"},
		"FILTER the_content add_filter": {"WP_Embed::autoembed"},
		"ACTION shutdown add_action":    nil,
		"ACTION init do_action":         nil,
	}
	for key, targets := range want {
		have, ok := handled[key]
		if !ok {
			t.Errorf("missing hook %s; have %v", key, handled)
			continue
		}
		if !slices.Equal(have, targets) {
			t.Errorf("%s handled_by = %v, want %v", key, have, targets)
		}
	}
}

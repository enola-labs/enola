// The handler of a file route.
//
// Nuxt and SvelteKit route by file: `pages/home.vue` is the page at /home, and
// `routes/article/[slug]/+page.svelte` the one at /article/[slug]. The route fact
// names the file. What serves it is the component the file declares, and for
// SvelteKit also the `load` beside it, in +page.server.js or +page.js, which the
// framework calls before the component renders and which is where the page's data
// comes from.
//
// A SvelteKit page also takes the form actions its +page.server file exports in
// `actions`: each is a POST to the page, the default one at the page's path and a
// named one at `?/<name>`, which is where most of a form-driven app's writes are.
package tsextractor

import (
	"maps"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// fileRouteFrameworks are the frameworks whose page routes are named by file.
var fileRouteFrameworks = map[string]bool{"nuxt": true, "sveltekit": true, "nextjs": true}

// svelteKitLoadFiles are the files beside a +page.svelte that may declare its load.
var svelteKitLoadFiles = []string{"+page.server.ts", "+page.server.js", "+page.ts", "+page.js"}

// bindFileRouteHandlers adds handled_by to each page route: the component its file
// declares, and a SvelteKit page's load. The component is the function the file
// tags as one (a Next.js page exports generateMetadata beside it), or else the
// file's one exported function.
func bindFileRouteHandlers(allFacts []facts.Fact) {
	var components map[string]string // file -> its one exported function, "" when several
	var tagged map[string]string     // file -> its one function tagged a component
	var loads map[string]string      // file -> the load it declares
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindRoute || f.PropString("type") != "page" || !fileRouteFrameworks[f.PropString("framework")] {
			continue
		}
		if components == nil {
			components, tagged, loads = map[string]string{}, map[string]string{}, map[string]string{}
			for j := range allFacts {
				s := &allFacts[j]
				if s.Kind != facts.KindSymbol || s.PropString("symbol_kind") != facts.SymbolFunc || !s.PropBool("exported") {
					continue
				}
				if prior, seen := components[s.File]; seen && prior != s.Name {
					components[s.File] = ""
				} else {
					components[s.File] = s.Name
				}
				if s.PropString("web_component") == "component" {
					if prior, seen := tagged[s.File]; seen && prior != s.Name {
						tagged[s.File] = ""
					} else {
						tagged[s.File] = s.Name
					}
				}
				if facts.ShortName(s.Name) == "load" {
					loads[s.File] = s.Name
				}
			}
		}
		target, ok := tagged[f.File]
		if !ok {
			target = components[f.File]
		}
		if target != "" {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		}
		if f.PropString("framework") != "sveltekit" {
			continue
		}
		dir := factpath.Dir(f.File)
		for _, name := range svelteKitLoadFiles {
			if target := loads[dir+"/"+name]; target != "" && !f.HasRelation(facts.RelHandledBy, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
			}
		}
	}
}

// svelteKitActionFiles are the files beside a +page.svelte that may export its actions.
var svelteKitActionFiles = []string{"+page.server.ts", "+page.server.js"}

// svelteKitActionRoutes returns a POST route per form action of each SvelteKit page,
// handled by the action. Its type is "action": the page's own form posts it, so like
// the page it is no server route another service calls.
func svelteKitActionRoutes(allFacts []facts.Fact) []facts.Fact {
	var actions map[string][]facts.Fact // file -> the actions it exports
	var out []facts.Fact
	done := map[string]bool{} // a page directory, whose +page.svelte and +page.js are both routes
	for i := range allFacts {
		page := &allFacts[i]
		if page.Kind != facts.KindRoute || page.PropString("type") != "page" ||
			page.PropString("framework") != "sveltekit" || !strings.HasPrefix(factpath.Base(page.File), "+page.") {
			continue
		}
		if actions == nil {
			actions = map[string][]facts.Fact{}
			for _, s := range allFacts {
				if s.Kind != facts.KindSymbol || s.PropString("symbol_kind") != facts.SymbolFunc {
					continue
				}
				if i := strings.LastIndex(s.Name, ".actions."); i >= 0 && !strings.Contains(s.Name[i+len(".actions."):], ".") {
					actions[s.File] = append(actions[s.File], s)
				}
			}
		}
		dir := factpath.Dir(page.File)
		if done[dir] {
			continue
		}
		done[dir] = true
		for _, file := range svelteKitActionFiles {
			for _, action := range actions[dir+"/"+file] {
				name := page.Name
				if short := facts.ShortName(action.Name); short != "default" {
					name += "?/" + short
				}
				out = append(out, facts.Fact{
					Kind: facts.KindRoute,
					Name: name,
					File: action.File,
					Line: action.Line,
					Props: map[string]any{
						"method":    "POST",
						"type":      "action",
						"router":    "sveltekit",
						"language":  "typescript",
						"framework": "sveltekit",
					},
					Relations: []facts.Relation{{Kind: facts.RelHandledBy, Target: action.Name}},
				})
			}
		}
	}
	return out
}

// nextRouteVerbs splits each Next.js App Router route.ts into one route per HTTP verb
// the file exports, handled by that function: `export async function POST()` is the
// POST route. A route.ts that exports no verb keeps its one ALL route.
func nextRouteVerbs(allFacts []facts.Fact) []facts.Fact {
	var verbs map[string][]facts.Fact // file -> its exported verb functions
	out := allFacts[:0:0]
	for _, f := range allFacts {
		if f.Kind != facts.KindRoute || f.PropString("framework") != "nextjs" || f.PropString("type") != "route" {
			out = append(out, f)
			continue
		}
		if verbs == nil {
			verbs = map[string][]facts.Fact{}
			for _, s := range allFacts {
				if s.Kind == facts.KindSymbol && s.PropString("web_component") == "route_handler" &&
					s.PropBool("exported") && s.PropString("method") != "" {
					verbs[s.File] = append(verbs[s.File], s)
				}
			}
		}
		handlers := verbs[f.File]
		if len(handlers) == 0 {
			out = append(out, f)
			continue
		}
		for _, h := range handlers {
			route := f
			route.Props = maps.Clone(f.Props)
			route.Props["method"] = h.PropString("method")
			route.Line = h.Line
			route.Relations = append(slices.Clone(f.Relations), facts.Relation{Kind: facts.RelHandledBy, Target: h.Name})
			out = append(out, route)
		}
	}
	return out
}

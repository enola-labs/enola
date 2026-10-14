// The handler of a file route.
//
// Nuxt and SvelteKit route by file: `pages/home.vue` is the page at /home, and
// `routes/article/[slug]/+page.svelte` the one at /article/[slug]. The route fact
// names the file. What serves it is the component the file declares, and for
// SvelteKit also the `load` beside it, in +page.server.js or +page.js, which the
// framework calls before the component renders and which is where the page's data
// comes from.
package tsextractor

import (
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// fileRouteFrameworks are the frameworks whose page routes are named by file.
var fileRouteFrameworks = map[string]bool{"nuxt": true, "sveltekit": true}

// svelteKitLoadFiles are the files beside a +page.svelte that may declare its load.
var svelteKitLoadFiles = []string{"+page.server.ts", "+page.server.js", "+page.ts", "+page.js"}

// bindFileRouteHandlers adds handled_by to each page route: the one exported function
// its file declares, which is the component, and a SvelteKit page's load.
func bindFileRouteHandlers(allFacts []facts.Fact) {
	var components map[string]string // file -> its one exported function, "" when several
	var loads map[string]string      // file -> the load it declares
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindRoute || f.PropString("type") != "page" || !fileRouteFrameworks[f.PropString("framework")] {
			continue
		}
		if components == nil {
			components, loads = map[string]string{}, map[string]string{}
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
				if facts.ShortName(s.Name) == "load" {
					loads[s.File] = s.Name
				}
			}
		}
		if target := components[f.File]; target != "" {
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

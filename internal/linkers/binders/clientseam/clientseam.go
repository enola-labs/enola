// Package clientseam turns a repo's own HTTP helper into client routes.
//
// A Go service that wraps net/http in one project-local function disappears from
// the cross-repo graph. A Go CLI in this estate is the measured case: every call
// it makes is `api.Request[Task]("/me/tasks", …)`, the net/http call is two
// packages away inside a generic helper, and the per-file client extractor is
// gated on the file importing net/http — which the command files do not. The
// repo emitted ZERO client routes and the cluster reported that the service it
// calls appears isolated, while 14 of its 14 distinct literal paths match a
// server route in that service exactly.
//
// The work splits across two passes because neither half can be done alone. The
// extractor sees one package and records a CANDIDATE for every call passing a
// path-shaped literal — it cannot know whether the callee reaches net/http,
// because the callee is usually somewhere else. This binder sees the whole
// module and answers exactly that question, then emits the routes.
//
// It runs pre-link: these are the facts the cross-repo linker matches on, so
// producing them afterwards would mean producing them for nobody.
package clientseam

import (
	"context"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// requestConstructors are the net/http entry points that mean "this function
// speaks HTTP". Reaching one of them transitively is the definition of a seam
// used here — not a naming convention, which is what a repo can rename out from
// under a graph.
var requestConstructors = map[string]bool{
	"net/http.NewRequest": true, "net/http.NewRequestWithContext": true,
	"net/http.Get": true, "net/http.Post": true, "net/http.PostForm": true,
	"net/http.Head": true, "net/http.Client.Do": true, "net/http.DefaultClient.Do": true,
}

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "client-seam" }

// Pre-link: the routes this emits are what the cross-repo linker matches on.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()
	seams := reachesHTTP(all)

	var out []facts.Fact
	chosen := map[string]int{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, candidate := range propStrings(f.PropAny("client_path_calls")) {
			callee, rest, ok := strings.Cut(candidate, "\x00")
			if !ok || !seams[key(f.Repo, callee)] {
				continue
			}
			path, verb, ok := strings.Cut(rest, "\x00")
			if !ok {
				continue
			}
			// One route per repo+method+path. A CLI calls the same collection
			// endpoint from several commands, and three facts for one contract
			// would inflate every count computed over them.
			route := facts.Fact{
				Kind: facts.KindRoute,
				Name: path,
				File: f.File,
				Line: f.Line,
				Repo: f.Repo,
				Props: map[string]any{
					facts.PropRole:   facts.RoleClient,
					"method":         verb,
					"language":       "go",
					"framework":      "client-seam",
					facts.PropSource: "go-client-seam",
					// The helper is named on the fact, because a reader asking why
					// this repo is said to call that endpoint should not have to
					// rediscover the indirection that hid it in the first place.
					"seam": callee,
				},
			}
			identity := f.Repo + "\x00" + verb + "\x00" + path
			at, exists := chosen[identity]
			switch {
			case !exists:
				chosen[identity] = len(out)
				out = append(out, route)
			case earlierSite(route, out[at]):
				out[at] = route
			}
		}
	}
	// The candidate list is scaffolding and does not belong in the published
	// graph: it names calls that may be nothing, it is keyed on an internal
	// separator, and a consumer who found it would be reading a question rather
	// than an answer. It is removed whether or not any of it resolved, so a repo
	// with no seam is left exactly as clean as one with a resolved seam.
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind == facts.KindSymbol && f.Props != nil {
			f.DelProp("client_path_calls")
		}
	})
	if len(out) > 0 {
		store.Add(out...)
	}
	return nil
}

// reachesHTTP returns every symbol that transitively calls a net/http request
// constructor, keyed by repo so two repos with the same package layout cannot
// bleed into each other.
//
// Transitively, and that is the whole point: that CLI's `api.Request` does not
// call net/http itself, it calls `api.doRequest` which does. A one-hop rule would
// find the seam nobody calls and miss the one everybody does.
func reachesHTTP(all []facts.Fact) map[string]bool {
	callers := map[string][]string{}
	direct := map[string]bool{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		self := key(f.Repo, f.Name)
		for _, rel := range f.Relations {
			if rel.Kind != facts.RelCalls {
				continue
			}
			if requestConstructors[rel.Target] {
				direct[self] = true
			}
			callers[key(f.Repo, rel.Target)] = append(callers[key(f.Repo, rel.Target)], self)
		}
	}

	reaching := map[string]bool{}
	frontier := make([]string, 0, len(direct))
	for symbol := range direct {
		reaching[symbol] = true
		frontier = append(frontier, symbol)
	}
	for len(frontier) > 0 {
		current := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, caller := range callers[current] {
			if reaching[caller] {
				continue
			}
			reaching[caller] = true
			frontier = append(frontier, caller)
		}
	}
	return reaching
}

func earlierSite(a, b facts.Fact) bool {
	if a.File != b.File {
		return a.File < b.File
	}
	return a.Line < b.Line
}

func key(repo, name string) string { return repo + "\x00" + name }

func propStrings(v any) []string {
	switch value := v.(type) {
	case []string:
		return value
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

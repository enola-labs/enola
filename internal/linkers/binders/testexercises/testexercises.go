// Package testexercises answers "which tests exercise this route or operation".
//
// A browser or API test reaches the application through the same seams a client does:
// it requests a path, navigates to a page, sends a GraphQL operation. The TypeScript
// extractor records those calls on the test file's test_ref; this binder resolves them
// after linking, so a test reaches a server route in any language and any loaded
// repository, matched by the HTTP linker's own rules, and writes one exercises
// relation per target.
package testexercises

import (
	"context"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/routeindex"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// The test_ref props the TypeScript extractor writes (see tsextractor/testcalls.go).
const (
	PropHTTPCalls  = "test_http_calls"
	PropPageVisits = "test_page_visits"
	PropGraphQL    = "test_graphql"
	PropGraphQLOps = "test_graphql_ops"
	// PropUnmatched lists the calls that reached no loaded route.
	PropUnmatched = "test_calls_unmatched"
)

// Binder writes exercises relations from test_ref facts to what they hit.
type Binder struct{ m *routeindex.Matcher }

// New returns the binder, matching under the given vocabulary.
func New(v *vocab.Set) *Binder { return &Binder{m: routeindex.New(v)} }

func (b *Binder) Name() string { return "test-exercises" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()
	server := b.m.IndexServerRoutes(all)
	pages := map[string][]routeindex.RouteRef{}
	graphql := map[string]bool{}
	byOperation := map[string][]string{}
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.Repo == "" {
			continue
		}
		switch {
		case routeindex.IsUIRoute(f):
			b.m.IndexServerRef(pages, routeindex.RouteRef{Repo: f.Repo, Method: "GET", Path: f.Name, FullPath: b.m.NormalizePath(f.Name)})
		case f.PropString(facts.PropRouteType) == facts.RouteTypeGraphQL:
			graphql[f.Name] = true
			if op := f.PropString("operation"); op != "" {
				byOperation[op] = append(byOperation[op], f.Name)
			}
		}
	}
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindTestRef {
			return
		}
		kept := f.Relations[:0:0]
		for _, r := range f.Relations {
			if r.Kind != facts.RelExercises {
				kept = append(kept, r)
			}
		}
		targets := map[string]bool{}
		var unmatched []string
		for _, call := range propStrings(f, PropHTTPCalls) {
			verb, path, _ := strings.Cut(call, " ")
			if b.m.NormalizePath(path) == "/" {
				// A test's bare "/" is the base URL it was given: the app under test
				// in a browser test, rarely an API's root. It names no endpoint.
				unmatched = append(unmatched, call)
				continue
			}
			method := routeindex.NormalizeMethod(verb)
			if verb == "*" {
				method = facts.MethodAny
			}
			refs := b.lookup(server, path, method)
			for _, ref := range refs {
				targets[ref.Path] = true
			}
			if len(refs) == 0 {
				unmatched = append(unmatched, call)
			}
		}
		for _, path := range propStrings(f, PropPageVisits) {
			refs := b.lookup(pages, path, "GET")
			for _, ref := range refs {
				targets[ref.Path] = true
			}
			if len(refs) == 0 {
				unmatched = append(unmatched, "VISIT "+path)
			}
		}
		for _, field := range propStrings(f, PropGraphQL) {
			if graphql[field] {
				targets[field] = true
			} else {
				unmatched = append(unmatched, field)
			}
		}
		for _, op := range propStrings(f, PropGraphQLOps) {
			for _, field := range byOperation[op] {
				targets[field] = true
			}
			if len(byOperation[op]) == 0 {
				unmatched = append(unmatched, "operation "+op)
			}
		}
		names := make([]string, 0, len(targets))
		for t := range targets {
			names = append(names, t)
		}
		sort.Strings(names)
		for _, t := range names {
			kept = append(kept, facts.Relation{Kind: facts.RelExercises, Target: t})
		}
		f.Relations = kept
		if len(unmatched) > 0 {
			f.SetProp(PropUnmatched, unmatched)
		} else {
			f.DelProp(PropUnmatched)
		}
	})
	return nil
}

// lookup matches a test's path as a client's is matched, then, when that finds
// nothing, with its id segments as parameters. A test requests a record by its id
// (`GET /users/1234`, `cy.visit("/orders/7f3c...")`), where a client builds the path
// from a variable the extractor already reads as a parameter. A literal route
// (`/users/search`) is tried first, so it still wins over `/users/:id`. Only tests
// get this: a client's literal segment is a name, a test's is usually a fixture.
func (b *Binder) lookup(index map[string][]routeindex.RouteRef, path, method string) []routeindex.RouteRef {
	refs, _ := b.m.LookupClientMatches(index, b.m.NormalizePath(path), method)
	if len(refs) > 0 {
		return refs
	}
	if p, ok := idsAsParams(path); ok {
		refs, _ = b.m.LookupClientMatches(index, b.m.NormalizePath(p), method)
	}
	return refs
}

// idsAsParams rewrites the segments of a path that are record ids (a number, a
// UUID, a 24-digit hex object id) to `{}`, reporting whether any was.
func idsAsParams(path string) (string, bool) {
	q := ""
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path, q = path[:i], path[i:]
	}
	segs := strings.Split(path, "/")
	changed := false
	for i, s := range segs {
		if isRecordID(s) {
			segs[i], changed = "{}", true
		}
	}
	return strings.Join(segs, "/") + q, changed
}

func isRecordID(s string) bool {
	if s == "" {
		return false
	}
	digits, hex := true, true
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case isHexDigit(r):
			digits = false
		default:
			digits, hex = false, false
		}
	}
	switch {
	case digits:
		return true
	case hex && len(s) == 24: // a MongoDB ObjectId
		return true
	}
	return isUUID(s)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexDigit(r) {
				return false
			}
		}
	}
	return true
}

// propStrings reads a []string prop, as written or as decoded from JSON.
func propStrings(f *facts.Fact, key string) []string {
	switch v := f.PropAny(key).(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func isHexDigit(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

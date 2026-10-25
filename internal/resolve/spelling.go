package resolve

import (
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Spellings returns the exact fact names term means when it is written as a reader
// of the code writes a name rather than as enola stores it, or nil:
//
//	gin.Engine.ServeHTTP                      ..Engine.ServeHTTP (Go: the package's name, not its directory)
//	server.Server                             internal/server.Server
//	django.db.models.query.QuerySet.__iter__  django/db/models/query.QuerySet.__iter__
//
// A name is offered only when a fact carries it exactly, so a spelling never widens
// a lookup into a guess.
func (r Resolver) Spellings(store *facts.Store, term string) []string {
	dot := strings.IndexByte(term, '.')
	if dot <= 0 || dot == len(term)-1 || strings.ContainsAny(term, " :") {
		return nil
	}
	var out []string
	add := func(name string) {
		if !slices.Contains(out, name) && len(store.LookupByExactName(name)) > 0 {
			out = append(out, name)
		}
	}
	// A Go package is named in code by its package clause; enola names it by its
	// directory, which for a module's root package is ".".
	pkg, rest := term[:dot], term[dot+1:]
	for _, m := range store.ByKind(facts.KindModule) {
		if m.Name != pkg && m.PropString("package") == pkg {
			add(m.Name + "." + rest)
		}
	}
	// A dotted module path: the module is some prefix of the dots, slashed.
	if !strings.Contains(term, "/") {
		b := []byte(term)
		for i, c := range b {
			if c != '.' {
				continue
			}
			b[i] = '/'
			add(string(b))
		}
	}
	return out
}

package resolve

import (
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Spellings returns the exact fact names term means when it is written as a reader
// of the code writes a name rather than as enola stores it, or nil:
//
//	gin.Engine.ServeHTTP                      github.com/gin-gonic/gin.Engine.ServeHTTP (Go: the package's name)
//	internal/server.Server                    example.com/app/internal/server.Server (Go: the package's directory)
//	internal/server                           example.com/app/internal/server
//	server.Server                             internal/server.Server
//	django.db.models.query.QuerySet.__iter__  django/db/models/query.QuerySet.__iter__
//
// A name is offered only when a fact carries it exactly, so a spelling never widens
// a lookup into a guess.
func (r Resolver) Spellings(store *facts.Store, term string) []string {
	if strings.ContainsAny(term, " :") {
		return nil
	}
	var out []string
	add := func(name string) {
		if !slices.Contains(out, name) && len(store.LookupByExactName(name)) > 0 {
			out = append(out, name)
		}
	}
	// A Go package is named by its import path, and a reader writes its directory:
	// "internal/server" is the package example.com/app/internal/server, and
	// "internal/server.Server" a symbol in it. In a multi-repo store the same
	// directory can be a package of several repositories, and each is offered.
	for _, m := range store.ByKind(facts.KindModule) {
		if !facts.IsGoPackage(m) || m.File == "" || m.File == m.Name {
			continue
		}
		for _, dir := range []string{m.File, strings.TrimPrefix(m.File, m.Repo+"/")} {
			if term == dir {
				add(m.Name)
			} else if rest, ok := strings.CutPrefix(term, dir+"."); ok && rest != "" {
				add(m.Name + "." + rest)
			}
		}
	}
	dot := strings.IndexByte(term, '.')
	if dot <= 0 || dot == len(term)-1 {
		return out
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

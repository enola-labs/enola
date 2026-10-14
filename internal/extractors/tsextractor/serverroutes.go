// Server-side routes declared by CALL, rather than by decorator: Express, Fastify,
// Hono, Koa/Oak. The shape `<recv>.<verb>('/path', handler)` is common to all of
// them, so one pass covers the family.
//
// The whole difficulty here is that the shape is ALSO v141's client shape:
// `axios.get('/x')` and `router.get('/x')` are the same text, and the client pass
// already claims it. Disambiguation is therefore by RECEIVER BINDING, resolved
// within the file, and the default is deliberately "client" — an unknown receiver
// keeps exactly the v141 behaviour rather than being silently reclassified.
package tsextractor

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// The four binding forms. ES-module and CommonJS are both first-class here: a Node
// server is as likely to be written `const app = require('express')()` as
// `import express from "express"; const app = express()`, and matching only the
// former found zero routes on the one real Express server in the corpus.

// appFactory binds an identifier to a whole application object — the ROOT of a route
// tree, so its routes are served at the path as written. ESM form.
var appFactory = regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=;]*)?=\s*(?:new\s+)?(express|fastify|Fastify|Hono|Koa)\s*\(`)

// appFactoryRequire is the same binding written CommonJS-style:
// `const app = require('express')()`.
var appFactoryRequire = regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=;]*)?=\s*require\(\s*['"](express|fastify|koa|hono)['"]\s*\)\s*\(`)

// routerFactory binds an identifier to a sub-router — a fragment MOUNTED somewhere,
// so its own paths are relative and mean nothing until the mount point is known.
var routerFactory = regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=;]*)?=\s*(?:new\s+)?(?:express\s*\.\s*Router|Router)\s*\(`)

// routerFactoryRequire: `const router = require('express').Router()`.
var routerFactoryRequire = regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=;]*)?=\s*require\(\s*['"]express['"]\s*\)\s*\.\s*Router\s*\(`)

// mountCall matches `app.use('/prefix', router)` — the statement that gives a
// sub-router its place in the tree.
var mountCall = regexp.MustCompile("([A-Za-z_$][\\w$]*)\\s*\\.\\s*use\\s*\\(\\s*(?:\"([^\"]*)\"|'([^']*)'|`([^`]*)`)\\s*,\\s*([A-Za-z_$][\\w$]*)")

// serverVerbCall matches a route registration on a named receiver, capturing the
// receiver so the binding table can rule on it.
var serverVerbCall = regexp.MustCompile("([A-Za-z_$][\\w$]*)\\s*\\.\\s*(get|post|put|patch|delete|all|options|head)\\s*(?:<[^()]*>)?\\s*\\(\\s*(?:\"([^\"]*)\"|'([^']*)'|`([^`]*)`)")

// frameworkOf normalises a factory token to the framework label emitted on facts.
var frameworkOf = map[string]string{
	"express": "express", "fastify": "fastify", "Fastify": "fastify",
	"Hono": "hono", "hono": "hono", "Koa": "koa", "koa": "koa",
}

// serverBinding is what an identifier in this file was bound to.
type serverBinding struct {
	framework string
	isRouter  bool   // a mounted fragment rather than an application root
	prefix    string // mount path, for a router mounted in THIS file
	mounted   bool   // whether a mount point is known at all
}

// serverBindings maps identifier -> binding for every app/router constructed in this
// file, with same-file mounts already resolved.
func serverBindings(src []byte) map[string]serverBinding {
	out := map[string]serverBinding{}
	for _, re := range []*regexp.Regexp{appFactory, appFactoryRequire} {
		for _, m := range re.FindAllSubmatch(src, -1) {
			out[string(m[1])] = serverBinding{framework: frameworkOf[string(m[2])], mounted: true}
		}
	}
	for _, re := range []*regexp.Regexp{routerFactory, routerFactoryRequire} {
		for _, m := range re.FindAllSubmatch(src, -1) {
			name := string(m[1])
			if _, taken := out[name]; taken {
				continue // an app binding of the same name wins; do not downgrade it
			}
			out[name] = serverBinding{framework: "express", isRouter: true}
		}
	}
	// Resolve mounts declared in this file: app.use('/webhooks', router).
	for _, m := range mountCall.FindAllSubmatch(src, -1) {
		prefix := firstNonEmpty(m[2], m[3], m[4])
		child := string(m[5])
		b, ok := out[child]
		if !ok || !b.isRouter || b.mounted {
			continue
		}
		// Only a parent whose OWN mount point is known can give one to a child.
		// Mounting onto an unmounted sub-router — `apiRouter.use('/orders', orders)`
		// in a file that does not itself mount apiRouter — yields the fragment
		// "/orders", which is exactly the wrong fact this pass exists to avoid: the
		// route really serves "/api/orders". The child stays unmounted here and
		// routermount.go composes it once both mounts are visible.
		if parent, ok := out[string(m[1])]; ok && parent.mounted {
			b.prefix = facts.JoinRoutePath(parent.prefix, prefix)
			b.mounted = true
			b.framework = parent.framework
			out[child] = b
		}
	}
	return out
}

// extractServerRouteFacts emits a server-role route for every call-registered route
// whose receiver is a known application or a router with a known mount point.
//
// A router whose mount is NOT visible in this file emits NOTHING, on purpose. Its
// declared path is a fragment: `router.post('/login')` in a routes module mounted at
// '/webhooks' elsewhere serves '/webhooks/login', so emitting '/login' would be a wrong
// fact rather than a missing one — and a wrong path can false-match another repo's
// route, which is worse than silence. Cross-file mount resolution is a repo-wide pass
// (the shape of goextractor/routeprefix.go); it is deliberately not attempted here.
func extractServerRouteFacts(src []byte, relFile string) []facts.Fact {
	bindings := serverBindings(src)
	if len(bindings) == 0 {
		return nil
	}
	dir := factpath.Dir(relFile)

	var out []facts.Fact
	seen := map[string]bool{}
	for _, m := range serverVerbCall.FindAllSubmatchIndex(src, -1) {
		b, ok := bindings[string(src[m[2]:m[3]])]
		if !ok || !b.mounted {
			continue
		}
		raw := firstNonEmptyGroup(src, m, 3, 4, 5)
		path, ok := cleanServerPath(raw)
		if !ok {
			continue
		}
		verb := strings.ToUpper(nodeSlice(src, m, 2))
		full := facts.JoinRoutePath(b.prefix, path)
		line := 1 + bytes.Count(src[:m[0]], []byte("\n"))
		key := verb + "\x00" + full + "\x00" + strconv.Itoa(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		props := map[string]any{
			facts.PropRole: facts.RoleServer,
			"method":       verb,
			"framework":    b.framework,
			"language":     "typescript",
		}
		if handler := routeHandlerName(lastCallArgument(src, m[1])); handler != "" {
			props["handler"] = handler
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      full,
			File:      relFile,
			Line:      line,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// cleanServerPath accepts a declared route path, rejecting the ones that carry no
// architectural signal: a non-rooted literal, and a bare catch-all. `app.get('*')` is
// a SPA fallback rather than an endpoint, and indexing it would let it match any
// client path at all.
func cleanServerPath(raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	if trimmed := strings.Trim(p, "/*"); trimmed == "" {
		return "", false
	}
	return p, true
}

// isServerReceiver reports whether an identifier in this file is bound to an
// application or router, so the CLIENT pass can leave its calls alone. Without this,
// `router.get('/x')` would be emitted twice — once as a server route here and once as
// an outbound client call by v141's lowerVerbCall.
func isServerReceiver(bindings map[string]serverBinding, name string) bool {
	_, ok := bindings[name]
	return ok
}

// identifierEndingAt returns the identifier immediately preceding pos, or "".
func identifierEndingAt(src []byte, pos int) string {
	end := pos
	i := pos
	for i > 0 && isIdentByte(src[i-1]) {
		i--
	}
	if i == end {
		return ""
	}
	return string(src[i:end])
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func firstNonEmpty(vals ...[]byte) string {
	for _, v := range vals {
		if len(v) > 0 {
			return string(v)
		}
	}
	return ""
}

// nodeSlice returns capture group g of a FindAllSubmatchIndex match.
func nodeSlice(src []byte, m []int, g int) string {
	s, e := m[2*g], m[2*g+1]
	if s < 0 || e <= s {
		return ""
	}
	return string(src[s:e])
}

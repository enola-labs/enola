// The handler of a call-registered server route.
//
// `router.post('/upload', authenticate, validate(schema), asyncMiddleware(addVideo))`
// names four functions and one of them serves the route: the last. The ones before it
// are middleware, and binding a route to its middleware reads as a handler to every
// consumer and is wrong. A wrapper around the last one is looked through, because the
// wrapper is how an async function is given to a framework that expects a callback.
//
// A handler written as a function literal has no symbol and names nothing here.
package tsextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// maxCallScan bounds the search for the end of a route registration. A registration
// with an inline handler runs to the end of that function, and a call that never
// closes within this much source is not worth reading further.
const maxCallScan = 64 << 10

// lastCallArgument returns the text of the last argument of the call whose argument
// list the position from lies in, at the top level of that list. "" when the call has
// no argument after from, or does not close.
func lastCallArgument(src []byte, from int) string {
	depth := 0
	argStart := -1
	limit := min(len(src), from+maxCallScan)
	for i := from; i < limit; i++ {
		switch c := src[i]; c {
		case '\'', '"', '`':
			i = skipQuoted(src, i, limit)
		case '/':
			if i+1 < limit && src[i+1] == '/' {
				for i < limit && src[i] != '\n' {
					i++
				}
			} else if i+1 < limit && src[i+1] == '*' {
				for i += 2; i+1 < limit && (src[i] != '*' || src[i+1] != '/'); i++ {
				}
				i++
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				if argStart < 0 {
					return ""
				}
				return stripLeadingComments(string(src[argStart:i]))
			}
			depth--
		case ',':
			if depth == 0 {
				argStart = i + 1
			}
		}
	}
	return ""
}

// stripLeadingComments removes the comments an argument is written under, which the
// scan leaves in front of it.
func stripLeadingComments(arg string) string {
	for {
		arg = strings.TrimSpace(arg)
		switch {
		case strings.HasPrefix(arg, "//"):
			_, rest, ok := strings.Cut(arg, "\n")
			if !ok {
				return ""
			}
			arg = rest
		case strings.HasPrefix(arg, "/*"):
			_, rest, ok := strings.Cut(arg, "*/")
			if !ok {
				return ""
			}
			arg = rest
		default:
			return arg
		}
	}
}

// skipQuoted returns the index of the quote closing the string that opens at i.
func skipQuoted(src []byte, i, limit int) int {
	quote := src[i]
	for i++; i < limit; i++ {
		switch src[i] {
		case '\\':
			i++
		case quote:
			return i
		}
	}
	return limit
}

var (
	handlerIdent   = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
	handlerWrapped = regexp.MustCompile(`^[A-Za-z_$][\w$.]*\s*\(\s*([A-Za-z_$][\w$]*)\s*\)$`)
)

// routeHandlerName returns the identifier of the function a handler argument names:
// the argument itself, or the single identifier a wrapper call is given. "" for
// anything else, a function literal included.
func routeHandlerName(arg string) string {
	if handlerIdent.MatchString(arg) {
		return arg
	}
	if m := handlerWrapped.FindStringSubmatch(arg); m != nil {
		return m[1]
	}
	return ""
}

// serverFrameworks are the frameworks whose routes serverroutes.go and routermount.go
// emit, and so the ones whose handler prop is an identifier of the route's own file.
var serverFrameworks = map[string]bool{"express": true, "fastify": true, "hono": true, "koa": true}

// bindServerRouteHandlers adds handled_by to each call-registered server route whose
// handler names a function the repository declares: one of the route's own file, or
// one that file imports. The name is matched against function symbols only, so the
// single argument of a call that is not a wrapper, `validate(schema)`, binds nothing.
func bindServerRouteHandlers(allFacts []facts.Fact, files []*routerFile) {
	byFile := map[string]*routerFile{}
	for _, f := range files {
		if f != nil {
			byFile[f.relFile] = f
		}
	}
	var functions map[string]bool
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindRoute || f.PropString(facts.PropRole) != facts.RoleServer ||
			!serverFrameworks[f.PropString("framework")] {
			continue
		}
		name := f.PropString("handler")
		if name == "" {
			continue
		}
		if functions == nil {
			functions = map[string]bool{}
			for j := range allFacts {
				if s := &allFacts[j]; s.Kind == facts.KindSymbol && s.PropString("symbol_kind") == facts.SymbolFunc {
					functions[s.Name] = true
				}
			}
		}
		target := factpath.Dir(f.File) + "." + name
		if rf := byFile[f.File]; rf != nil {
			if imp, imported := rf.imports[name]; imported {
				declares := func(file, n string) bool { return functions[factpath.Dir(file)+"."+n] }
				if file, local, ok := resolveExported(byFile, imp.file, imp.export, 0, declares); ok {
					target = factpath.Dir(file) + "." + local
				}
			}
		}
		if functions[target] {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		}
	}
}

// stringConstReader returns the lookup composeRouterMounts uses for a constant in a
// mount prefix: the string literal `const NAME = '...'` binds in a file of the
// repository. The file is read when a mount asks, which is once or twice a repository.
func stringConstReader(repoPath string) func(file, name string) (string, bool) {
	return func(file, name string) (string, bool) {
		if !handlerIdent.MatchString(name) {
			return "", false
		}
		src, err := os.ReadFile(filepath.Join(repoPath, file))
		if err != nil {
			return "", false
		}
		re := regexp.MustCompile(`(?m)^\s*(?:export\s+)?const\s+` + regexp.QuoteMeta(name) + `\s*(?::\s*string\s*)?=\s*(?:"([^"\n]*)"|'([^'\n]*)')`)
		m := re.FindSubmatch(src)
		if m == nil {
			return "", false
		}
		return firstNonEmpty(m[1], m[2]), true
	}
}

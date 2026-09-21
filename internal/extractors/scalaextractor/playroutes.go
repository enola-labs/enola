package scalaextractor

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Play's routes file is a DSL of its own — not Scala, and not reachable through the
// tree-sitter walker at all. It is read directly from disk for the same reason the
// OpenAPI and Symfony extractors read theirs: the ignore globs exist to suppress
// config and data noise, not to hide the file that declares a service's entire HTTP
// surface. `conf/routes` has no extension, so no glob would admit it anyway.
//
// The format is three whitespace-separated columns:
//
//	GET     /users/:id          controllers.Users.show(id: Long)
//	POST    /teams              controllers.Teams.create
//	->      /appeal             appeal.Routes
//
// The third form is an INCLUDE, and it is what makes the file a tree rather than a
// list: the included router's own routes all hang below that prefix. Reading only
// the top-level file would report `/appeal/...` endpoints at their bare paths, which
// is the same mis-mounting the Go and Rust extractors compose away for subrouters.

var (
	// playRouteRe matches a verb line. The handler runs to end-of-line because it
	// may carry a parameter list with spaces and type ascriptions.
	playRouteRe = regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+(\S+)\s+(.+)$`)
	// playIncludeRe matches a sub-router mount: `->  /prefix  pkg.Routes`.
	playIncludeRe = regexp.MustCompile(`^->\s+(\S+)\s+(\S+)`)
	// playParamRe matches Play's regex-constrained parameter form, `$name<regex>`.
	// The regex body is discarded: it constrains what matches, not what the path IS.
	playParamRe = regexp.MustCompile(`\$(\w+)<[^>]*>`)
)

// playRoutesFile is one parsed routes file plus the includes it mounts.
type playRoutesFile struct {
	rel      string
	routes   []playRoute
	includes []playInclude
}

type playRoute struct {
	method, path, handler string
	line                  int
}

type playInclude struct {
	prefix, router string
	line           int
}

// extractPlayRoutes reads a Play routes tree and emits one server route fact per
// endpoint, at its composed mount path.
//
// It walks from the repository root rather than from the passed file list, because
// the walker has already filtered that list and `conf/routes` — extensionless — is
// not something any glob admits. Only the `conf/` directory is read, so the cost is
// a single directory listing on a repository that has no Play app at all.
func extractPlayRoutes(repoPath string) []facts.Fact {
	confDir := filepath.Join(repoPath, "conf")
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return nil // no conf/ directory: not a Play application
	}

	files := map[string]*playRoutesFile{} // router name -> parsed file
	var rootFile *playRoutesFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		isRoot := name == "routes"
		if !isRoot && !strings.HasSuffix(name, ".routes") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("conf", name))
		parsed := parsePlayRoutesFile(filepath.Join(confDir, name), rel)
		if parsed == nil {
			continue
		}
		if isRoot {
			rootFile = parsed
		} else {
			// `appeal.routes` is mounted as `appeal.Routes`; key on the stem so an
			// include can find it.
			files[strings.TrimSuffix(name, ".routes")] = parsed
		}
	}
	if rootFile == nil && len(files) == 0 {
		return nil
	}

	var out []facts.Fact
	emit := func(f *playRoutesFile, prefix string) {
		for _, r := range f.routes {
			out = append(out, playRouteFact(f.rel, prefix, r))
		}
	}

	// The root file's own routes, then each include's routes at their mount prefix.
	// One level of nesting is composed; a router included from an included file
	// keeps its own prefix only, which is reported rather than guessed at.
	if rootFile != nil {
		emit(rootFile, "")
		for _, inc := range rootFile.includes {
			sub, ok := files[playRouterStem(inc.router)]
			if !ok {
				continue // mounts a router this repository does not declare
			}
			emit(sub, inc.prefix)
			delete(files, playRouterStem(inc.router))
		}
	}
	// Any routes file nobody mounted still declares endpoints; emit them at their
	// bare paths rather than dropping them, and say the mount was not found.
	for _, f := range files {
		for _, r := range f.routes {
			fact := playRouteFact(f.rel, "", r)
			fact.SetProp("unmounted", true)
			out = append(out, fact)
		}
	}
	return out
}

// playRouterStem turns `appeal.Routes` into `appeal`, the routes-file stem.
func playRouterStem(router string) string {
	router = strings.TrimSuffix(router, ".Routes")
	router = strings.TrimSuffix(router, ".routes")
	if i := strings.LastIndex(router, "."); i >= 0 {
		return router[i+1:]
	}
	return router
}

func playRouteFact(relFile, prefix string, r playRoute) facts.Fact {
	path := composePlayPath(prefix, normalizePlayPath(r.path))
	return facts.Fact{
		Kind: facts.KindRoute,
		Name: path,
		File: relFile,
		Line: r.line,
		Props: map[string]any{
			"language":          "scala",
			facts.PropFramework: "play",
			facts.PropSource:    facts.RouteSourcePlayRoutes,
			facts.PropRole:      facts.RoleServer,
			"method":            r.method,
			"path":              path,
			"handler":           normalizePlayHandler(r.handler),
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: "conf"}},
	}
}

// parsePlayRoutesFile reads one routes file. Comments and blank lines are skipped;
// a continuation of the modifier syntax (`+ nocsrf`) is a directive, not a route.
func parsePlayRoutesFile(absPath, rel string) *playRoutesFile {
	f, err := os.Open(absPath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	out := &playRoutesFile{rel: rel}
	scanner := bufio.NewScanner(f)
	// Play routes files are line-oriented but a handler list can be long; raise the
	// limit above bufio's 64 KB default so a wide line cannot truncate a file.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "+") {
			continue
		}
		if m := playIncludeRe.FindStringSubmatch(line); m != nil {
			out.includes = append(out.includes, playInclude{prefix: m[1], router: m[2], line: lineNo})
			continue
		}
		if m := playRouteRe.FindStringSubmatch(line); m != nil {
			out.routes = append(out.routes, playRoute{
				method: m[1], path: m[2], handler: strings.TrimSpace(m[3]), line: lineNo,
			})
		}
	}
	return out
}

// composePlayPath mounts a sub-router's path under its include prefix, unless the
// path already carries that prefix.
//
// Play prepends the mount prefix to a sub-router's paths, so an included file is
// normally written relative. But writing them ABSOLUTE — repeating the prefix in
// every line of the included file — is common enough that it cannot be treated as a
// mistake: all four of the included routes files in the benchmark corpus do it, in
// every one of their 101 lines. Composing blindly turns `/team` mounted at `/team`
// into `/team/team`, an endpoint the application does not serve, and a fabricated
// path is worse than an uncomposed one because it matches no client call and reads
// as a real endpoint.
//
// The check is SEGMENT-wise rather than a string prefix, which matters: a route
// `/teams` under a `/team` mount shares a string prefix but not a segment, and must
// still be composed.
func composePlayPath(prefix, path string) string {
	if prefix == "" {
		return path
	}
	if hasPathSegmentPrefix(path, prefix) {
		return path
	}
	return facts.JoinRoutePath(prefix, path)
}

// hasPathSegmentPrefix reports whether path's leading segments are exactly prefix's.
func hasPathSegmentPrefix(path, prefix string) bool {
	ps := splitSegments(prefix)
	xs := splitSegments(path)
	if len(ps) == 0 || len(xs) < len(ps) {
		return false
	}
	for i, seg := range ps {
		if xs[i] != seg {
			return false
		}
	}
	return true
}

func splitSegments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// normalizePlayPath rewrites Play's three parameter spellings into the single `:name`
// form the rest of enola matches on, so a client call can be joined to the endpoint
// it hits regardless of which spelling the server used.
//
//	/users/:id            -> /users/:id      (already canonical)
//	/assets/*file         -> /assets/:file   (catch-all)
//	/$lang<\w\w>/tv       -> /:lang/tv       (regex-constrained)
//
// The regex body is deliberately dropped. It constrains which requests match, not
// what the path is, and keeping it would make two spellings of the same endpoint
// compare unequal.
func normalizePlayPath(p string) string {
	p = playParamRe.ReplaceAllString(p, ":$1")
	if strings.Contains(p, "*") {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			if strings.HasPrefix(s, "*") {
				segs[i] = ":" + strings.TrimPrefix(s, "*")
			}
		}
		p = strings.Join(segs, "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// normalizePlayHandler strips the parameter list from a handler reference, leaving
// `controllers.Users.show`. The binder matches on the symbol, and the argument list
// is type ascription rather than identity.
func normalizePlayHandler(h string) string {
	if i := strings.Index(h, "("); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSpace(h)
}

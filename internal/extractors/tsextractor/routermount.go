// Cross-file Express router mounts: the repo-wide half of serverroutes.go.
//
// serverroutes.go resolves a mount only when it is written in the same file as the
// router it mounts, and deliberately emits NOTHING otherwise — `router.post('/login')`
// in a module mounted at '/webhooks' elsewhere serves '/webhooks/login', and emitting
// the fragment '/login' would be a wrong fact rather than a missing one. That is the
// right call for a per-file pass, but it is also the shape every real Express backend
// is written in: routes in `src/api/*.ts`, `app.use('/api', router)` in `src/server.ts`.
// A four-route service split that way contributed zero routes.
//
// This file closes the gap the same way the Go extractor did for gorilla/mux
// subrouters (goextractor/routeprefix.go, v125) and the Rust one for Axum's .nest()
// (rustextractor/axum.go, v130): each file reports the routers it declares, the routes
// pending on them, and the mounts it writes; a repo-wide fixpoint then propagates
// prefixes from the application roots outward, and routes are emitted at their true
// runtime path.
//
// Three properties hold it to the standard serverroutes.go set:
//
//   - Literal prefixes only. `app.use(base, router)` resolves nothing.
//   - Reachable mounts only. A router nothing mounts, or one whose mount cannot be
//     resolved to a declaration, still emits nothing — never a fragment.
//   - No double emission. A router mounted in its OWN file is already emitted by
//     serverroutes.go and is skipped here; only routers left unmounted by that pass
//     are candidates.
//
// So the pass can add routes and correct their paths; it cannot invent one.
package tsextractor

import (
	"bytes"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// tsFileResult is what one parallel per-file walk returns: the facts it extracted,
// plus the router/mount data the repo-wide pass needs. Mirrors rustextractor's
// fileResult — a per-file pass cannot see both ends of a cross-file mount, so the
// evidence has to travel out of it.
type tsFileResult struct {
	facts   []facts.Fact
	routers *routerFile
	angular angularCounts
	// angularRouter is the route arrays and router calls this file declares, held
	// for the repo-wide walk: a lazy loadChildren has its path in one file and its
	// routes in another.
	angularRouter *angularRouterFile
	// angularInline holds the inline templates this file's components declare,
	// keyed by component symbol name, for the repo-wide template join.
	angularInline map[string]*angularTemplate
	// angularHTTP is the requests and path constants this file declares, held for
	// the repo-wide pass that joins them.
	angularHTTP *angularHTTPFile
	// clients is what each declared in-house client found in this file, summed into one
	// account per client for the repository.
	clients clientCounts
}

// --- what one file contributes ---

// routerRef identifies one router binding: the file that declares it and the
// identifier it is bound to. Two files may each have a `router`, so neither half
// identifies a unit on its own.
type routerRef struct {
	file string
	name string
}

// pendingRoute is a route registered on a router whose mount point this file does
// not know. It carries what the emitted fact needs, minus the prefix.
type pendingRoute struct {
	verb      string
	path      string
	line      int
	framework string
}

// routerMountEdge is one `<parent>.use('<prefix>', <child>)`, where child is either
// a router identifier or a call to a factory that returns one.
type routerMountEdge struct {
	file      string
	parent    string
	prefix    string
	child     string
	childCall bool
}

// importRef is a resolved import: the file it points at and the export name taken
// from it. `defaultExport` covers `import x from`, `export default` and CommonJS's
// `module.exports =`, which are the same thing seen from either side.
type importRef struct {
	file   string
	export string
}

const defaultExport = "default"

// routerFile is one file's contribution to the repo-wide pass. Collected during the
// parallel per-file walk, where the path aliases needed to resolve an import are in
// scope; resolution across files happens later, serially.
type routerFile struct {
	relFile   string
	roots     map[string]bool           // identifiers bound to an application (mount prefix "")
	routers   map[string]bool           // identifiers bound to a sub-router, unmounted in this file
	pending   map[string][]pendingRoute // router identifier -> routes awaiting a prefix
	mounts    []routerMountEdge
	exports   map[string]string // export name -> local identifier
	imports   map[string]importRef
	factories map[string]string // function name -> the router identifier it returns
}

func (f *routerFile) empty() bool {
	return len(f.pending) == 0 && len(f.mounts) == 0 && len(f.exports) == 0 && len(f.factories) == 0
}

// --- per-file collection ---

// mountCallFactory matches `app.use('/api', routes())` — a router returned by a call
// rather than held in a variable. Only a zero-argument call: an argument list is a
// middleware factory as often as a router factory, and guessing is how a wrong prefix
// gets invented.
var mountCallFactory = regexp.MustCompile("([A-Za-z_$][\\w$]*)\\s*\\.\\s*use\\s*\\(\\s*(?:\"([^\"]*)\"|'([^']*)'|`([^`]*)`)\\s*,\\s*([A-Za-z_$][\\w$]*)\\s*\\(\\s*\\)")

// The export forms, ESM and CommonJS. Both are first-class here for the same reason
// serverroutes.go binds both `express()` and `require('express')()`: a Node service is
// as likely to be written one way as the other.
var (
	exportDefaultIdent = regexp.MustCompile(`export\s+default\s+([A-Za-z_$][\w$]*)\s*;?`)
	exportNamedBinding = regexp.MustCompile(`export\s+(?:const|let|var)\s+([A-Za-z_$][\w$]*)`)
	exportNamedFunc    = regexp.MustCompile(`export\s+(?:async\s+)?function\s+([A-Za-z_$][\w$]*)`)
	exportList         = regexp.MustCompile(`export\s*\{([^}]*)\}`)
	moduleExportsIdent = regexp.MustCompile(`module\s*\.\s*exports\s*=\s*([A-Za-z_$][\w$]*)\s*;?`)
	moduleExportsNamed = regexp.MustCompile(`(?:module\s*\.\s*)?exports\s*\.\s*([A-Za-z_$][\w$]*)\s*=\s*([A-Za-z_$][\w$]*)`)
)

// The import forms. A namespace import (`import * as routes from`) is deliberately
// absent: its members are reached as `routes.router`, which no mount form here matches.
var (
	importDefault = regexp.MustCompile(`import\s+([A-Za-z_$][\w$]*)\s*(?:,\s*\{([^}]*)\})?\s*from\s*['"]([^'"]+)['"]`)
	importNamed   = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
	requireWhole  = regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*require\(\s*['"]([^'"]+)['"]\s*\)\s*;?`)
	requireNamed  = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]*)\}\s*=\s*require\(\s*['"]([^'"]+)['"]\s*\)`)
)

// funcStart matches the head of a function that could return a router: a declaration
// or a variable-bound arrow/function expression. Used only to attribute a `return x`
// to the function it sits in.
var (
	funcStart  = regexp.MustCompile(`(?:async\s+)?function\s+([A-Za-z_$][\w$]*)|(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=;]*)?=\s*(?:async\s*)?(?:function\b|\()`)
	returnCall = regexp.MustCompile(`return\s+([A-Za-z_$][\w$]*)\s*;?`)
)

// collectRouterFile records everything in one file that the repo-wide pass needs.
// Returns nil when the file has nothing to contribute, which is almost every file.
func collectRouterFile(src []byte, relFile string, aliases map[string]tsAlias, knownFiles map[string]bool) *routerFile {
	bindings := serverBindings(src)
	imports := collectRouterImports(src, relFile, aliases, knownFiles)
	if len(bindings) == 0 && len(imports) == 0 {
		return nil
	}

	f := &routerFile{
		relFile: relFile,
		roots:   map[string]bool{},
		routers: map[string]bool{},
		pending: map[string][]pendingRoute{},
		exports: map[string]string{},
		imports: imports,
	}

	for name, b := range bindings {
		switch {
		case !b.isRouter && b.mounted:
			// An application object: the root of a mount tree, serving at "".
			f.roots[name] = true
		case b.isRouter && !b.mounted:
			// A sub-router with no mount point in this file — the case that emits
			// nothing today and the only one this pass may claim.
			f.routers[name] = true
		}
	}

	// Routes registered on those unmounted routers, held until a prefix is known.
	for _, m := range serverVerbCall.FindAllSubmatchIndex(src, -1) {
		recv := string(src[m[2]:m[3]])
		if !f.routers[recv] {
			continue // an app root or a locally-mounted router: serverroutes.go owns it
		}
		path, ok := cleanServerPath(firstNonEmptyGroup(src, m, 3, 4, 5))
		if !ok {
			continue
		}
		f.pending[recv] = append(f.pending[recv], pendingRoute{
			verb:      strings.ToUpper(nodeSlice(src, m, 2)),
			path:      path,
			line:      1 + bytes.Count(src[:m[0]], []byte("\n")),
			framework: bindings[recv].framework,
		})
	}

	for _, m := range mountCall.FindAllSubmatch(src, -1) {
		f.mounts = append(f.mounts, routerMountEdge{
			file:   relFile,
			parent: string(m[1]),
			prefix: firstNonEmpty(m[2], m[3], m[4]),
			child:  string(m[5]),
		})
	}
	for _, m := range mountCallFactory.FindAllSubmatch(src, -1) {
		f.mounts = append(f.mounts, routerMountEdge{
			file:      relFile,
			parent:    string(m[1]),
			prefix:    firstNonEmpty(m[2], m[3], m[4]),
			child:     string(m[5]),
			childCall: true,
		})
	}

	collectRouterExports(src, f)
	f.factories = collectRouterFactories(src, f.routers)

	if f.empty() {
		return nil
	}
	return f
}

// collectRouterExports fills f.exports with export name -> local identifier for every
// export form that can carry a router or a router factory.
func collectRouterExports(src []byte, f *routerFile) {
	for _, m := range exportDefaultIdent.FindAllSubmatch(src, -1) {
		f.exports[defaultExport] = string(m[1])
	}
	for _, m := range moduleExportsIdent.FindAllSubmatch(src, -1) {
		f.exports[defaultExport] = string(m[1])
	}
	// `export const router = …` / `export function routes()` export under their own
	// name, so the exported name and the local identifier are the same.
	for _, re := range []*regexp.Regexp{exportNamedBinding, exportNamedFunc} {
		for _, m := range re.FindAllSubmatch(src, -1) {
			name := string(m[1])
			f.exports[name] = name
		}
	}
	for _, m := range exportList.FindAllSubmatch(src, -1) {
		for local, exported := range parseSpecifierList(string(m[1])) {
			// In an export list the local name is written first: `{ router as api }`
			// exports local `router` under the name `api`.
			f.exports[exported] = local
		}
	}
	for _, m := range moduleExportsNamed.FindAllSubmatch(src, -1) {
		f.exports[string(m[1])] = string(m[2])
	}
}

// collectRouterImports resolves each import to (file, export name), so a mount written
// against an imported identifier can find the declaration it names. Only imports that
// resolve to a file in this snapshot are kept: an external package is not a router
// this repository declares.
func collectRouterImports(src []byte, relFile string, aliases map[string]tsAlias, knownFiles map[string]bool) map[string]importRef {
	fileDir := factpath.Dir(relFile)
	out := map[string]importRef{}

	resolve := func(spec string) (string, bool) {
		resolved, external := resolveImportPath(spec, fileDir, aliases)
		if external {
			return "", false
		}
		// An import written WITH an extension: a real .js file, or — under TypeScript's
		// nodenext resolution, where the extension is mandatory — a `.js` specifier
		// naming the file `.ts` will compile to. resolveModuleFile only ever appends an
		// extension, so both forms have to be tried before it.
		if knownFiles[resolved] {
			return resolved, true
		}
		if stem, ok := stripEmittedExt(resolved); ok {
			if file, _, ok := resolveModuleFile(stem, knownFiles); ok {
				return file, true
			}
		}
		file, _, ok := resolveModuleFile(resolved, knownFiles)
		return file, ok
	}

	for _, m := range importDefault.FindAllSubmatch(src, -1) {
		file, ok := resolve(string(m[3]))
		if !ok {
			continue
		}
		out[string(m[1])] = importRef{file: file, export: defaultExport}
		for local, exported := range parseSpecifierList(string(m[2])) {
			out[local] = importRef{file: file, export: exported}
		}
	}
	for _, m := range importNamed.FindAllSubmatch(src, -1) {
		file, ok := resolve(string(m[2]))
		if !ok {
			continue
		}
		// In an IMPORT list the written order is reversed relative to an export list:
		// `{ router as api }` binds the local name `api` to the exported `router`.
		for exported, local := range parseSpecifierList(string(m[1])) {
			out[local] = importRef{file: file, export: exported}
		}
	}
	for _, m := range requireWhole.FindAllSubmatch(src, -1) {
		file, ok := resolve(string(m[2]))
		if !ok {
			continue
		}
		out[string(m[1])] = importRef{file: file, export: defaultExport}
	}
	for _, m := range requireNamed.FindAllSubmatch(src, -1) {
		file, ok := resolve(string(m[2]))
		if !ok {
			continue
		}
		for exported, local := range parseSpecifierList(string(m[1])) {
			out[local] = importRef{file: file, export: exported}
		}
	}
	return out
}

// stripEmittedExt removes the extension of the file a TypeScript source COMPILES to,
// which is what an ESM specifier under nodenext resolution names: `./orders.js` is
// written for a module whose source is `orders.ts`.
func stripEmittedExt(path string) (string, bool) {
	for _, ext := range []string{".js", ".jsx", ".mjs", ".cjs"} {
		if strings.HasSuffix(path, ext) {
			return strings.TrimSuffix(path, ext), true
		}
	}
	return "", false
}

// parseSpecifierList reads `a, b as c` into first-name -> second-name (`a`->`a`,
// `b`->`c`). Which side is local and which is exported depends on the clause, so
// callers name the halves rather than this function guessing.
func parseSpecifierList(list string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "type ") {
			continue // `import { type Foo }` binds no value
		}
		first, second := part, part
		if i := strings.Index(part, " as "); i >= 0 {
			first = strings.TrimSpace(part[:i])
			second = strings.TrimSpace(part[i+4:])
		}
		if isIdentifier(first) && isIdentifier(second) {
			out[first] = second
		}
	}
	return out
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return s[0] < '0' || s[0] > '9'
}

// collectRouterFactories maps a function name to the router it returns, so
// `app.use('/api', routes())` can reach the router `routes` built. A `return x` is
// attributed to the nearest function head above it, and kept only when x is one of
// this file's unmounted routers — a wrong attribution therefore yields no factory
// rather than a wrong one.
func collectRouterFactories(src []byte, routers map[string]bool) map[string]string {
	if len(routers) == 0 {
		return nil
	}
	type head struct {
		pos  int
		name string
	}
	var heads []head
	for _, m := range funcStart.FindAllSubmatchIndex(src, -1) {
		name := nodeSlice(src, m, 1)
		if name == "" {
			name = nodeSlice(src, m, 2)
		}
		if name != "" {
			heads = append(heads, head{pos: m[0], name: name})
		}
	}
	if len(heads) == 0 {
		return nil
	}

	out := map[string]string{}
	for _, m := range returnCall.FindAllSubmatchIndex(src, -1) {
		returned := nodeSlice(src, m, 1)
		if !routers[returned] {
			continue
		}
		i := sort.Search(len(heads), func(i int) bool { return heads[i].pos > m[0] }) - 1
		if i < 0 {
			continue
		}
		out[heads[i].name] = returned
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- the repo-wide pass ---

// composeRouterMounts emits the routes that per-file extraction had to hold back:
// those on routers mounted from another file. Files are keyed by their path; the
// result is deterministic (files, then units, then prefixes, all sorted).
func composeRouterMounts(files []*routerFile) []facts.Fact {
	byFile := map[string]*routerFile{}
	pendingCount := 0
	for _, f := range files {
		if f == nil {
			continue
		}
		byFile[f.relFile] = f
		pendingCount += len(f.pending)
	}
	if pendingCount == 0 {
		return nil // nothing was held back; the common case, and free
	}

	// Resolve every mount to (parent unit, child unit). A mount whose child cannot be
	// resolved to a declaration in this snapshot simply contributes no edge.
	type edge struct {
		parent routerRef
		child  routerRef
		prefix string
	}
	var edges []edge
	roots := map[routerRef]bool{}

	for _, name := range sortedFileNames(byFile) {
		f := byFile[name]
		for _, m := range f.mounts {
			child, ok := resolveMountChild(byFile, f, m)
			if !ok {
				continue
			}
			parent := routerRef{file: f.relFile, name: m.parent}
			switch {
			case f.roots[m.parent]:
				roots[parent] = true
			case !f.routers[m.parent]:
				// The receiver is neither an application nor a router this file
				// declares (a locally-mounted router, or an unrelated identifier).
				// Locally-mounted routers already carry their full path through
				// serverroutes.go, so treating this as a root would double-count.
				continue
			}
			edges = append(edges, edge{parent: parent, child: child, prefix: m.prefix})
		}
	}
	if len(edges) == 0 {
		return nil
	}

	// Propagate prefixes outward from the roots to a fixpoint. Bounded by the edge
	// count: a longer chain than that implies a cycle, and a cycle in a mount graph
	// is not a shape to follow forever.
	prefixes := map[routerRef]map[string]bool{}
	for r := range roots {
		prefixes[r] = map[string]bool{"": true}
	}
	for round := 0; round <= len(edges); round++ {
		changed := false
		for _, e := range edges {
			for base := range prefixes[e.parent] {
				full := facts.JoinRoutePath(base, e.prefix)
				if prefixes[e.child] == nil {
					prefixes[e.child] = map[string]bool{}
				}
				if !prefixes[e.child][full] {
					prefixes[e.child][full] = true
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	var out []facts.Fact
	for _, name := range sortedFileNames(byFile) {
		f := byFile[name]
		for _, ident := range sortedKeys(f.pending) {
			unit := routerRef{file: f.relFile, name: ident}
			mounts := sortedSet(prefixes[unit])
			if len(mounts) == 0 {
				continue // mounted nowhere resolvable: still silent, by design
			}
			dir := factpath.Dir(f.relFile)
			seen := map[string]bool{}
			for _, prefix := range mounts {
				for _, r := range f.pending[ident] {
					full := facts.JoinRoutePath(prefix, r.path)
					key := r.verb + "\x00" + full + "\x00" + strconv.Itoa(r.line)
					if seen[key] {
						continue
					}
					seen[key] = true
					out = append(out, facts.Fact{
						Kind: facts.KindRoute,
						Name: full,
						File: f.relFile,
						Line: r.line,
						Props: map[string]any{
							facts.PropRole: facts.RoleServer,
							"method":       r.verb,
							"framework":    r.framework,
							"language":     "typescript",
							// The mount that produced this path lives in another file,
							// so the fact records that it was composed rather than read
							// off one line — the same courtesy the Go extractor pays.
							"mount_composed": true,
						},
						Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
					})
				}
			}
		}
	}
	return out
}

// resolveMountChild maps the mounted expression to the router unit it names, looking
// through an import and through a factory function. Unresolvable is the safe answer
// and the common one.
func resolveMountChild(byFile map[string]*routerFile, f *routerFile, m routerMountEdge) (routerRef, bool) {
	if m.childCall {
		if ident, ok := f.factories[m.child]; ok {
			return routerRef{file: f.relFile, name: ident}, true
		}
		g, exported, ok := followImport(byFile, f, m.child)
		if !ok {
			return routerRef{}, false
		}
		if ident, ok := g.factories[exported]; ok {
			return routerRef{file: g.relFile, name: ident}, true
		}
		// `export default function () { … return router }` exports the factory
		// anonymously; the export map then points at the router itself.
		if local, ok := g.exports[exported]; ok {
			if ident, ok := g.factories[local]; ok {
				return routerRef{file: g.relFile, name: ident}, true
			}
		}
		return routerRef{}, false
	}

	if f.routers[m.child] {
		return routerRef{file: f.relFile, name: m.child}, true
	}
	g, exported, ok := followImport(byFile, f, m.child)
	if !ok {
		return routerRef{}, false
	}
	local, ok := g.exports[exported]
	if !ok {
		return routerRef{}, false
	}
	if g.routers[local] {
		return routerRef{file: g.relFile, name: local}, true
	}
	return routerRef{}, false
}

// followImport resolves a local identifier to the file it was imported from and the
// name it carries there.
func followImport(byFile map[string]*routerFile, f *routerFile, local string) (*routerFile, string, bool) {
	ref, ok := f.imports[local]
	if !ok {
		return nil, "", false
	}
	g, ok := byFile[ref.file]
	if !ok {
		return nil, "", false
	}
	return g, ref.export, true
}

func sortedFileNames(byFile map[string]*routerFile) []string {
	out := make([]string, 0, len(byFile))
	for name := range byFile {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string][]pendingRoute) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

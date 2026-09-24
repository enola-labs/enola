package pythonextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// PythonExtractor extracts architectural facts from Python source code using
// line-based regex parsing with indentation-based scope tracking.
type PythonExtractor struct{}

// New creates a new PythonExtractor.
func New() *PythonExtractor {
	return &PythonExtractor{}
}

func (e *PythonExtractor) Name() string {
	return "python"
}

// Detect returns true if the repository looks like a Python project.
// It checks root-level markers first, then walks up to 3 subdirectory levels
// to support monorepos where Python code lives in a subdirectory (e.g. python/).
func (e *PythonExtractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// pyMarkers are the manifests that name a Python project wherever they appear.
var pyMarkers = map[string]bool{
	"pyproject.toml": true, "setup.py": true, "requirements.txt": true, "Pipfile": true,
	"pytest.ini": true, "mypy.ini": true, "tox.ini": true, "setup.cfg": true,
}

// DetectFiles implements plugin.FileListDetector.
//
// Two bounds went away here, not one. The marker search was capped at three levels,
// and the markers were the ONLY signal — so a manifest-less Python tree was
// undetectable at any depth, which no amount of raising the cap would have fixed.
// gmsh (52 .py, 43 of them within the old bound) and the dart-sdk (102) were both
// invisible for that second reason rather than the first.
//
// .py is therefore accepted as a signal in its own right. The extension is
// unambiguous — it names one language and no build system shares it — unlike the
// Gradle file that forced the JVM extractors to insist on a declared plugin.
func (e *PythonExtractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, "site-packages", "__pycache__", ".venv", "venv") {
			continue
		}
		name := detectnames.Base(rel)
		if pyMarkers[name] || strings.HasSuffix(name, ".py") {
			return true, nil
		}
	}
	return false, nil
}

// Extract parses Python files and emits architectural facts.
//
// Both passes parse files in parallel (bounded by GOMAXPROCS) but merge their
// results in stable file order, so the emitted facts are byte-for-byte identical
// regardless of how the work was scheduled. Python parsing dominates snapshot
// time on large polyglot repos, so this is the main throughput lever.
func (e *PythonExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	isDjango := detectDjango(repoPath)
	isFlask := detectFlask(repoPath)
	isFastAPI := detectFastAPI(repoPath)

	// Restrict to Python files once; both passes iterate the same ordered set.
	var pyFiles []string
	for _, relFile := range files {
		if isPythonFile(relFile) {
			pyFiles = append(pyFiles, relFile)
		}
	}

	// One parse per file. Each file emits its facts and, from the SAME tree, the
	// slice of the symbol index it contributes. There used to be a separate indexing
	// pass ahead of this one, which read and parsed every file a second time purely
	// to populate the index; measured on two corpus repositories it was 15-18% of
	// this extractor's wall time.
	//
	// Only one lookup in the walk is genuinely cross-file (the implementor calls of
	// a receiver's type), so that one is held back per file and answered below once
	// the index is merged. The other two only ask about the file's own module.
	type pyFileResult struct {
		ff      []facts.Fact
		topo    pyRouterTopology
		idx     *pySymbolIndex
		pending []pyImplCall
	}
	perFileFacts := parallel.MapFiles(ctx, pyFiles, func(relFile string) pyFileResult {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[python-extractor] error reading %s: %v", relFile, err)
			return pyFileResult{}
		}
		ff, topo, local, pending := extractFileIndexed(src, relFile, isDjango, isFlask, isFastAPI)
		// gRPC client call sites (stub.Method(...)) become client-role routes,
		// detected from source since generated *_pb2_grpc.py stubs are typically
		// not committed. Names are provisional (short service) and resolved to the
		// fully-qualified wire path by the engine before cross-repo linking.
		ff = append(ff, extractPyGRPCClientFacts(src, relFile)...)
		return pyFileResult{ff: ff, topo: topo, idx: local, pending: pending}
	})

	// Merge the per-file indices in file order, so duplicate-module last-write-wins
	// stays deterministic, then answer the held-back lookups against the result.
	idx := &pySymbolIndex{classes: make(map[string]*pyClassInfo), moduleDefs: make(map[string]map[string]bool)}
	for _, r := range perFileFacts {
		if r.idx == nil {
			continue
		}
		for qualName, info := range r.idx.classes {
			idx.classes[qualName] = info
		}
		for module, defs := range r.idx.moduleDefs {
			idx.moduleDefs[module] = defs
		}
	}
	finalizeImplMap(idx)
	for _, r := range perFileFacts {
		applyPendingImplementors(r.ff, r.pending, idx)
	}

	var allFacts []facts.Fact
	var routerTopos []pyRouterTopology
	modules := make(map[string]bool)
	for i, r := range perFileFacts {
		// Route refs index into the file's own slice; rebase them onto the
		// repo-wide one now that this file's offset is known.
		base := len(allFacts)
		for j := range r.topo.routes {
			r.topo.routes[j].idx += base
		}
		for j := range r.topo.paths {
			r.topo.paths[j].idx += base
		}
		allFacts = append(allFacts, r.ff...)
		routerTopos = append(routerTopos, r.topo)
		modules[factpath.Dir(pyFiles[i])] = true
	}

	// The packages (dirs with __init__.py) let suffix matching tell a real
	// top-level package from a like-named directory nested inside another package,
	// so a vendored-looking "…/relational/sqlalchemy" does not capture `import
	// sqlalchemy`.
	pkgDirs := packageDirs(pyFiles)

	// The file modules ("app/db" for app/db.py) let an absolute import that names a
	// module in the importer's OWN package bind to that sibling file; the directory
	// index alone can only reach the shared parent, which is the importer itself.
	fileModules := make(map[string]bool, len(pyFiles))
	for _, f := range pyFiles {
		fileModules[strings.TrimSuffix(f, ".py")] = true
	}

	// Resolve dotted import targets to internal module slash paths (and classify
	// stdlib/external) now that the full module set is known. Without this,
	// Python imports never match module Names downstream.
	resolveImports(allFacts, modules, fileModules, pkgDirs)

	// Parse pyproject.toml entry-points (console_scripts, plugin groups) into
	// reference edges, so functions registered as entry points — loaded by name by
	// the framework, never called in-code — are not mis-reported as dead. Emitted
	// before resolveCallTargets so their dotted targets resolve to slash symbols.
	allFacts = append(allFacts, extractEntryPoints(repoPath, files)...)

	// Resolve the dotted call/instantiate targets emitted for absolute imports into
	// canonical slash symbol names (dropping stdlib/third-party edges) now that the
	// full file set is known. Without this, functions reached via absolute imports
	// have no incoming edge and read as dead code.
	resolveCallTargets(allFacts, fileModules, pkgDirs)
	resolveImplementsTargets(allFacts, fileModules, pkgDirs)

	// Fold FastAPI include_router mount prefixes onto the bare decorator paths, so
	// a route reads as the path it actually serves ("/api/v1/cognify") rather than
	// the leaf its router declares ("/"). Runs last among the index-based passes:
	// it rebuilds the fact slice, invalidating the route indices it consumes.
	// Resolve routes registered on a computed path (`@router.get(HEALTH_PATH)`)
	// against the repository's string constants first, so composing joins the
	// mount prefix onto the real leaf. Unresolved ones are dropped after composing.
	resolveRoutePaths(allFacts, routerTopos, fileModules, pkgDirs, buildReexportIndex(allFacts, pkgDirs))
	allFacts = composeRouterPrefixes(allFacts, routerTopos, fileModules, pkgDirs)
	allFacts = dropPendingRoutes(allFacts)

	// Propagate the walk-time io_direct flag transitively across the (now canonical)
	// call graph into performs_io, so a function that reaches DB/network I/O only through
	// helpers is still flagged — the signal the performance analyzer reads to tell a real
	// per-iteration I/O call from a name that merely collides with a DB verb.
	computePyPerformsIO(allFacts)

	for dir := range modules {
		allFacts = append(allFacts, facts.Fact{
			Kind: facts.KindModule,
			Name: dir,
			File: dir,
			Props: map[string]any{
				"language": "python",
			},
		})
	}

	return allFacts, nil
}

// computePyPerformsIO propagates the per-body io_direct flag transitively across the
// call graph into a performs_io prop, so a function that reaches DB/network/file I/O only
// through helpers is still flagged. Mirrors computeTSPerformsIO: a monotone fixpoint that
// only ever flips false→true, so it is cycle-safe. Only edges to known symbol names
// propagate (unresolved external calls are ignored), so the closure stays within the repo.
func computePyPerformsIO(allFacts []facts.Fact) {
	exists := make(map[string]bool)
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			exists[allFacts[i].Name] = true
		}
	}

	io := make(map[string]bool)      // name → performs I/O (directly or transitively)
	adj := make(map[string][]string) // name → called names that are known symbols
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if b, _ := f.PropAny("io_direct").(bool); b {
			io[f.Name] = true
		}
		seen := make(map[string]bool)
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls || r.Target == f.Name || seen[r.Target] || !exists[r.Target] {
				continue
			}
			seen[r.Target] = true
			adj[f.Name] = append(adj[f.Name], r.Target)
		}
	}

	for changed := true; changed; {
		changed = false
		for name, callees := range adj {
			if io[name] {
				continue
			}
			for _, c := range callees {
				if io[c] {
					io[name] = true
					changed = true
					break
				}
			}
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol && io[f.Name] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp("performs_io", true)
		}
	}
}

// --- Regex patterns used by the AST walker ---

var (
	// routeDecoratorRe matches FastAPI/Starlette verb decorators AND Flask's
	// @app.route / @bp.route. Groups: (object, method, path). The path may be
	// positional (`@r.get("/x")`) or the `path=` keyword (`@r.get(path="/x")`), and
	// may be empty (`path=""` — the collection endpoint under the router prefix);
	// `\s*` spans the newline of a multi-line decorator. When method is `route`
	// (Flask), the HTTP verbs live in a `methods=[...]` kwarg parsed by
	// routeMethodsListRe below, defaulting to GET.
	routeDecoratorRe = regexp.MustCompile(`^\s*@([\w.]+)\.(route|api_route|websocket|websocket_route|get|post|put|delete|patch|head|options)\s*\(\s*(?:path\s*=\s*)?["']([^"']*)["']`)

	// routeMethodRe matches a route-method decorator in ANY path form (literal,
	// path= keyword, or a computed expression), used to tag the handler as a
	// framework-dispatched entry point even when the path isn't a parseable literal.
	routeMethodRe = regexp.MustCompile(`^\s*@[\w.]+\.(?:route|api_route|websocket|websocket_route|get|post|put|delete|patch|head|options)\s*\(`)

	// exposeDecoratorRe matches Flask-AppBuilder's @expose("/path", methods=[...]) —
	// which has no receiver dot, so routeDecoratorRe cannot match it. It is the
	// dominant route idiom in a real FAB app (289 of 293 route defs on superset).
	// Group: (path). Also matches the qualified `@self.expose(...)` form.
	exposeDecoratorRe = regexp.MustCompile(`^\s*@(?:[\w.]+\.)?expose\s*\(\s*["']([^"']*)["']`)

	// routeMethodsListRe extracts the `methods=[...]` kwarg of a Flask route/expose
	// decorator. Group: (list body) — the uppercase verbs are then pulled by
	// httpMethodWordRe, mirroring @api_view.
	routeMethodsListRe = regexp.MustCompile(`methods\s*=\s*[\[({]([^\])}]+)[\])}]`)

	// tableNameRe matches SQLAlchemy __tablename__ assignments. Group: (table).
	tableNameRe = regexp.MustCompile(`^\s*__tablename__\s*=\s*["']([^"']+)["']`)

	// decoratorRe captures the full decorator name for structural prop detection.
	// Group: (name) e.g. "staticmethod", "app.task".
	decoratorRe = regexp.MustCompile(`^\s*@([\w.]+)`)

	// apiViewRe matches Django REST Framework @api_view decorators.
	// Group: (methods_list) — bracket contents, e.g. "'GET', 'POST'"
	apiViewRe = regexp.MustCompile(`^\s*@(?:[\w.]*\.)?api_view\s*\(\s*\[([^\]]+)\]`)

	// httpMethodWordRe extracts uppercase HTTP method tokens from an api_view list.
	httpMethodWordRe = regexp.MustCompile(`[A-Z]+`)

	// urlPathRe matches Django path() and re_path() calls in urls.py.
	// Groups: (url_path, view_ref)
	urlPathRe = regexp.MustCompile(`(?:re_)?path\s*\(\s*r?["']([^"']+)["']\s*,\s*([\w.]+)`)

	// entryPointSectionRe matches pyproject.toml TOML section headers that declare
	// entry points: [project.scripts], [project.gui-scripts], and
	// [project.entry-points…] (incl. quoted group subtables).
	entryPointSectionRe = regexp.MustCompile(`^\s*\[\s*project\.(scripts|gui-scripts|entry-points)\b`)
	// anyTOMLSectionRe matches any TOML section header (ends an entry-point section).
	anyTOMLSectionRe = regexp.MustCompile(`^\s*\[`)
	// entryPointValueRe matches an entry-point line `name = "module.path:attr"`,
	// capturing the module path and the attribute (a function or Class.method).
	entryPointValueRe = regexp.MustCompile(`^\s*[^=\[\]#]+=\s*["']([A-Za-z_][\w.]*):([A-Za-z_][\w.]*)["']`)

	// registrationDecorators are decorator names (matched on the last dotted segment)
	// that REGISTER their function with a framework, which then dispatches it — so the
	// function has no in-code caller by construction (like a route handler or CLI
	// command). Covers SQLAlchemy (@compiles, @event.listens_for), functools
	// singledispatch (@base.register), signals (@sig.connect), and Flask app/blueprint
	// hooks. A function with any such decorator is marked used via a self file-ref edge.
	registrationDecorators = map[string]bool{
		"compiles": true, "listens_for": true, // SQLAlchemy
		"register":     true, // functools.singledispatch (also ABCMeta/registries)
		"connect":      true, // blinker/Celery/Django signals
		"errorhandler": true, "app_errorhandler": true,
		"before_request": true, "after_request": true,
		"teardown_request": true, "teardown_appcontext": true,
		"context_processor": true,
		"template_filter":   true, "template_global": true, "template_test": true,
		// MCP servers: FastMCP @mcp.tool / @mcp.resource / @mcp.prompt /
		// @mcp.custom_route register the function as a protocol handler the server
		// dispatches per client request — plus bare re-exported wrappers (@tool,
		// @prompt) that app cores build around FastMCP.
		"tool": true, "resource": true, "prompt": true, "custom_route": true,
	}

	// dottedPathRe matches a string literal that is an identifier-dotted symbol path
	// of at least 3 segments (module.sub.symbol), e.g. a lazy_load_command target or
	// a provider "class-name". The ≥3-segment floor avoids crediting short dotted
	// strings (logger names, config keys); resolveCallTargets prunes non-internal ones.
	dottedPathRe = regexp.MustCompile(`^[A-Za-z_]\w*(?:\.[A-Za-z_]\w*){2,}$`)
)

// extractEntryPoints scans each pyproject.toml in files for console-script / plugin
// entry points and emits a KindFileRef fact per file carrying RelCalls edges to the
// referenced module.attr targets (colon rewritten to dot). Entry-point functions are
// loaded by name by the framework, so without these edges they read as dead code.
// The dotted targets are resolved to slash symbols by resolveCallTargets.
func extractEntryPoints(repoPath string, files []string) []facts.Fact {
	var out []facts.Fact
	for _, rel := range files {
		if filepath.Base(rel) != "pyproject.toml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		var rels []facts.Relation
		inEntryPoints := false
		for _, line := range strings.Split(string(data), "\n") {
			if anyTOMLSectionRe.MatchString(line) {
				inEntryPoints = entryPointSectionRe.MatchString(line)
				continue
			}
			if !inEntryPoints {
				continue
			}
			if m := entryPointValueRe.FindStringSubmatch(line); m != nil {
				rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: m[1] + "." + m[2]})
			}
		}
		if len(rels) > 0 {
			out = append(out, facts.Fact{
				Kind:      facts.KindFileRef,
				Name:      rel,
				File:      rel,
				Line:      1,
				Props:     map[string]any{"language": "python"},
				Relations: rels,
			})
		}
	}
	return out
}

// Django class base sets used to classify models, views, and serializers.
var (
	djangoModelBases = map[string]bool{
		"Model": true, "AbstractModel": true, "MPTTModel": true,
		"TimeStampedModel": true, "UUIDModel": true, "PolymorphicModel": true,
	}

	djangoCBVBases = map[string]bool{
		"View": true, "APIView": true, "GenericAPIView": true,
		"ListAPIView": true, "CreateAPIView": true, "RetrieveAPIView": true,
		"UpdateAPIView": true, "DestroyAPIView": true, "ListCreateAPIView": true,
		"RetrieveUpdateDestroyAPIView": true, "ViewSet": true, "ModelViewSet": true,
		"ReadOnlyModelViewSet": true, "TemplateView": true, "DetailView": true,
		"ListView": true, "CreateView": true, "UpdateView": true, "DeleteView": true,
		"FormView": true, "RedirectView": true,
	}

	djangoSerializerBases = map[string]bool{
		"Serializer": true, "ModelSerializer": true,
		"HyperlinkedModelSerializer": true, "ListSerializer": true,
	}

	// pyAbstractBases are base classes / metaclasses that make a class abstract
	// in Python's sense (no direct interface keyword). Used to set the "abstract"
	// prop so package-metrics abstractness (A) is meaningful for Python.
	pyAbstractBases = map[string]bool{
		"ABC": true, "ABCMeta": true, "Protocol": true,
	}

	// pyEnumBases mark a class as an enum. Enums are concrete value enumerations,
	// not domain types, so package-metrics excludes them from N (mirrors the Kotlin
	// enum handling) — otherwise a pure-enum package skews abstractness/distance.
	pyEnumBases = map[string]bool{
		"Enum": true, "IntEnum": true, "StrEnum": true,
		"Flag": true, "IntFlag": true, "ReprEnum": true,
	}

	// pyDataHolderBases mark a class as a data holder (DTO / schema / record):
	// Pydantic models/settings, typing.NamedTuple, and TypedDict. These are value
	// carriers, the Python analogue of TypeScript structural interfaces — concrete
	// BY DESIGN. package-metrics uses the "data_class" prop to keep such packages out
	// of the "rigid — extract interfaces" off-main-sequence finding, which is not
	// actionable for schema/model bundles (e.g. OpenAPI-generated Pydantic models).
	// A base whose name ends in "BaseModel" is also treated as a data holder, which
	// covers project-local Pydantic subclasses used as a common base (StrictBaseModel,
	// <App>BaseModel) — see isDataHolderBase.
	pyDataHolderBases = map[string]bool{
		"BaseModel": true, "RootModel": true, "GenericModel": true,
		"BaseSettings": true, "NamedTuple": true, "TypedDict": true,
	}
)

// isDataHolderBase reports whether a base-class short name marks the subclass as a
// data holder: an exact Pydantic/typing data base, or any "*BaseModel" name (the
// idiomatic project-local Pydantic base, e.g. StrictBaseModel).
func isDataHolderBase(last string) bool {
	return pyDataHolderBases[last] || strings.HasSuffix(last, "BaseModel")
}

// applyDecoratorProps sets structural boolean props on a symbol based on a
// decorator name. Only well-known structural decorators produce props; unknown
// decorators are silently ignored.
func applyDecoratorProps(props map[string]any, decoratorName string, importsModal bool) {
	// Use the last dot-separated component: "functools.cached_property" → "cached_property".
	last := decoratorName
	if idx := strings.LastIndex(decoratorName, "."); idx >= 0 {
		last = decoratorName[idx+1:]
	}
	switch last {
	case "property", "cached_property":
		props["property"] = true
	case "staticmethod":
		props["static"] = true
	case "classmethod":
		props["class_method"] = true
	case "abstractmethod":
		props["abstract"] = true
	case "dataclass":
		// @dataclass / @dataclasses.dataclass / @pydantic.dataclasses.dataclass.
		props["data_class"] = true
	case "define", "frozen", "mutable", "attrs":
		// attrs data classes: @attrs.define / @define / @frozen / @attr.attrs.
		props["data_class"] = true
	case "task":
		props["task"] = true
	case "command", "group":
		// click/Typer CLI command or group (@cli.command, @app.group). The function
		// is registered with and dispatched by the CLI framework, never called by
		// name, so the dead-code detector treats it as an entry point.
		props["cli_command"] = true
	case "shared_task":
		// shared_task is Celery-specific; bare @task is used by Airflow, Prefect, Luigi, etc.
		props["task"] = true
		props["framework"] = "celery"
	case "exception_handler", "middleware", "on_event", "websocket":
		// FastAPI/Starlette registration decorators (@app.exception_handler(Err),
		// @app.middleware("http"), @app.on_event("startup")). The framework invokes
		// the handler for a matching event; nothing calls it by name, so it has no
		// incoming call edge by construction — an entry point, not dead code. These
		// names are distinctive enough to match without a framework guard.
		props["framework_registered"] = true
	case "local_entrypoint":
		// Modal's CLI entry point — distinctive, no guard needed.
		props["framework_registered"] = true
	case "function", "cls":
		// Modal's remote-function decorators (@app.function(...), @app.cls(...)).
		// Unlike the names above these are far too generic to match on their own —
		// "function" would swallow any @x.function-decorated symbol in any codebase —
		// so they count only in a file that imports modal.
		if importsModal {
			props["framework_registered"] = true
		}
	}
	// @attr.s is the legacy attrs data-class decorator; its last component "s" is
	// too generic to switch on, so match the full "attr.s" path explicitly.
	if last == "s" && strings.HasPrefix(decoratorName, "attr") {
		props["data_class"] = true
	}
}

// detectDjango returns true if the project at repoPath uses Django, by scanning
// common dependency files and checking for manage.py.
func detectDjango(repoPath string) bool {
	for _, name := range []string{"requirements.txt", "pyproject.toml", "setup.cfg", "setup.py"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(string(data)), "django") {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(repoPath, "manage.py"))
	return err == nil
}

// detectDependencyToken reports whether any of the project's dependency manifests
// mentions token (case-insensitive). Shared by the Flask/FastAPI detectors, which
// disambiguate the framework prop of verb-shorthand routes (@app.get).
func detectDependencyToken(repoPath, token string) bool {
	for _, name := range []string{"requirements.txt", "pyproject.toml", "setup.cfg", "setup.py"} {
		data, err := os.ReadFile(filepath.Join(repoPath, name))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(string(data)), token) {
			return true
		}
	}
	return false
}

// detectFlask returns true if the project at repoPath depends on Flask.
func detectFlask(repoPath string) bool { return detectDependencyToken(repoPath, "flask") }

// detectFastAPI returns true if the project at repoPath depends on FastAPI.
func detectFastAPI(repoPath string) bool { return detectDependencyToken(repoPath, "fastapi") }

// camelToSnake converts a PascalCase class name to the snake_case table name
// Django would auto-generate. e.g. "UserProfile" → "user_profile".
func camelToSnake(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if i > 0 && ch >= 'A' && ch <= 'Z' {
			b.WriteByte('_')
		}
		if ch >= 'A' && ch <= 'Z' {
			b.WriteByte(ch + 32) // ASCII lowercase
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// lastComponent returns the last dot-separated segment of a qualified name.
// e.g. "models.Model" → "Model", "Model" → "Model".
func lastComponent(name string) string {
	if idx := strings.LastIndex(name, "."); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

// isPythonFile returns true if the file has a .py extension.
func isPythonFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".py")
}

// OwnsFile implements plugin.FileOwner for incremental caching.
func (e *PythonExtractor) OwnsFile(relFile string) bool { return isPythonFile(relFile) }

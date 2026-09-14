package engine_test

// End-to-end golden + determinism tests for the full extraction pipeline.
//
// Unlike the per-extractor unit tests (which feed inline snippets to a single
// extractor), these run the real engine wired exactly as production wires it
// (via bootstrap.NewEngine: all 9 extractors, 9 explainers, the llm_context
// renderer) over small fixture repos under testdata/repos, then assert the
// emitted fact graph byte-for-byte against a committed golden file.
//
// This is the regression net for Enola's core promise — "deterministic,
// structurally faithful extraction." A golden mismatch means the fact graph
// for a known repo changed; a reviewer diffs the regenerated JSONL to decide
// whether the change is intended. Regenerate with:
//
//	go test ./internal/engine -run TestGolden -update
//
// The golden captures only the fact graph (Store.WriteJSONL, which sorts facts
// and their relations deterministically). Snapshot metadata (timestamps,
// durations, file hashes, absolute repo path) is intentionally excluded so the
// golden is stable across machines and runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/golden")

// fixture describes a testdata repo and the sub-repos to snapshot, in order.
// Single-repo fixtures use one entry ("."); multi-repo fixtures list each
// sub-repo so the harness can drive append mode (the 2nd+ repo with append=true).
type fixture struct {
	name     string
	subRepos []string

	// goldenInsights additionally pins the FINDINGS this fixture produces, in
	// <name>.insights.json beside the fact golden.
	//
	// Facts alone are not enough for a fixture that exists to pin what an explainer
	// CONCLUDES. A declared layer order is the case that forced this: every fact
	// behind it — the modules, the import edge, the compiled intent — can be
	// perfectly correct while the finding says the order classifies nothing, which
	// is exactly the shape issue #242 shipped in. Opt-in rather than universal
	// because most fixtures pin extraction, and re-recording every finding on every
	// heuristic tweak would make the goldens noise.
	goldenInsights bool

	// dir is the tree under testdata/repos, when it is not name. Several goldens can
	// share one tree and differ only in config, which is how a behavior that config
	// switches on is pinned beside the default it must leave untouched.
	dir string

	// config is a file under testdata/configs the engine is built with. Empty means
	// the built-in defaults, as every fixture had before.
	config string
}

// sourceDir returns the fixture tree to copy.
func (f fixture) sourceDir() string {
	d := f.dir
	if d == "" {
		d = f.name
	}
	return filepath.Join("testdata", "repos", d)
}

// configPath returns the config to build the engine with. A named config that is
// missing fails the test: bootstrap treats a missing path as "use defaults", so a
// typo here would otherwise record a default run under a configured golden's name.
func (f fixture) configPath(t *testing.T) string {
	t.Helper()
	if f.config == "" {
		return filepath.Join(t.TempDir(), "no-such-config.yaml")
	}
	p := filepath.Join("testdata", "configs", f.config)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s names config %s: %v", f.name, p, err)
	}
	return p
}

var fixtures = []fixture{
	{name: "go_sample", subRepos: []string{"."}},
	{name: "ts_sample", subRepos: []string{"."}},
	{name: "python_sample", subRepos: []string{"."}},
	// Flask app: @app.route (methods=), @app.get shorthand, a Blueprint @bp.route,
	// and Flask-AppBuilder @expose views. Pins GAP-PY-01 (v109) — routes detected
	// and framework=flask (the @app.get shorthand is NOT mislabeled fastapi).
	{name: "python_flask_sample", subRepos: []string{"."}},
	// TypeScript ORMs: a TypeORM @Entity class, a Drizzle pgTable const, and Prisma
	// models in schema.prisma (read off-glob). Pins GAP-XL-04's TS half (v112) —
	// tsextractor emitted ZERO storage facts, so a database-backed Node service
	// modelled no tables at all. Also pins the io_direct seeding: repo.ts wraps an ORM
	// call in loadPostsFor() and calls it once per iteration, which is only detectable
	// as an N+1 once the ORM call seeds performs_io through the wrapper.
	{name: "ts_orm_sample", subRepos: []string{"."}},
	// Ember app: two .gts template-tag components (one importing the other through a
	// tsconfig path alias, one injecting a service), a .gjs holding a named-binding
	// component AND a default-export template (expression-position blanking; each
	// owns its own template's refs, including a same-file reference), a classic
	// .hbs + .js component pair invoking a component and a helper, a template-only
	// .hbs, a route template owned by its route class and using a modifier, a
	// Router.map with nested paths, ember-data models with a belongsTo/hasMany
	// relationship edge, and a service. Pins v148 end-to-end: template blanking
	// preserves line numbers, strict-mode template refs resolve through imports and
	// locals, and the ember-resolver binder joins .hbs invocations, @service
	// injections and model relationships to the symbols the files actually declare.
	{name: "ts_ember_sample", subRepos: []string{"."}},
	// React Navigation: screen registrations become page routes handled_by their
	// imported components; a literal navigate() becomes a navigation edge from
	// the enclosing symbol. Pins v151's RN half.
	{name: "ts_reactnav_sample", subRepos: []string{"."}},
	// A graphql-ruby server beside a gql-tag client. Pins the GraphQL seam end
	// to end: root-field route facts on both sides, the graphql cross-repo
	// signal drawing client -> server on the exact field name, the unserved
	// operation counted but unlinked, and GraphQL staying OUT of HTTP matching.
	{name: "graphql_multirepo", subRepos: []string{"server", "client", "reporter"}},
	// Terraform: blocks as symbols, literal references (prefixed, declared bare
	// addresses, depends_on lists) as edges, a local module source drawing the
	// directory dependency. Pins v151's HCL extractor.
	{name: "hcl_sample", subRepos: []string{"."}},
	// Ansible: plays depend on the roles they list, import_role draws role-to-
	// role edges, templates count without rendering. Self-walking (YAML is
	// ignore-globbed), so the fixture also pins that the walk stays in bounds.
	{name: "ansible_sample", subRepos: []string{"."}},
	{name: "ruby_sample", subRepos: []string{"."}},
	{name: "swift_sample", subRepos: []string{"."}},
	{name: "kotlin_sample", subRepos: []string{"."}},
	{name: "rust_sample", subRepos: []string{"."}},
	// Scala: a two-project sbt build where `app` extends a trait declared in
	// `core` and imports a JAVA type from that same package, so cross-module and
	// cross-LANGUAGE resolution are both exercised (spark and pekko are both
	// majority-Scala repos holding hundreds of .java files). Also pins v181's two
	// measured decisions, and pins them by ABSENCE and PRESENCE respectively:
	// app/src/test/scala/.../ServiceSpec.scala contributes no symbols and no module
	// (the sbt test source set is excluded) while still carrying its references as a
	// test_ref, whereas core/src/main/scala/com/example/core/test/
	// Fixtures.scala IS extracted — production code under a directory merely named
	// `test`, which a one-segment glob would have deleted along with zio's entire
	// test library. Plus Scala 3 braceless bodies, enum/given/extension, and a file
	// whose chained package deliberately disagrees with its directory.
	{name: "scala_sample", subRepos: []string{"."}},
	// gin. Pins v191: Group("/") joins rather than concatenates (so /ping, never
	// //ping — the failure that would silently break every cross-repo match under a
	// no-prefix group), a real prefix composes and nests, the handler is the LAST
	// argument because gin takes variadic middleware first, and func(*gin.Context) is
	// tagged http_handler so each route binds to the function serving it.
	{name: "go_gin_sample", subRepos: []string{"."}},
	// Dart/Flutter. Pins v190's decisions, several by ABSENCE:
	// lib/models/user.freezed.dart yields nothing at all (generated code is the
	// majority of files in a build_runner project), and lib/models/user_helpers.dart
	// declares no imports of its own yet is still walked with user.dart's — a `part`
	// shares its host library's import scope, which every framework gate depends on.
	// The three GoRoute shapes are all present: a literal path, one declared as
	// `SettingsScreen.routeName` and resolved repo-wide, and a nested relative path
	// composed onto its parent. Every navigation route carries type "page", so
	// routeindex.IsUIRoute keeps a screen out of the cross-repo server index.
	// `class TodoItems extends Table` is a drift table ONLY because the file imports
	// drift — Table is also a Flutter layout widget.
	{name: "dart_sample", subRepos: []string{"."}},
	// The cross-repo half: a Flutter client's http call sites resolving against a Go
	// net/http server. Pins that a Dart client participates in the graph of graphs at
	// all, and that /api/internal/reindex — served but called by no loaded client —
	// stays an unused-route candidate rather than being matched by a page route.
	{name: "dart_multirepo", subRepos: []string{"mobile", "api"}},
	{name: "java_sample", subRepos: []string{"."}},
	{name: "cpp_sample", subRepos: []string{"."}},
	// C#: both namespace spellings, a positional record, an interface whose
	// members carry no modifier, constructor injection through a `using` alias,
	// and a partial type split across two files. Pins v164's three C#-specific
	// decisions: the two Widget halves fold into ONE symbol carrying the union of
	// their edges, private state produces no symbol while public properties do,
	// and a `.g.cs` file yields nothing at all — the absence of a
	// src/Acme.Api/Generated module is what proves it was skipped rather than
	// merely emptied.
	{name: "csharp_sample", subRepos: []string{"."}},
	// ASP.NET Core attribute routing. Pins v165: a controller inherits its
	// [Route("[controller]")] from a base class in ANOTHER file and resolves the
	// token to its own name, a named argument stays out of the path, a leading "/"
	// makes a template absolute, and — by their ABSENCE from the golden —
	// AccountController's conventionally-routed actions mint no route at all
	// rather than collapsing onto "/".
	{name: "csharp_aspnet_sample", subRepos: []string{"."}},
	{name: "php_sample", subRepos: []string{"."}},
	{name: "php_laravel_sample", subRepos: []string{"."}},
	{name: "php_symfony_sample", subRepos: []string{"."}},
	{name: "openapi_sample", subRepos: []string{"."}},
	// AsyncAPI producer and consumer contracts become directional messaging topic
	// facts; the consumer contract links billing -> orders through Kafka.
	{name: "asyncapi_multirepo", subRepos: []string{"svc-orders", "svc-billing"}},
	{name: "multirepo", subRepos: []string{"repoA", "repoB"}},
	{name: "php_multirepo", subRepos: []string{"provider", "consumer"}},
	{name: "go_grpc_multirepo", subRepos: []string{"server", "client"}},
	// A Go backend plus a Go client that calls it and two third-party APIs. Pins
	// GAP-LK-02 (v101): a `baseURL + "/path"` concat to a hardcoded host is tagged
	// external, a hardcoded INTERNAL host still resolves to its loaded repo, and a
	// config-injected base URL stays an unresolved internal edge.
	{name: "go_httpclient_multirepo", subRepos: []string{"api", "consumer"}},
	// Two Go services coupled ONLY by Kafka topics — no import, no call, no HTTP
	// route between them. Pins the async linking signal (v132) end-to-end, which the
	// unit tests cannot: the topic name the extractor emits has to resolve against
	// the repo label the ENGINE assigns (the directory basename). Covers all four
	// outcomes — a consumed topic owned by a loaded repo draws the edge (including
	// one the producer declares no fact for, so it resolves from the consumer side
	// alone), an own topic and an unowned topic draw nothing, and an in-process event
	// bus emits no topic fact at all.
	{name: "go_kafka_multirepo", subRepos: []string{"svc-orders", "svc-billing"}},
	{name: "py_grpc_multirepo", subRepos: []string{"server", "client"}},
	// A FastAPI backend with its own frontend, beside an API-compatible rewrite
	// that also publishes npm packages under an @acme scope. Pins v133 end-to-end,
	// which the unit tests cannot: (a) routes declared on a factory-built router
	// resolve to their mounted path ("/api/v1/search/results", not "/results"),
	// (b) the frontend's calls bind to the backend IN ITS OWN REPO rather than to
	// the rewrite that serves the same shapes, and (c) acme-rs importing
	// "@acme/native-darwin-arm64" — a package under the scope it publishes itself —
	// draws no import edge to the repo labeled "acme". Both cross-repo edges here
	// are false positives the linker used to emit; the golden pins their absence.
	{name: "py_fastapi_multirepo", subRepos: []string{"acme", "acme-rs"}},
	// Two different-language repos sharing only nested type names. The linker must
	// draw no shared_symbols edge between them; see GAP-LK-03.
	{name: "kotlin_swift_multirepo", subRepos: []string{"android", "ios"}},
	// A Spring backend plus a Java consumer calling it through BOTH hand-written
	// client forms — RestTemplate (source="java-http-client") and @FeignClient
	// (source="feign"). Pins the contract-vocabulary fix that no unit test could:
	// the cross-repo linker kept its own private copy of the hand-written client
	// source set and had never included either Java value, so every Java call site
	// linked as via="http" — indistinguishable from an edge merely implied by an
	// OpenAPI spec. The golden now pins via=["http-client"] end to end, through the
	// real extractor rather than synthetic facts. java_sample cannot cover this:
	// it is single-repo, and a single-repo snapshot draws no cross-repo edge at
	// all, which is exactly why the omission survived undetected.
	{name: "java_httpclient_multirepo", subRepos: []string{"inventory", "storefront"}},
	// A decorator-routed TypeScript backend plus an SDK that calls it. Pins v142 end
	// to end, which the unit tests cannot: the server routes the @Controller classes
	// compose to have to RESOLVE against the SDK's client calls and draw a cross-repo
	// edge. Before v142 TypeScript had no server-side route DSL at all, so the api
	// repo emitted zero routes and was classified `isolated` while every SDK call sat
	// unresolved. Covers both argument forms (@Controller({path}) and
	// @Controller("…")), a bare @Get() serving the class path, the InversifyJS
	// vocabulary, and — by its absence from the golden — a verb decorator on a
	// non-controller class minting nothing.
	{name: "ts_nest_multirepo", subRepos: []string{"api", "sdk"}},
	// A call-routed Express server plus a consumer that calls it. Pins v143's three
	// rules, none of which a unit test can prove end to end: (a) receiver binding
	// separates registrations from v141's identically-shaped client calls, so no call
	// site is emitted twice and no client route is reclassified; (b) a sub-router
	// mounted in the SAME file composes ('/admin/users'), while one mounted from
	// another file emits nothing rather than a wrong fragment path ('/login'); and
	// (c) a bare catch-all is not an endpoint. The consumer's fourth call is served by
	// nobody, so it stays unresolved — the control that the linker is matching real
	// paths rather than accepting anything.
	{name: "ts_express_multirepo", subRepos: []string{"server", "consumer"}},
	// A declared layer order, end to end — the fixture for issue #242. It pins the
	// findings as well as the facts (goldenInsights), because every fact behind a
	// layer order can be correct while the CONCLUSION drawn from them is that the
	// order classifies nothing, which is the shape the bug shipped in.
	//
	// Three decisions are pinned at once, and two of them by their finding alone:
	// `web-components` is declared with BACKSLASHES and must classify exactly as its
	// forward-slash siblings do; `web-legacy` names a directory that does not exist
	// and must raise the advisory rather than pass silently; and src/lib importing
	// src/components — innermost reaching up into the layer above it — must be a
	// violation at confidence 1.00, which is what makes `--fail-on=layers` bite.
	{name: "ts_layers_sample", subRepos: []string{"."}, goldenInsights: true},
	// An in-house HTTP wrapper (sendRequest(serviceName, path, options)) called from a
	// shared SDK, against NestJS servers. With no config it is invisible: no client
	// route, no sdk -> gateway edge. This golden pins that miss so the configured
	// goldens over the same tree show exactly what config changes and nothing else.
	// replica serves gateway's literal route (ambiguity control), MessageBus calls a
	// sendRequest on an unconfigured type (precision control), and backend reaches the
	// gateway only through the SDK.
	{name: "ts_custom_client_default", dir: "ts_custom_client_cluster",
		subRepos: []string{"gateway", "replica", "sdk", "backend"}},
	// The same tree with the client declared and no service alias. sendRequest through
	// IHttpRequestService becomes client routes; MessageBus's sendRequest on another
	// type, and the call whose path is a method result, do not. Still no edge: the
	// literal route has two providers and the service name names neither, and the
	// parameterized route needs literal-against-parameter matching.
	{name: "ts_custom_client_no_alias", dir: "ts_custom_client_cluster", config: "ts_custom_client_no_alias.yaml",
		subRepos: []string{"gateway", "replica", "sdk", "backend"}},
	// The client declared and its service name aliased to gateway. The literal route
	// both gateway and replica serve now resolves to gateway alone: the edge the
	// feature exists for. The parameterized route stays unresolved until
	// literal-against-parameter matching. Findings pinned too, since the edge changes
	// what the crossrepo and unused-routes explainers conclude.
	{name: "ts_custom_client_configured", dir: "ts_custom_client_cluster", config: "ts_custom_client_configured.yaml",
		subRepos: []string{"gateway", "replica", "sdk", "backend"}, goldenInsights: true},
	// The configured tree with literal-against-parameter matching turned on. The call to
	// /v1/resources/catalog/items now reaches gateway's /v1/resources/:type/items: a
	// second endpoint on the same edge, probable, and listed as resting on a parameter.
	// gateway's parameter route stops reading as unused.
	{name: "ts_custom_client_param_match", dir: "ts_custom_client_cluster", config: "ts_custom_client_param_match.yaml",
		subRepos: []string{"gateway", "replica", "sdk", "backend"}, goldenInsights: true},
}

func TestGolden(t *testing.T) {
	for _, f := range fixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			got, insights := snapshotFixture(t, f)
			assertGolden(t, f.name, got)
			if f.goldenInsights {
				assertInsightsGolden(t, f.name, insights)
			}
		})
	}
}

// snapshotFixture copies the fixture repo into a temp dir, runs the full
// pipeline (append mode for multi-repo fixtures), and returns the normalized,
// deterministic JSONL of the resulting fact graph.
func snapshotFixture(t *testing.T, f fixture) ([]byte, []facts.Insight) {
	t.Helper()

	root := copyTree(t, f.sourceDir(), t.TempDir())

	// Build the engine the same way production does, so the golden reflects the
	// real OSS plugin wiring. A non-existent config path falls back to defaults.
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: f.configPath(t),
	})
	if err != nil {
		t.Fatalf("bootstrap.NewEngine: %v", err)
	}

	var insights []facts.Insight
	for i, sub := range f.subRepos {
		repoPath := root
		if sub != "." {
			repoPath = filepath.Join(root, sub)
		}
		appendMode := i > 0
		snap, err := eng.GenerateSnapshot(context.Background(), repoPath, appendMode)
		if err != nil {
			t.Fatalf("GenerateSnapshot(%s, append=%v): %v", sub, appendMode, err)
		}
		// The last snapshot's findings are the union's findings: explainers run over
		// the whole store each time, so the final pass has seen every repo.
		insights = snap.Insights
	}

	var buf bytes.Buffer
	if err := eng.Store().WriteJSONL(&buf); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}
	return normalize(buf.Bytes(), root), insights
}

// assertInsightsGolden pins a fixture's findings.
//
// Only the fields a reader would review are captured — the title, which explainer
// raised it, its confidence, whether it is descriptive, and its evidence. Free-text
// descriptions and suggested actions are excluded on purpose: they are prose, they
// get rewritten, and a golden that fails on a reworded sentence teaches people to
// regenerate it without reading the diff.
func assertInsightsGolden(t *testing.T, name string, insights []facts.Insight) {
	t.Helper()

	type evidence struct {
		Fact   string `json:"fact,omitempty"`
		File   string `json:"file,omitempty"`
		Detail string `json:"detail,omitempty"`
	}
	type record struct {
		Source        string     `json:"source"`
		Title         string     `json:"title"`
		Confidence    float64    `json:"confidence"`
		Informational bool       `json:"informational,omitempty"`
		Evidence      []evidence `json:"evidence,omitempty"`
	}

	records := make([]record, 0, len(insights))
	for _, in := range insights {
		r := record{Source: in.Source, Title: in.Title, Confidence: in.Confidence, Informational: in.Informational}
		for _, e := range in.Evidence {
			r.Evidence = append(r.Evidence, evidence{Fact: e.Fact, File: e.File, Detail: e.Detail})
		}
		records = append(records, r)
	}
	// Sorted here rather than trusted from the explainers: the golden's job is to
	// pin WHAT was concluded, and a fixture failing because two explainers ran in a
	// different order would be a false alarm about the wrong thing.
	sort.Slice(records, func(i, j int) bool {
		if records[i].Source != records[j].Source {
			return records[i].Source < records[j].Source
		}
		return records[i].Title < records[j].Title
	})

	got, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatalf("marshalling insights: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", "golden", name+".insights.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write insights golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read insights golden (run `go test ./internal/engine -run TestGolden -update` to create it): %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("insights golden mismatch for %s; run `go test ./internal/engine -run TestGolden -update` and review the diff.\n%s",
			name, firstDiff(want, got))
	}
}

// normalize defends against any absolute temp path leaking into a fact by
// replacing the temp repo root with a stable placeholder. Facts use repo-
// relative paths today, so this is usually a no-op, but it keeps the golden
// machine-independent if that ever changes.
func normalize(b []byte, root string) []byte {
	out := strings.ReplaceAll(string(b), root, "<REPO>")
	return []byte(out)
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".facts.jsonl")

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/engine -run TestGolden -update` to create it): %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("golden mismatch for %s; run `go test ./internal/engine -run TestGolden -update` and review the diff.\n%s",
			name, firstDiff(want, got))
	}
}

// firstDiff returns a short, human-readable description of the first differing
// line between want and got, so failures point at the regression directly
// instead of dumping the entire fact graph.
func firstDiff(want, got []byte) string {
	wl := strings.Split(string(want), "\n")
	gl := strings.Split(string(got), "\n")
	n := len(wl)
	if len(gl) < n {
		n = len(gl)
	}
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			return "first diff at line " + itoa(i+1) + ":\n  - want: " + wl[i] + "\n  + got:  " + gl[i]
		}
	}
	if len(wl) != len(gl) {
		return "line count differs: want " + itoa(len(wl)) + " got " + itoa(len(gl))
	}
	return "(no line-level diff found)"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// copyTree recursively copies src into a fresh subdir of dstParent and returns
// the new root. Each fixture is copied per-test so the pipeline (and the MCP
// generate_snapshot handler, which writes .enola/) never touches the source
// tree under version control.
func copyTree(t *testing.T, src, dstParent string) string {
	t.Helper()
	dst := filepath.Join(dstParent, filepath.Base(src))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", src, err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		if e.IsDir() {
			copyTree(t, s, dst)
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatalf("read %s: %v", s, err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", filepath.Join(dst, e.Name()), err)
		}
	}
	return dst
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/internal/providers"
)

// Config represents the mcp-arch.yaml configuration.
type Config struct {
	Repo string `yaml:"repo"`

	// Repos is the ordered list of repositories that form a multi-repo cluster.
	// When set it supersedes Repo, and the whole cluster is indexed in one run —
	// the first repository fresh, the rest appended.
	//
	// It exists because cross-repo linking was previously reachable only from an
	// MCP session: `append` is a generate_snapshot tool parameter, and both CLIs
	// hardcoded false, so a CI job or a developer not driving an agent could only
	// ever see the single-repo subset (no service nodes, no cross-repo edges, no
	// coverage_report, no unused-routes). Naming the cluster in the config also
	// makes its composition a reviewable file rather than a property of the order
	// somebody happened to issue tool calls in.
	//
	// Entries are resolved relative to the DIRECTORY OF THIS FILE, not the working
	// directory, so a checked-in cluster config means the same thing wherever it is
	// run from. (Repo keeps its historical cwd-relative behaviour.)
	Repos []string `yaml:"repos"`

	// Intent is the cluster config's intent block: declared architectural
	// intent keyed by repo label, for repos the operator observes but does not
	// own. An entry here overrides that repo's own enola-intent.yaml wholesale
	// (never key-by-key), and the override is reported — see the intent
	// package for the composition rule and vocabulary validation.
	Intent map[string]*intent.Declaration `yaml:"intent"`

	// SourcePath is the file this config was read from, or "" when the built-in
	// defaults are in force. Not read from YAML — set by Load, and used to resolve
	// Repos entries against the config's own directory.
	SourcePath string `yaml:"-"`

	// ExtractorsExplicit reports whether the file named `extractors:` itself, as
	// opposed to inheriting Default()'s list. Not read from YAML — set by Load.
	//
	// It exists because the two cases are indistinguishable afterwards and mean
	// opposite things. An explicit list REPLACES the defaults (YAML unmarshalling of
	// a sequence overwrites the slice), so a config written before an extractor
	// existed permanently disables it — silently, since a disabled extractor is
	// simply never tried. Knowing the list was explicit lets the engine say so when
	// a disabled extractor would have detected the repository.
	ExtractorsExplicit bool `yaml:"-"`

	Ignore     []string     `yaml:"ignore"`
	TestGlobs  []string     `yaml:"test_globs"`
	Extractors []string     `yaml:"extractors"`
	Explainers []string     `yaml:"explainers"`
	Renderers  []string     `yaml:"renderers"`
	Output     OutputConfig `yaml:"output"`

	// Providers names the external fact providers the engine runs at snapshot
	// time, before linking: for each, a census name, the command (argv) to
	// execute, and the version the installed build is expected to report. See
	// internal/providers for the exchange contract; changing this list changes
	// emitted facts, so it is folded into the snapshot's config hash and the
	// ran-provider set is compared between snapshots like the extractor set.
	Providers []providers.Provider `yaml:"providers"`

	// Linking overlays the cross-repo linker's tuning vocabulary — the word lists that
	// decide which names are too generic to link on, and the numeric thresholds. It is
	// ADDITIVE over the built-in defaults (see vocab.Overlay), so fixing one false edge
	// cannot silently discard the rest.
	//
	// Changing it changes emitted facts, so it is folded into the snapshot's config
	// hash; two snapshots taken under different vocabularies are not comparable and the
	// receipt says so.
	Linking *vocab.Overlay `yaml:"linking,omitempty"`

	// Clients declares in-house HTTP clients: which method on which injected type makes
	// a request, and where its service name, path and verb sit among the arguments. It
	// teaches an extractor to read a call site and never declares an edge; see
	// internal/clientspec. It changes emitted facts, so it is folded into the config
	// hash and into the reading extractor's cache key.
	Clients []clientspec.Spec `yaml:"clients,omitempty"`

	// ServiceAliases maps a service name a client passes (a string argument, not a
	// host) to the repository label that serves it, for when the two differ. It only
	// chooses between repositories that already serve a called path; it never makes a
	// repository a candidate. Folded into the config hash.
	ServiceAliases map[string]string `yaml:"service_aliases,omitempty"`

	// Dashboard configures the localhost dashboard served alongside the MCP
	// server. Optional: the zero value keeps the built-in defaults.
	Dashboard DashboardConfig `yaml:"dashboard"`

	// History records each snapshot as a revision in an append-only architecture
	// history, so a repository has a timeline rather than only a current state and one
	// baseline. Optional; the zero value keeps the built-in defaults.
	//
	// Deliberately ABSENT from computeConfigHash: it changes what enola remembers, never
	// what it extracts. Folding it in would make every snapshot taken with history on
	// incomparable with every snapshot taken with it off — turning an experimental,
	// per-user setting into a reason to decline to grade somebody's change.
	History HistoryConfig `yaml:"history"`

	// Incremental enables per-extractor caching: an extractor's facts are reused
	// across snapshots when the files it owns (and the repo's shared config files)
	// are unchanged. Defaults to true. Set `incremental: false` to force a full
	// re-extraction every run.
	Incremental *bool `yaml:"incremental,omitempty"`

	// ChangeVerifyHint is optional extra guidance a host/wrapper can inject into the
	// change-verification surfaces — the server Instructions block and the runtime
	// post-snapshot nudge (loopHint). It lets a wrapper that registers additional
	// change-verification tools draw the agent toward them from the same
	// generate_snapshot → diff loop the OSS tools advertise. Not read from YAML; set
	// programmatically before the server is constructed. Empty by default (OSS).
	ChangeVerifyHint string `yaml:"-"`
}

// IncrementalEnabled reports whether per-extractor caching is on (the default).
func (c *Config) IncrementalEnabled() bool {
	return c.Incremental == nil || *c.Incremental
}

// DashboardConfig controls the localhost dashboard.
//
// Port is the fixed "shared URL" port every server competes for, so there is one
// address worth bookmarking even though each server also listens on an ephemeral
// port of its own. Zero means the built-in default; a negative value turns the
// shared URL off, leaving only the ephemeral port. The ENOLA_DASHBOARD_PORT
// environment variable overrides it.
type DashboardConfig struct {
	Port int `yaml:"port"`
}

// HistoryConfig controls the architecture history — the append-only record of the
// revisions a repository has passed through.
//
// Recording is ON by default, which is a deliberate reversal of how a feature like this
// usually ships. The questions a history exists to answer — when did this coupling
// appear, which revision introduced this cycle — are questions about the PAST, so
// opt-in guarantees that the first time anybody wants one, there is nothing to read.
// Reconstructing it afterwards means re-snapshotting the repository commit by commit,
// which is by far the most expensive thing in this feature. The data has to already be
// there.
//
// What makes that affordable is that a revision is a ~450-byte line, the working-revision
// ring bounds what an agent loop can produce, and the default location is outside the
// repository — so the cost of being wrong about this default is a few megabytes in a
// directory enola already owns, not a dirty git status or a surprise in someone's tree.
//
// It stays honest about enola's central rule (docs/SNAPSHOTS.md: everything enola writes
// is derivable, and nothing that judges the present may read an accumulated file) because
// that rule is enforced by tests rather than by leaving the feature switched off —
// TestVerdictPathsDoNotReadTheHistory and
// TestHistory_DeletingItChangesNothingAboutThePresent.
type HistoryConfig struct {
	// Enabled turns recording on. Nil (absent) means on; set it to false to stop
	// recording without removing what is already there.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Dir overrides where the history is kept. Empty means outside the repository, under
	// ~/.enola/graphs/<workspace>/history — see history.Root for why that is the default.
	// A relative path resolves against the repository; set it when the history should
	// travel with the checkout (to commit it, or to publish it from CI).
	Dir string `yaml:"dir,omitempty"`

	SharedDir string `yaml:"shared_dir,omitempty"`

	// WorkingKeep caps how many unanchored revisions (dirty tree, or no git at all) are
	// kept per base commit. Zero means the built-in default; negative keeps every one.
	WorkingKeep int `yaml:"working_keep,omitempty"`

	// RevisionKeep caps how many revisions the log keeps in total, committed ones
	// included — appending past it drops the oldest. Zero means the built-in default
	// (200); negative keeps every one.
	RevisionKeep int `yaml:"revision_keep,omitempty"`

	// Blobs stores each revision's facts and findings, not just the line describing what
	// changed — the difference between a timeline you can read and one you can replay
	// (`enola show`, `enola diff A..B`). Nil (absent) means on.
	//
	// It is a SEPARATE switch from Enabled because the two cost different orders of
	// magnitude: a header is ~600 bytes and kept forever, contents are ~4 KB a revision
	// plus a ~128 KB base per segment and bounded by BlobKeep. One flag governing both
	// would silently change meaning by two orders of magnitude the moment blobs shipped.
	Blobs *bool `yaml:"blobs,omitempty"`

	// BlobKeep is roughly how many recent revisions keep their stored contents. Older ones
	// keep their header and report themselves as replayable-by-re-snapshotting. Zero means
	// the built-in default; negative keeps every one.
	BlobKeep int `yaml:"blob_keep,omitempty"`
}

// HistoryEnabled reports whether snapshots are recorded as history (the default).
func (c *Config) HistoryEnabled() bool {
	return c.History.Enabled == nil || *c.History.Enabled
}

// HistoryBlobsEnabled reports whether each revision's contents are stored (the default).
// Always false when recording is off — there is nothing to attach contents to.
func (c *Config) HistoryBlobsEnabled() bool {
	return c.HistoryEnabled() && (c.History.Blobs == nil || *c.History.Blobs)
}

// OutputConfig controls where and how output artifacts are generated.
type OutputConfig struct {
	Dir              string `yaml:"dir"`
	MaxContextTokens int    `yaml:"max_context_tokens"`
}

// KnownExplainers is every explainer this build ships, in the order Default runs
// them. It is deliberately the ONLY list of explainer names in the tree that
// anything reads: the default config below, and `check --fail-on`'s validation and
// help text, all derive from it.
//
// They were separate lists before, and the drift was not hypothetical — four
// explainers (constraints, domain, query-loops, entry-points) shipped enforceable
// while --fail-on's help named eleven of the fifteen, so the one a caller most
// wanted to gate on looked unsupported. A name added here becomes gateable,
// documented and validated in the same edit, or it does not exist.
var KnownExplainers = []string{
	"cycles", "layers", "crossrepo", "coverage", "unused-routes", "messaging-coverage",
	"god-class", "hotspots", "dependency-depth", "exported-surface", "complexity-outliers",
	"intent", "constraints", "domain", "query-loops", "entry-points", "dead-methods",
	"vendored-candidates", "import-closure", "package-metrics", "dead-code",
	"performance",
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Repo: ".",
		Ignore: []string{
			// Any-depth forms, deliberately: a monorepo's sub-app carries its own
			// node_modules/dist, and the root-anchored globs these replace let a
			// nested tree straight into the graph — on one production monolith the
			// sub-app's node_modules alone contributed ~880k facts, dwarfing the
			// repository's own ~150k. CI clones never install dependencies, which
			// is why the corpus runs never caught it; live working trees do.
			"**/vendor/**",
			"**/node_modules/**",
			"**/.git/**",
			"**/dist/**",
			"**/build/**",
			"**/tmp/**",
			"**/public/assets/**",
			// Go reserves testdata/ for fixtures — the toolchain never compiles it.
			// Fixture repos are whole miniature codebases (clients, servers, routes),
			// so indexing them injects their call sites into the HOST repo's graph:
			// enola's own testdata/repos/** supplied all 9 of its detected outbound
			// HTTP call sites, manufacturing a coverage gap for a service that makes
			// no outbound calls at all. Any-depth form, and safe for fixture-driven
			// tests: those root each scan INSIDE the fixture (testdata/repos/<f>/<sub>),
			// so the scanned-relative paths contain no testdata/ segment to match.
			// Keep in sync with the bundled mcp-arch.yaml ignore list.
			"**/testdata/**",
			"**/*_test.go",
			"**/*.test.ts",
			"**/*.test.tsx",
			"**/*.spec.ts",
			"**/*.spec.tsx",
			// Ember's test convention is a HYPHENATED suffix under tests/ —
			// ember-cli generates and qunit discovers tests/**/*-test.{js,ts,gjs,gts}.
			// The directory is demanded for the same reason Ruby's is below: a bare
			// "**/*-test.ts" also swallows production code that merely ends in the
			// token (an experimentation util named ab-test.ts), deleting it from the
			// graph. Inside tests/ the name is tool-reserved and cannot collide.
			"**/tests/**/*-test.js",
			"**/tests/**/*-test.ts",
			"**/tests/**/*-test.gjs",
			"**/tests/**/*-test.gts",
			// Ruby, unlike Go and TS, has no co-located test convention: RSpec
			// requires spec/, Minitest defaults to test/. Demand the directory as
			// well as the filename — a bare "**/*_test.rb" also swallows production
			// code that merely ends in the token (a job named cache_warmup_ab_test.rb),
			// deleting it from the graph.
			// Keep in sync with TestGlobs below: a file that stops being a test must
			// stop being ignored, or it is dropped without being recovered.
			"**/spec/**/*_spec.rb",
			"**/test/**/*_test.rb",
			// Python. Test files here are not merely noise to be filtered later: a
			// pytest fixture that assembles a throwaway app
			// (app.include_router(get_cognify_router(), prefix="/cognify")) is a
			// route-mount fact, and the repo-wide prefix fixpoint (v133) folds every
			// mount it can see. A test-only prefix therefore REWRITES production
			// routes — a real corpus gained six phantom endpoints its service never
			// served, which then matched a client call and mis-attributed a
			// cross-repo edge's evidence. Python test files were also ~35% of that
			// repo's total facts.
			//
			// conftest.py is reserved by pytest outright, and the test_ prefix is its
			// discovery convention (a production module so named would itself be
			// collected as a test), so both are safe at any depth. A whole tests/ tree
			// goes too, since fixtures and factories carry no test_ prefix.
			// Deliberately NOT a bare "**/*_test.py": that repeats the Ruby hazard
			// above of swallowing production code that merely ends in the token, and
			// inside a tests/ tree it is already covered.
			// Keep in sync with TestGlobs below: a file that stops being a test must
			// stop being ignored, or it is dropped without being recovered.
			"**/conftest.py",
			"**/test_*.py",
			"**/tests/**/*.py",
			"**/test/**/*.py",
			// C# test projects. Directory-scoped, with NO filename pattern, and
			// that is measured rather than cautious: across 40,014 .cs files in the
			// benchmark corpus, `**/*Tests.cs` would have added exactly one file the
			// directory rules miss — a generator that EMITS unit tests — while
			// `**/*Test.cs` would have deleted `XmlQualifiedNameTest` (an XPath
			// node-test type in System.Private.Xml) and the Azure Load Testing
			// tool's `LoadTest/Test.cs` model. That is the Ruby `_test.rb` hazard
			// above in its C# form.
			//
			// `**/*.Tests/**` is not redundant with `**/tests/**`: the dominant .NET
			// solution layout puts a test project in `MyApp.Tests/` BESIDE `MyApp/`
			// rather than under a `tests/` directory, and 303 files in the corpus
			// are reachable only that way.
			// BOTH casings. Glob matching is case-sensitive and .NET names these
			// directories in PascalCase: dotnet/roslyn puts 784 files under Test/ and
			// 73 under Tests/, none of which the lowercase patterns match. They were
			// being indexed as production code.
			"**/tests/**/*.cs",
			"**/test/**/*.cs",
			"**/Tests/**/*.cs",
			"**/Test/**/*.cs",
			"**/*.Tests/**/*.cs",
			// Razor markup in the same test trees. A test project's .razor/.cshtml
			// reference production members exactly as a test .cs does, so indexing
			// them would vouch for symbols no production code uses — the same
			// suppression of genuine dead-code findings the .cs rules avoid.
			// VB.NET test trees, for the same reason as the .cs rules above. Roslyn's
			// analyzer tests are themselves .vb files that embed VB source as XML
			// literals, so indexing them contributes thousands of fixture types
			// (`Class C`, `Interface I`, `Enum E`) that no production code declares.
			"**/tests/**/*.fs",
			"**/test/**/*.fs",
			"**/Tests/**/*.fs",
			"**/Test/**/*.fs",
			"**/*.Tests/**/*.fs",
			"**/tests/**/*.vb",
			"**/test/**/*.vb",
			"**/Tests/**/*.vb",
			"**/Test/**/*.vb",
			"**/*.Tests/**/*.vb",
			"**/tests/**/*.razor",
			"**/test/**/*.razor",
			"**/Tests/**/*.razor",
			"**/Test/**/*.razor",
			"**/*.Tests/**/*.razor",
			"**/tests/**/*.cshtml",
			"**/test/**/*.cshtml",
			"**/Tests/**/*.cshtml",
			"**/Test/**/*.cshtml",
			"**/*.Tests/**/*.cshtml",
			"**/tests/**/*.xaml",
			"**/test/**/*.xaml",
			"**/Tests/**/*.xaml",
			"**/Test/**/*.xaml",
			"**/*.Tests/**/*.xaml",
			"**/tests/**/*.axaml",
			"**/test/**/*.axaml",
			"**/Tests/**/*.axaml",
			"**/Test/**/*.axaml",
			"**/*.Tests/**/*.axaml",
			// Scala test source sets. Scoped to the SOURCE SET (src/test, src/it,
			// src/multi-jvm), never to a directory merely NAMED `test`, and with no
			// filename pattern at all. Both restrictions are measured rather than
			// cautious: a one-segment `**/test/**/*.scala` deletes 183 production
			// files across the benchmark corpus — 175 of them zio's own test LIBRARY,
			// which compiles from `test-magnolia/src/main/scala-3/zio/test/` and whose
			// package is literally `zio.test`. That is the Ruby `cache_warmup_ab_test.rb`
			// hazard in its Scala form, and a filename pattern like `**/*Spec.scala`
			// would add a second one. sbt settles it by convention — test sources live
			// in a test source set — so the source set is what is matched, which is
			// also why the prefix needs TWO directory segments (see matchDirScopedGlob).
			// The doubled `src/test` covers every layout in the corpus: `src/test/scala`,
			// the cross-build variants `src/test/scala-3` and `src/test/scala-2.13`,
			// and lila's bare `src/test`.
			// Keep in sync with TestGlobs below: a file that stops being a test must
			// stop being ignored, or it is dropped without being recovered.
			"**/src/test/**/*.scala",
			"**/src/it/**/*.scala",
			"**/src/multi-jvm/**/*.scala",
			// Dart tests. Directory-scoped, following the Ruby and C# precedent: pub's
			// convention puts them under a package's `test/` (and `integration_test/`)
			// directory, and a bare `**/*_test.dart` would additionally swallow
			// production files that merely end in the token. Keep in sync with
			// TestGlobs below.
			"**/test/**/*.dart",
			"**/integration_test/**/*.dart",
			"**/test_driver/**/*.dart",
			// Dart code generation output. This is not tidiness: build_runner output is
			// the MAJORITY of files in a real Flutter project, and none of it is code a
			// human navigates. One @freezed model yields a .freezed.dart of hundreds of
			// generated lines plus a .g.dart of serialization; indexing them inflates
			// symbol counts and manufactures god-class and complexity findings about
			// machine output. Kept in sync with generatedSuffixes in the dart extractor,
			// which applies the same list when the walker hands it a file directly.
			"**/*.g.dart",
			"**/*.freezed.dart",
			"**/*.mocks.dart",
			"**/*.gr.dart",
			"**/*.config.dart",
			"**/*.pb.dart",
			"**/*.pbenum.dart",
			"**/*.pbjson.dart",
			"**/*.pbserver.dart",
			"**/*.pbgrpc.dart",
			// Dart/Flutter build and package caches.
			"**/.dart_tool/**",
			"**/.pub-cache/**",
			"**/.flutter-plugins",
			"**/.flutter-plugins-dependencies",
			// The Dart SDK's own parser fixtures and language suite are programs
			// DESIGNED to be rejected — `trailing_comma_error_test.dart` and its
			// neighbours are deliberately invalid Dart, and front_end/testcases holds
			// thousands of them. They are ordinary source to the walker and would be
			// counted as parse failures against the grammar. Only the dart-lang/sdk
			// repository has these, but the cost of the two globs is nil elsewhere.
			"**/pkg/front_end/testcases/**",
			"**/pkg/_fe_analyzer_shared/test/**",
			// enola's own output. This is the DEFAULT location only; the glob for the
			// configured one is derived in Normalize, which is what makes a custom
			// output.dir safe. The literal stays because a repository that used the
			// default before changing it still has artifacts here, and indexing its own
			// history is exactly the failure the derived glob exists to prevent.
			defaultOutputDir + "/**",
			// And at any depth. A cluster config that snapshots subdirectories —
			// `modules/web` and `modules/api` of one repository — leaves an output
			// directory in each, and only the rooted glob covered them: enola then
			// indexed its own llm_context.md as a source document, so a repository's
			// fact count depended on which of its subdirectories somebody had
			// snapshotted before.
			"**/" + defaultOutputDir + "/**",
			// Build / cache artifacts. These are generated output (often transpiled
			// JS, e.g. Next.js .next/) and must never be indexed as source — doing so
			// pollutes query_facts with thousands of spurious facts. The **/<dir>/**
			// form matches the directory at ANY depth (e.g. Gradle/Android emit
			// data/build/kspCaches/...), not just at the repo root.
			".next/**",
			"dist/**",
			"**/build/**",
			"out/**",
			".vercel/**",
			".turbo/**",
			"coverage/**",
			".nuxt/**",
			".svelte-kit/**",
			"**/__pycache__/**",
			// Python virtual environments, installed dependencies, and tool caches.
			// A repo-local .venv/venv holds the entire dependency tree (thousands of
			// third-party .py files); indexing it is never wanted and dominates
			// snapshot time. site-packages is the definitive catch for any oddly-named
			// env (.tox/.nox/conda/direnv all nest one). Any-depth (**/x/**) form so
			// monorepo sub-project venvs are pruned too.
			"**/.venv/**",
			"**/venv/**",
			"**/site-packages/**",
			"**/.tox/**",
			"**/.nox/**",
			"**/.eggs/**",
			"**/.mypy_cache/**",
			"**/.pytest_cache/**",
			"**/.ruff_cache/**",
			"**/Pods/**",
			"**/.gradle/**",
			"**/target/**",
			// .NET build output, at any depth (one obj/bin pair per project).
			// `obj/` is the one that matters: the compiler writes generated C#
			// there on every build — GlobalUsings.g.cs, AssemblyInfo.cs,
			// source-generator output — which would otherwise be attributed to
			// the project that merely built it. `obj` is not a source directory
			// convention in any other ecosystem, so it is ignored outright.
			// `bin` IS one (a Node package's bin/cli.js, a Rails bin/setup), so
			// only .NET's own configuration subdirectories are excluded rather
			// than every bin/ in every repo.
			"**/obj/**",
			"**/bin/Debug/**",
			"**/bin/Release/**",
			// Minified / bundled JS by name. The extractor also detects minified
			// content heuristically (very long lines), but these globs cheaply skip
			// the common named cases before a file is ever read. Keep in sync with
			// the bundled mcp-arch.yaml ignore list.
			"**/*.min.js",
			"**/*.bundle.js",
		},
		// TestGlobs identify test/spec files. They stay ignored for normal indexing
		// (still listed in Ignore above) — production architecture facts must not
		// include test symbols — but the engine collects them separately for
		// reference-only extraction so the dead-code detector can see that a
		// production symbol is exercised by a test and not mis-report it as dead.
		// A glob here without an extractor implementing plugin.TestRefExtractor is a
		// no-op (engine.runTestRefExtractors skips non-implementers), so extend this
		// list only alongside the matching extractor. Go's and TypeScript's dotted
		// suffixes are correct: the toolchain/convention reserves *_test.go and
		// *.test.ts(x)/*.spec.ts(x) for tests, so — unlike Ruby's _test.rb (v97) — no
		// production file can collide with them.
		TestGlobs: []string{
			"**/*_test.go",
			"**/*.test.ts", "**/*.test.tsx", "**/*.spec.ts", "**/*.spec.tsx",
			"**/tests/**/*-test.js", "**/tests/**/*-test.ts",
			"**/tests/**/*-test.gjs", "**/tests/**/*-test.gts",
			"**/spec/**/*_spec.rb", "**/test/**/*_test.rb",
			// Python has no TestRefExtractor yet, so these four are a deliberate
			// no-op today: runTestRefExtractors skips non-implementers. They are
			// listed anyway because Ignore above requires the two lists to agree —
			// a file ignored here but absent from TestGlobs is dropped with no way
			// to recover it — and so that implementing PythonExtractor.ExtractTestRefs
			// switches the signal on without a second config change.
			// Until then, a Python symbol called only from a test reads as dead:
			// expect dead-code false positives on Python repos to rise.
			"**/conftest.py", "**/test_*.py",
			"**/tests/**/*.py", "**/test/**/*.py",
			// C# has no TestRefExtractor either, so these three are the same
			// deliberate no-op as Python's four: listed because Ignore above
			// requires the two lists to agree, and so that implementing
			// CSharpExtractor.ExtractTestRefs switches the signal on without a
			// second config change. Until then, a C# symbol called only from a test
			// reads as dead — the same trade Python already makes, and the right
			// way round: a missing edge is visible in a dead-code review, while the
			// 31 phantom endpoints csharp-sdk reported before this change were not.
			"**/tests/**/*.cs", "**/test/**/*.cs", "**/Tests/**/*.cs", "**/Test/**/*.cs", "**/*.Tests/**/*.cs",
			"**/tests/**/*.fs", "**/test/**/*.fs", "**/Tests/**/*.fs", "**/Test/**/*.fs", "**/*.Tests/**/*.fs",
			"**/tests/**/*.vb", "**/test/**/*.vb", "**/Tests/**/*.vb", "**/Test/**/*.vb", "**/*.Tests/**/*.vb",
			"**/tests/**/*.razor", "**/test/**/*.razor", "**/Tests/**/*.razor", "**/Test/**/*.razor", "**/*.Tests/**/*.razor",
			"**/tests/**/*.cshtml", "**/test/**/*.cshtml", "**/Tests/**/*.cshtml", "**/Test/**/*.cshtml", "**/*.Tests/**/*.cshtml",
			"**/tests/**/*.xaml", "**/test/**/*.xaml", "**/Tests/**/*.xaml", "**/Test/**/*.xaml", "**/*.Tests/**/*.xaml",
			"**/tests/**/*.axaml", "**/test/**/*.axaml", "**/Tests/**/*.axaml", "**/Test/**/*.axaml", "**/*.Tests/**/*.axaml",
			// Scala has no TestRefExtractor yet, so these three are the same
			// deliberate no-op as Python's and C#'s: listed because Ignore above
			// requires the two lists to agree, and so that implementing
			// ScalaExtractor.ExtractTestRefs switches the signal on without a second
			// config change. Until then, a Scala symbol called only from a spec reads
			// as dead — expect dead-code false positives on Scala repos until it lands.
			"**/src/test/**/*.scala",
			"**/src/it/**/*.scala",
			"**/src/multi-jvm/**/*.scala",
			// Dart. Unlike Scala's above, these are live: DartExtractor implements
			// TestRefExtractor, so a production symbol whose only caller is its test
			// keeps a reference and does not read as dead.
			"**/test/**/*.dart",
			"**/integration_test/**/*.dart",
			"**/test_driver/**/*.dart",
		},
		Extractors: []string{"asyncapi", "cpp", "dart", "dotnet", "go", "grpc", "java", "kotlin", "openapi", "php", "python", "typescript", "swift", "ruby", "rust", "scala", "hcl", "ansible", "mdintent", "manifests"},
		Explainers: append([]string(nil), KnownExplainers...),
		Renderers:  []string{"llm_context"},
		Output: OutputConfig{
			Dir:              defaultOutputDir,
			MaxContextTokens: 16000,
		},
	}
}

// Load reads a configuration file from the given path.
// Missing fields are filled with defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Whether the key was PRESENT cannot be recovered from cfg afterwards: an
	// absent `extractors:` leaves Default()'s list in place, and a list identical
	// to it unmarshals to the same value. A second pass with a pointer field is
	// the only way to tell "inherited the defaults" from "chose exactly these".
	var probe struct {
		Extractors *[]string `yaml:"extractors"`
	}
	if err := yaml.Unmarshal(data, &probe); err == nil {
		cfg.ExtractorsExplicit = probe.Extractors != nil
	}
	// Recorded so Repos entries resolve against the config's own directory; see
	// RepoPaths.
	if abs, err := filepath.Abs(path); err == nil {
		cfg.SourcePath = abs
	} else {
		cfg.SourcePath = path
	}

	for label, decl := range cfg.Intent {
		if decl == nil {
			continue
		}
		decl.Normalize()
		if err := decl.Validate(); err != nil {
			return nil, fmt.Errorf("in config %s, intent entry %q: %w", path, label, err)
		}
		decl.Source = intent.ClusterSource
	}

	if err := cfg.Normalize(); err != nil {
		return nil, fmt.Errorf("in config %s: %w", path, err)
	}
	return cfg, nil
}

// defaultOutputDir is where artifacts go unless output.dir says otherwise. It is
// also present as a literal in Default().Ignore, and deliberately so — see Normalize.
const defaultOutputDir = ".enola"

// Normalize fills in required defaults, validates them, and derives the settings
// that follow from other settings. Idempotent, and called both by Load and by the
// engine, so a config built in code gets the same treatment as one read from a file.
//
// Its real job is the output directory. `.enola/**` used to be a hard-coded literal
// in the default ignore list, sitting between `.next/**` and `dist/**` as though it
// were another build-artifact glob, and agreeing with Output.Dir only by coincidence.
// Point output.dir anywhere else and the next snapshot walked the previous one's
// artifacts — facts.jsonl, insights.json, llm_context.md, plus the previous/ rotation
// from run 2 onward — so an unchanged tree produced a different snapshot every run.
// Reproducibility is the property the baseline diff rests on, and this broke it for a
// reason that has nothing to do with enola's determinism, on a setting users are
// invited to change, with a symptom that points nowhere near the cause.
//
// The literal `.enola/**` stays in Default().Ignore as well: a repository that once
// used the default and later changed it must not start indexing its own history.
func (c *Config) Normalize() error {
	if c.Output.Dir == "" {
		c.Output.Dir = defaultOutputDir
	}
	if c.Output.MaxContextTokens == 0 {
		c.Output.MaxContextTokens = 16000
	}

	dir, err := cleanOutputDir(c.Output.Dir)
	if err != nil {
		return err
	}
	c.Output.Dir = dir

	if err := providers.Validate(c.Providers); err != nil {
		return err
	}

	clientspec.Normalize(c.Clients)
	if err := clientspec.Validate(c.Clients); err != nil {
		return err
	}
	if err := clientspec.ValidateAliases(c.ServiceAliases); err != nil {
		return err
	}

	// Both anchorings. The rooted glob covers this repository's own output; the
	// nested one covers a snapshot somebody took of a SUBDIRECTORY — a cluster
	// config pointing at `modules/web` and `modules/api` leaves an `.enola` in each,
	// and only the root one was ignored. The artifacts then index as source: one
	// corpus repository carried 14 markdown facts for the sections of a nested
	// `llm_context.md`, and how many it carried depended on which subdirectories
	// somebody had snapshotted, which is the same reproducibility failure the rooted
	// glob exists to prevent, one directory down.
	for _, glob := range []string{dir + "/**", "**/" + dir + "/**"} {
		if !contains(c.Ignore, glob) {
			c.Ignore = append(c.Ignore, glob)
		}
	}
	return nil
}

// cleanOutputDir validates output.dir and returns it in slash form.
//
// It must be a subdirectory of the repository, because that is the only thing the
// rest of the code can express: Output.Dir is JOINED to the repository path in half a
// dozen places, so an absolute path silently produced a directory nested inside the
// repo (/repo/private/tmp/.../out) rather than the location asked for, and an ignore
// glob derived from it could not describe the artifacts either. An error naming the
// constraint beats a path that looks accepted and means something else.
func cleanOutputDir(dir string) (string, error) {
	if filepath.IsAbs(dir) {
		return "", fmt.Errorf("output.dir %q must be a path inside the repository, not an absolute one "+
			"(it is joined to the repository path, so an absolute value would nest the whole path inside the repo)", dir)
	}
	clean := filepath.ToSlash(filepath.Clean(dir))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("output.dir %q must name a subdirectory of the repository", dir)
	}
	return clean, nil
}

// RepoPaths returns the absolute repository paths this run covers, in the order
// they should be indexed: the first fresh, the rest appended.
//
// This is the single definition of "which repositories does this config describe",
// so the CLI, --explain and any wrapper agree. Two resolution rules, and the
// difference is deliberate:
//
//   - Repos entries resolve against the config file's own directory, because a
//     cluster config is meant to be checked in and to mean the same thing wherever
//     it is run from. With no config file (built-in defaults) there is no such
//     directory, so they fall back to the working directory.
//   - Repo resolves against the working directory, unchanged, because that is what
//     `repo: "."` has always meant.
//
// Returns exactly one path when Repos is empty, so a single-repo caller needs no
// special case.
func (c *Config) RepoPaths() ([]string, error) {
	if len(c.Repos) == 0 {
		abs, err := filepath.Abs(c.Repo)
		if err != nil {
			return nil, fmt.Errorf("resolving repo %q: %w", c.Repo, err)
		}
		return []string{abs}, nil
	}

	base := ""
	if c.SourcePath != "" {
		base = filepath.Dir(c.SourcePath)
	}
	out := make([]string, 0, len(c.Repos))
	seen := map[string]bool{}
	for _, r := range c.Repos {
		// Trimmed before the emptiness check: a whitespace-only entry is a YAML
		// slip, and filepath.Abs would happily turn it into the working directory.
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		p := r
		if !filepath.IsAbs(p) && base != "" {
			p = filepath.Join(base, p)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("resolving repos entry %q: %w", r, err)
		}
		// A repository listed twice would be indexed twice, the second pass
		// appending a duplicate of every fact it already contributed.
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("repos is set but contains no usable paths")
	}
	return out, nil
}

// IsExtractorEnabled returns true if the named extractor is enabled.
// extractorAliases maps an extractor's current name to names it also answers to.
//
// `csharp` became `dotnet` when the extractor grew past C# into VB.NET, F#, Razor
// and XAML. The alias is not politeness: an `extractors:` list REPLACES the
// built-in default rather than merging with it, so a config written under the old
// name would silently disable .NET extraction entirely and report zero facts with
// no error — the exact failure this file's own comments warn about.
var extractorAliases = map[string][]string{
	"dotnet": {"csharp"},
}

func (c *Config) IsExtractorEnabled(name string) bool {
	if contains(c.Extractors, name) {
		return true
	}
	for _, alias := range extractorAliases[name] {
		if contains(c.Extractors, alias) {
			return true
		}
	}
	return false
}

// IsExplainerEnabled returns true if the named explainer is enabled.
func (c *Config) IsExplainerEnabled(name string) bool {
	return contains(c.Explainers, name)
}

// IsRendererEnabled returns true if the named renderer is enabled.
func (c *Config) IsRendererEnabled(name string) bool {
	return contains(c.Renderers, name)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// LinkingVocab resolves the effective cross-repo linking vocabulary: the built-in
// defaults with any `linking:` overlay applied, carrying the declared service_aliases so
// every matcher built from it resolves a service name the same way. It returns an error
// for an invalid threshold rather than clamping — see vocab.Apply.
func (c *Config) LinkingVocab() (*vocab.Set, error) {
	v, err := vocab.Apply(c.Linking)
	if v != nil && len(c.ServiceAliases) > 0 {
		v.ServiceAliases = c.ServiceAliases
	}
	return v, err
}

package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/explainers/coverage"
	"github.com/enola-labs/enola/internal/facts"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	importsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/imports"
	kafkasignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/kafka"
	sharedcodesignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/sharedcode"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

func TestIsIgnored(t *testing.T) {
	tests := []struct {
		name     string
		relPath  string
		isDir    bool
		patterns []string
		want     bool
	}{
		{
			"vendor directory",
			"vendor/foo/bar.go", false,
			[]string{"vendor/**"},
			true,
		},
		{
			"vendor dir itself",
			"vendor", true,
			[]string{"vendor/**"},
			true,
		},
		{
			"node_modules",
			"node_modules/react/index.js", false,
			[]string{"node_modules/**"},
			true,
		},
		{
			// The reported bug: a repo-local .venv held the whole dependency tree
			// and was indexed. The **/x/** form must prune the directory itself so
			// the walk never descends into it.
			"venv dir itself pruned at any depth",
			"backend/.venv", true,
			[]string{"**/.venv/**"},
			true,
		},
		{
			"site-packages under a nested venv",
			"backend/.venv/lib/python3.12/site-packages/pandas/core/frame.py", false,
			[]string{"**/.venv/**", "**/site-packages/**"},
			true,
		},
		{
			"plain venv dir",
			"venv/lib/python3.11/site-packages/x.py", false,
			[]string{"**/venv/**"},
			true,
		},
		{
			// "env" alone must NOT be treated as a venv (too common a name).
			"env is not ignored",
			"app/env/settings.py", false,
			[]string{"**/.venv/**", "**/venv/**", "**/site-packages/**"},
			false,
		},
		{
			"git directory",
			".git/HEAD", false,
			[]string{".git/**"},
			true,
		},
		{
			"test files with ** prefix",
			"src/main_test.go", false,
			[]string{"**/*_test.go"},
			true,
		},
		{
			"non-test file not ignored",
			"src/main.go", false,
			[]string{"**/*_test.go"},
			false,
		},
		{
			"spec files",
			"src/utils.spec.ts", false,
			[]string{"**/*.spec.ts"},
			true,
		},
		{
			"enola output dir",
			".enola/facts.jsonl", false,
			[]string{".enola/**"},
			true,
		},
		{
			"normal source not ignored",
			"src/app.go", false,
			[]string{"vendor/**"},
			false,
		},
		{
			"build directory",
			"build/output.kt", false,
			[]string{"build/**"},
			true,
		},
		{
			"nested test file",
			"internal/pkg/foo_test.go", false,
			[]string{"**/*_test.go"},
			true,
		},
		{
			"deeply nested vendor",
			"vendor/github.com/foo/bar/baz.go", false,
			[]string{"vendor/**"},
			true,
		},
		{
			"nested build dir via **/build/**",
			"data/build/kspCaches/devDebug/Gen.kt", false,
			[]string{"**/build/**"},
			true,
		},
		{
			"top-level build dir via **/build/**",
			"build/output.kt", false,
			[]string{"**/build/**"},
			true,
		},
		{
			"build as filename is not a build dir",
			"cmd/build.go", false,
			[]string{"**/build/**"},
			false,
		},
		{
			"rebuild is a different segment",
			"rebuild/main.go", false,
			[]string{"**/build/**"},
			false,
		},
		{
			"cocoapods at any depth",
			"Pods/Alamofire/Source/Request.swift", false,
			[]string{"**/Pods/**"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Ignore = tt.patterns

			eng, _ := New(cfg)
			got := eng.isIgnored(tt.relPath, tt.isDir)
			if got != tt.want {
				t.Errorf("isIgnored(%q, isDir=%v) with patterns %v = %v, want %v",
					tt.relPath, tt.isDir, tt.patterns, got, tt.want)
			}
		})
	}
}

// TestMatchAnyGlob_MidPatternDoublestar covers the "<prefix>/**/<fileglob>" form,
// which lets a pattern require BOTH a directory segment and a filename shape.
//
// The Ruby test globs need it. A bare "**/*_test.rb" is a filename-suffix match, so
// a production ActiveJob named cache_warmup_ab_test.rb was ignored AND routed to
// reference-only test-ref extraction — its class vanished from the graph. No
// filename-only rule can separate that file from lib/foo_test.rb: both end in the
// token "test". The directory segment is the only reliable signal, and Ruby supplies
// one (RSpec requires spec/, Minitest defaults to test/).
func TestMatchAnyGlob_MidPatternDoublestar(t *testing.T) {
	rubyTestGlobs := []string{"**/spec/**/*_spec.rb", "**/test/**/*_test.rb"}

	tests := []struct {
		name     string
		relPath  string
		patterns []string
		want     bool
	}{
		{
			// The reported bug: a production A/B-test job under app/jobs.
			"production job whose name ends in _ab_test",
			"app/jobs/reporting/cache_warmup_ab_test.rb",
			rubyTestGlobs,
			false,
		},
		{
			"production model named ab_test",
			"app/models/ab_test.rb",
			rubyTestGlobs,
			false,
		},
		{
			// Zero intermediate directories. filepath.Match's "*" never crosses a
			// separator, so the pre-existing "**/<glob>" branch could not match this.
			"spec directly under spec/",
			"spec/user_spec.rb",
			rubyTestGlobs,
			true,
		},
		{
			"spec one level down",
			"spec/services/report_worker_spec.rb",
			rubyTestGlobs,
			true,
		},
		{
			"spec segment at any depth, several levels down",
			"engines/billing/spec/models/nested/invoice_spec.rb",
			rubyTestGlobs,
			true,
		},
		{
			"minitest file under test/",
			"test/models/user_test.rb",
			rubyTestGlobs,
			true,
		},
		{
			// The dir segment is present but the basename shape is wrong.
			"support file under spec/ is not a spec",
			"spec/rails_helper.rb",
			rubyTestGlobs,
			false,
		},
		{
			// "spec" must be a DIRECTORY segment, not the basename stem.
			"file named spec.rb outside a spec dir",
			"app/models/spec.rb",
			rubyTestGlobs,
			false,
		},
		{
			"anchored prefix form",
			"spec/models/user_spec.rb",
			[]string{"spec/**/*_spec.rb"},
			true,
		},
		{
			"anchored prefix form does not match a nested spec dir",
			"engines/billing/spec/models/user_spec.rb",
			[]string{"spec/**/*_spec.rb"},
			false,
		},
		// The pre-existing pattern forms must keep their semantics — the new branch
		// fires only on a literal "/**/" in the pattern, which none of them contain.
		{
			"**/build/** still matches a nested build dir",
			"data/build/kspCaches/devDebug/Gen.kt",
			[]string{"**/build/**"},
			true,
		},
		{
			"**/*_test.go still matches by filename at any depth",
			"internal/pkg/foo_test.go",
			[]string{"**/*_test.go"},
			true,
		},
		{
			"vendor/** still matches an anchored prefix",
			"vendor/github.com/foo/bar.go",
			[]string{"vendor/**"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchAnyGlob(tt.relPath, tt.patterns); got != tt.want {
				t.Errorf("matchAnyGlob(%q, %v) = %v, want %v",
					tt.relPath, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestResolveFactFile_SingleRepo(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)
	eng.SetSnapshot(&facts.Snapshot{
		Meta: facts.SnapshotMeta{RepoPath: "/Users/me/myrepo"},
	})

	f := &facts.Fact{File: "internal/server/server.go"}
	got := eng.ResolveFactFile(f)
	want := filepath.Join("/Users/me/myrepo", "internal/server/server.go")
	if got != want {
		t.Errorf("ResolveFactFile = %q, want %q", got, want)
	}
}

func TestResolveFactFile_MultiRepo(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)
	eng.SetRepoPaths(map[string]string{
		"go-service":    "/Users/me/development/go-service",
		"ruby-monolith": "/Users/me/development/ruby-monolith",
	})
	eng.SetSnapshot(&facts.Snapshot{
		Meta: facts.SnapshotMeta{RepoPath: "/Users/me/workspace"},
	})

	tests := []struct {
		name string
		fact facts.Fact
		want string
	}{
		{
			"multi-repo go-service",
			facts.Fact{File: "go-service/lib/foo.rb", Repo: "go-service", RepoPrefixed: true},
			filepath.Join("/Users/me/development/go-service", "lib/foo.rb"),
		},
		{
			"multi-repo ruby-monolith",
			facts.Fact{File: "ruby-monolith/lib/bar.rb", Repo: "ruby-monolith", RepoPrefixed: true},
			filepath.Join("/Users/me/development/ruby-monolith", "lib/bar.rb"),
		},
		{
			"a directory named like the repository is part of the path",
			facts.Fact{File: "go-service/lib/foo.rb", Repo: "go-service"},
			filepath.Join("/Users/me/development/go-service", "go-service/lib/foo.rb"),
		},
		{
			"no repo label falls back to snapshot",
			facts.Fact{File: "internal/server.go"},
			filepath.Join("/Users/me/workspace", "internal/server.go"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eng.ResolveFactFile(&tt.fact)
			if got != tt.want {
				t.Errorf("ResolveFactFile = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLinkCrossRepo_ConnectsServicesInGraph verifies that the cross-repo linking
// step produces service nodes and dependency edges that make the graph traversable
// across repos, and that re-running it does not duplicate facts.
func TestLinkCrossRepo_ConnectsServicesInGraph(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)
	registerOSSSignals(eng)

	// Two repos: svc-alpha calls an endpoint svc-beta serves.
	eng.Store().Add(
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "svc-alpha",
			Props: map[string]any{"method": "GET", "role": "client"},
		},
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "svc-beta",
			Props: map[string]any{"method": "GET", "role": "server"},
		},
	)

	eng.linkCrossRepo(nil)

	if got := eng.Store().ByKind(facts.KindService); len(got) != 2 {
		t.Fatalf("service nodes = %d, want 2", len(got))
	}
	depFacts, _ := eng.Store().QueryAdvanced(facts.QueryOpts{Prop: "type", PropValue: "cross_repo"})
	if len(depFacts) != 1 {
		t.Fatalf("cross_repo dep facts = %d, want 1", len(depFacts))
	}
	if depFacts[0].Repo != "svc-alpha" || depFacts[0].Name != "svc-alpha -> svc-beta" {
		t.Errorf("edge = %+v, want svc-alpha -> svc-beta", depFacts[0])
	}

	// The graph now connects the two service nodes.
	eng.Store().BuildGraph()
	g := eng.Store().Graph()
	res := g.Traverse("svc-alpha", "forward", nil, nil, 5, 100)
	reached := false
	for _, n := range res.Nodes {
		if n.Name == "svc-beta" {
			reached = true
		}
	}
	if !reached {
		t.Errorf("traverse from svc-alpha did not reach svc-beta: %+v", res.Nodes)
	}
	if path := g.FindPath("svc-alpha", "svc-beta", nil, 10); !path.Found {
		t.Errorf("FindPath(svc-alpha, svc-beta) not found")
	}

	// Idempotent: re-running linking keeps exactly one service node per repo
	// and one edge (no duplicates).
	eng.linkCrossRepo(nil)
	if got := eng.Store().ByKind(facts.KindService); len(got) != 2 {
		t.Errorf("after relink, service nodes = %d, want 2", len(got))
	}
	deps2, _ := eng.Store().QueryAdvanced(facts.QueryOpts{Prop: "type", PropValue: "cross_repo"})
	if len(deps2) != 1 {
		t.Errorf("after relink, cross_repo dep facts = %d, want 1", len(deps2))
	}
}

// TestLinkCrossRepo_CoverageGapInsight exercises the full coverage path through
// the engine: a service whose only outbound call site cannot be resolved must get
// edge_coverage props on its service node and a "Coverage gap" insight from the
// coverage explainer — so it reads as a blind spot, not a true isolate.
func TestLinkCrossRepo_CoverageGapInsight(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)
	registerOSSSignals(eng)

	// svc-alpha calls a path no loaded repo serves; svc-beta serves something else.
	eng.Store().Add(
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/orders/{id}", Repo: "svc-alpha",
			Props: map[string]any{"method": "GET", "role": "client"},
		},
		facts.Fact{
			Kind: facts.KindRoute, Name: "/api/items/{id}", Repo: "svc-beta",
			Props: map[string]any{"method": "GET", "role": "server"},
		},
	)
	eng.linkCrossRepo(nil)

	// The svc-alpha service node carries edge_coverage with an unresolved call site.
	var alpha *facts.Fact
	for _, f := range eng.Store().ByKind(facts.KindService) {
		if f.Name == "svc-alpha" {
			ff := f
			alpha = &ff
		}
	}
	if alpha == nil {
		t.Fatal("no svc-alpha service node")
	}
	if _, ok := alpha.Props["edge_coverage"]; !ok {
		t.Errorf("svc-alpha service node missing edge_coverage prop: %+v", alpha.Props)
	}

	// The coverage explainer turns that into a "Coverage gap" insight.
	insights, err := coverage.New().Explain(context.Background(), eng.Store())
	if err != nil {
		t.Fatalf("coverage Explain: %v", err)
	}
	found := false
	for _, in := range insights {
		if in.Title == "Coverage gap: service svc-alpha appears isolated but has 1 unresolved outbound call site(s)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Coverage gap insight for svc-alpha, got %+v", insights)
	}
}

// TestGenerateSnapshot_ConcurrentCallsSerialized verifies that the engine mutex
// prevents concurrent GenerateSnapshot calls from corrupting shared state.
func TestGenerateSnapshot_ConcurrentCallsSerialized(t *testing.T) {
	cfg := config.Default()
	eng, _ := New(cfg)

	// Use a non-existent repo path — GenerateSnapshot will fail at walkRepo,
	// but that's fine: we're testing that concurrent calls don't panic or
	// produce a data race. The mutex should serialize them.
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = eng.GenerateSnapshot(context.Background(), t.TempDir(), false)
		}(i)
	}
	wg.Wait()

	// All calls should complete without panic. Errors from missing extractors
	// or empty repos are expected — the key thing is no race.
	for i, err := range errs {
		if err != nil {
			t.Logf("goroutine %d error (expected): %v", i, err)
		}
	}
}

// TestSourceReaderFor_ResolvesRepoPrefixedPaths covers the timing trap in
// linkCrossRepo: it runs mid-snapshot, before the new bundle is published, so the
// reader must resolve paths from the in-flight map rather than ResolveFactFile. Facts
// carry repo-prefixed paths in append mode, which the reader has to strip.
func TestSourceReaderFor_ResolvesRepoPrefixedPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "class Widget\nend\n"
	if err := os.WriteFile(filepath.Join(root, "app", "widget.rb"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	read := sourceReaderFor(map[string]string{"svc": root})
	if read == nil {
		t.Fatal("expected a reader for a non-empty repo path map")
	}

	got, ok := read(facts.Fact{Repo: "svc", RepoPrefixed: true, File: "svc/app/widget.rb"})
	if !ok || got != want {
		t.Errorf("read(repo-prefixed) = %q, %v; want %q, true", got, ok, want)
	}
	if _, ok := read(facts.Fact{Repo: "svc", RepoPrefixed: true, File: "svc/app/missing.rb"}); ok {
		t.Error("missing file should report not-ok, not an empty success")
	}
	if _, ok := read(facts.Fact{Repo: "unknown", File: "unknown/app/widget.rb"}); ok {
		t.Error("unknown repo label should report not-ok")
	}
}

// TestSourceReaderFor_NilWithoutRepoPaths: no known paths means verification is off,
// not that every comparison silently sees empty files.
func TestSourceReaderFor_NilWithoutRepoPaths(t *testing.T) {
	if sourceReaderFor(nil) != nil {
		t.Error("nil repo paths should yield a nil reader (verification disabled)")
	}
	if sourceReaderFor(map[string]string{}) != nil {
		t.Error("empty repo paths should yield a nil reader (verification disabled)")
	}
}

// registerOSSSignals wires the production cross-repo signal set onto a bare engine.
//
// An engine created with New() has no signals registered, exactly as it has no
// extractors: linking is plugin-driven, so a test that exercises it must register them
// the way bootstrap does. Without this, cross-repo linking silently produces service
// nodes and no edges — which is the correct behavior for an engine with no signals, and
// a confusing test failure if you expected the old hardcoded set.
func registerOSSSignals(e *Engine) {
	e.RegisterCrossRepoSignal(httpsignal.New(vocab.Default()))
	e.RegisterCrossRepoSignal(importsignal.New())
	e.RegisterCrossRepoSignal(kafkasignal.New(vocab.Default()))
	e.RegisterCrossRepoSignal(sharedcodesignal.New(vocab.Default()))
}

// TestMatchAnyGlob_GlobbedDirectorySegment covers the .NET solution layout, where
// a test project sits in `MyApp.Tests/` BESIDE `MyApp/` rather than under a
// `tests/` directory — so no literal directory segment names it, and the segment
// itself has to be a glob.
//
// The literal cases are asserted alongside because generalising the comparison
// must not widen them: `**/build/**` still has to mean a directory called exactly
// "build".
func TestMatchAnyGlob_GlobbedDirectorySegment(t *testing.T) {
	csharpTestGlobs := []string{"**/tests/**/*.cs", "**/test/**/*.cs", "**/*.Tests/**/*.cs"}

	tests := []struct {
		name     string
		relPath  string
		patterns []string
		want     bool
	}{
		{
			"test project beside its app, the dominant .NET layout",
			"src/MyApp.Tests/OrderServiceTests.cs",
			csharpTestGlobs,
			true,
		},
		{
			"glob-named directory at the repo root",
			"PlatformDocAnalyzer.Tests/AnalyzerTests.cs",
			csharpTestGlobs,
			true,
		},
		{
			"a test directory still matches",
			"src/libraries/System.Text.Json/tests/JsonTests.cs",
			csharpTestGlobs,
			true,
		},
		{
			// The Ruby _test.rb hazard in its C# form. XmlQualifiedNameTest is a
			// real XPath node-test type in System.Private.Xml, and Test.cs is a
			// production model in the Azure Load Testing tool — which is why the
			// C# globs are directory-scoped and carry no filename patterns.
			"production type whose name ends in Test",
			"src/System.Private.Xml/src/System/Xml/Xsl/XmlQualifiedNameTest.cs",
			csharpTestGlobs,
			false,
		},
		{
			"production directory that merely contains the word",
			"src/Contest/Entry.cs",
			csharpTestGlobs,
			false,
		},
		{
			// Generalising the segment comparison must not widen a literal pattern.
			"literal segment does not match a longer name",
			"src/buildtools/Gen.cs",
			[]string{"**/build/**"},
			false,
		},
		{
			"literal segment still matches exactly",
			"src/build/Gen.cs",
			[]string{"**/build/**"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchAnyGlob(tt.relPath, tt.patterns); got != tt.want {
				t.Errorf("matchAnyGlob(%q) = %v, want %v", tt.relPath, got, tt.want)
			}
		})
	}
}

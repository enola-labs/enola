package config

import (
	"testing"

	"github.com/enola-labs/enola/internal/pathglob"
)

// TestScalaTestGlobsScopeToSourceSet pins the one decision that separates a correct
// Scala test glob from a destructive one: the pattern matches a test SOURCE SET, not
// a directory named `test`.
//
// Every path below is real, taken from the benchmark corpus. The production cases
// are the point — a one-segment `**/test/**/*.scala` matches all of them, and would
// silently delete 183 files, 175 of them zio's own test library.
func TestScalaTestGlobsScopeToSourceSet(t *testing.T) {
	cfg := Default()

	cases := []struct {
		path   string
		isTest bool
		why    string
	}{
		// --- test source sets, which must be excluded ---
		{"core/src/test/scala/org/apache/spark/FooSuite.scala", true, "spark: sbt convention"},
		{"modules/study/src/test/NewTreeCheck.scala", true, "lila: bare src/test, no language dir"},
		{"core/src/test/scala-3/zio/ZIOSpec.scala", true, "zio: cross-build variant dir"},
		{"http/src/test/scala-2.13/Foo.scala", true, "pekko-http: versioned variant dir"},
		{"cluster/src/multi-jvm/scala/MultiNodeSpec.scala", true, "pekko: multi-jvm source set"},
		{"server/src/it/scala/IntegrationSpec.scala", true, "sbt IntegrationTest source set"},

		// --- production code a one-segment `test` glob would have eaten ---
		{"test-magnolia/src/main/scala-3/zio/test/magnolia/DeriveGen.scala", false,
			"zio: the test LIBRARY is production code, package zio.test"},
		{"core/shared/src/main/scala/zio/test/Assertion.scala", false,
			"zio: a `test` package segment under src/main is production"},
		{"sql/core/src/main/scala/org/apache/spark/sql/test/SQLTestUtils.scala", false,
			"spark: a test-UTILITY shipped from src/main is production"},
		{"src/main/scala/RouteSpec.scala", false,
			"a production type merely named *Spec — why there is no filename pattern"},
		{"modules/core/src/main/scala/trading/core/http/HealthRoutes.scala", false,
			"ordinary production source"},
	}

	for _, tc := range cases {
		gotIgnored := pathglob.MatchAny(tc.path, cfg.Ignore)
		gotTest := pathglob.MatchAny(tc.path, cfg.TestGlobs)

		if gotIgnored != tc.isTest {
			t.Errorf("Ignore: %s\n  got ignored=%v, want %v (%s)", tc.path, gotIgnored, tc.isTest, tc.why)
		}
		// The two lists must agree exactly. A file ignored but absent from
		// TestGlobs is dropped with no way to recover it — the failure the
		// "keep in sync" comments in Default() exist to prevent.
		if gotTest != gotIgnored {
			t.Errorf("Ignore/TestGlobs disagree on %s: ignored=%v test=%v (%s)",
				tc.path, gotIgnored, gotTest, tc.why)
		}
	}
}

// TestMultiSegmentGlobPrefixIsConsecutive pins the matcher semantics the Scala globs
// rely on: `**/src/test/**` means the two segments ADJACENT, at any depth — not a
// `src` somewhere above a `test`. Without adjacency, zio's
// `core/shared/src/main/scala/zio/test/Assertion.scala` matches (it has a `src`, and
// a `test` below it) and the production/test distinction collapses.
func TestMultiSegmentGlobPrefixIsConsecutive(t *testing.T) {
	pattern := []string{"**/src/test/**/*.scala"}

	adjacent := []string{
		"src/test/Foo.scala",
		"a/b/src/test/Foo.scala",
		"a/src/test/scala/deep/Foo.scala",
	}
	for _, p := range adjacent {
		if !pathglob.MatchAny(p, pattern) {
			t.Errorf("%s should match %v (src/test adjacent)", p, pattern)
		}
	}

	separated := []string{
		"src/main/scala/zio/test/Assertion.scala", // src ... test, but not adjacent
		"src/main/test/Foo.scala",                 // ditto, one level apart
		"test/src/Foo.scala",                      // reversed order
		"src/testing/Foo.scala",                   // segment is not `test`
	}
	for _, p := range separated {
		if pathglob.MatchAny(p, pattern) {
			t.Errorf("%s must NOT match %v (src/test not adjacent)", p, pattern)
		}
	}
}

// TestExistingSingleSegmentGlobsUnchanged guards the generalization: extending the
// matcher to multi-segment prefixes must leave every pattern that predates it
// behaving exactly as before, including the two hazards earlier languages already
// paid for.
func TestExistingSingleSegmentGlobsUnchanged(t *testing.T) {
	cfg := Default()
	cases := []struct {
		path string
		want bool
		why  string
	}{
		{"spec/models/user_spec.rb", true, "ruby: spec/ source dir"},
		{"engines/core/spec/services/report_worker_spec.rb", true, "ruby: nested spec/"},
		{"app/jobs/reporting/cache_warmup_ab_test.rb", false, "ruby: production A/B-test job"},
		{"tests/Unit/FooTest.cs", true, "c#: tests/ directory"},
		{"src/MyApp.Tests/FooTests.cs", true, "c#: *.Tests/ project directory"},
		{"src/System.Private.Xml/XmlQualifiedNameTest.cs", false, "c#: production XPath node-test type"},
	}
	for _, tc := range cases {
		if got := pathglob.MatchAny(tc.path, cfg.Ignore); got != tc.want {
			t.Errorf("%s: ignored = %v, want %v (%s)", tc.path, got, tc.want, tc.why)
		}
	}
}

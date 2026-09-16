package facts

import (
	"path/filepath"
	"strings"
)

// testSegments are path segments that mark a test or test-support tree: test
// sources, and the mock/fixture/helper packages that exist only to serve them.
// Membership is an exact, case-sensitive segment match.
//
// These carry the weight of the predicate. Every language enola supports puts its
// tests in a tool-enforced directory — an Xcode test target (Tests/, Mocks/), a
// Gradle source set (src/test/, src/androidTest/, and the KMP per-target trees),
// a pytest/rspec tree, a Jest __tests__/ — so the directory is the reliable signal
// and the filename is not. See IsTestPath for why that distinction matters.
var testSegments = map[string]bool{
	"test": true, "tests": true, "testing": true,
	"testutil": true, "testutils": true, "testdata": true,
	"integrationTests": true, "integration_tests": true,
	"mock": true, "mocks": true,
	"fixture": true, "fixtures": true,
	"spec": true, "specs": true,
	// Xcode names a test target's tree `Tests` and its mock tree `Mocks`; Jest and
	// Next.js colocate specs in `__tests__`. The match is case-sensitive, so the
	// lowercase entries above do not cover them.
	"__tests__": true,
	"Tests":     true, "Mocks": true,
	// Gradle instrumented tests and the Kotlin-Multiplatform per-target test trees.
	// The plain `test` segment above already covers local JVM unit tests.
	"androidTest": true, "commonTest": true, "jvmTest": true,
	"iosTest": true, "nativeTest": true,
	// Python shared test-support trees.
	"tests_common": true, "test_utils": true,
	// Cargo builds `benches/` as bench targets, which is this list's admission rule:
	// the directory is enforced by the tool rather than merely conventional. Rust's
	// `tests/` tree is already covered by the `tests` segment above. Measured on
	// tokio, where 11 of 43 high-severity performance findings were loops inside
	// benchmarks — a benchmark iterates by definition, so a loop there is the point
	// rather than a finding.
	"benches": true,
}

// testFileSuffixes are the filename conventions safe enough to trust on their own,
// for the languages whose tests are genuinely colocated with production source:
// Go's `_test.go` (reserved by the compiler — a production file cannot be named
// this), pytest's `*_test.py`, RSpec's `*_spec.rb`, and the dotted Jest/Vitest
// forms, which really do appear outside a test tree (`cypress/e2e/explore/
// chart.test.js` on python/superset is one).
//
// Several sibling conventions are deliberately ABSENT. The criterion is empirical:
// a filename rule is dropped when, measured across the corpus, the files it
// uniquely claims — the ones no directory segment already covers — are ALL
// production code. Each of these was:
//
//   - Swift `Tests.swift`/`Test.swift` and Kotlin `Test.kt`/`Tests.kt`/`Spec.kt`.
//     On a large iOS app they matched 402 files, 393 already covered by a segment,
//     and all 9 of the remainder were production A/B-test features
//     (`FeatureRolloutABTest.swift`, `Tracking+ExperimentABTest.swift`, …). On
//     a large Android app: 261 matched, 260 already covered, the 1 remainder a
//     production model (`api/…/model/ABTest.kt`). Neither SPM nor Gradle will build
//     a test from a production target, so the real tests are always in the test
//     tree and these rules had no true positives to contribute.
//   - Ruby `_test.rb`. Across the whole corpus it uniquely claimed exactly one
//     file: `app/jobs/notification/finish_experiment_ab_test.rb` — the
//     production ActiveJob that the identically-shaped `**/*_test.rb` ignore glob
//     once deleted from the graph outright, and which is why that glob is now
//     directory-scoped. Minitest files live in `test/`, which the segments claim.
//   - pytest's `test_*.py` PREFIX. It uniquely claimed three files on
//     python/superset and all three were production commands that *test a database
//     connection*: `superset/cli/test_db.py`, `superset/cli/test_loaders.py`,
//     `superset/commands/database/test_connection.py`.
//
// The rule that survives is: a name that merely LOOKS like a test is production.
// Erring this way costs at worst a false negative (a stray colocated test rated as
// production, which shows up as noise); erring the other way silently removes
// production code from the analysis, which is strictly worse and much harder to
// notice. It is the same reason ModuleRoleForPath matches whole '-'/'_' tokens
// rather than substrings.
//
// Residual risk, recorded honestly: `_test.py` and `_spec.rb` have the same
// theoretical collision as `_test.rb` (a production `feature_ab_test.py`), but
// unlike `_test.rb` no such file exists on the corpus, and dropping them would
// re-introduce false-positive orphans for colocated pytest suites. If one ever
// turns up, they go the way of `_test.rb`.
var testFileSuffixes = []string{
	"_test.go",
	"_test.py",
	"_spec.rb",
	".test.ts", ".test.tsx", ".spec.ts", ".spec.tsx",
	".test.js", ".test.jsx", ".spec.js", ".spec.jsx",
	// End-to-end suites. `.e2e-spec.ts` is what the Nest CLI generates and what its
	// jest-e2e config matches (testRegex ".e2e-spec.ts$"); `.e2e.ts` is the
	// Playwright/Cypress convention. Both are tool-enforced, which is this list's
	// admission rule — and neither is reachable by the `.spec.ts`/`.test.ts` entries
	// above, because those require a leading dot the hyphenated form does not have.
	// Without them a NestJS API's e2e suite reads as production HTTP-client code:
	// on one real NestJS API that was 500+ supertest calls turned into client routes,
	// which fabricated a cross-repo dependency edge out of test traffic.
	".e2e-spec.ts", ".e2e-spec.tsx", ".e2e.ts", ".e2e.tsx",
}

// IsTestPath reports whether a repo-relative path is test or test-support code.
//
// This is the single definition. The dead-code detector (which must not offer an
// XCTest helper as a deletion candidate), the performance analyzer (whose findings
// on test code are not actionable) and the god-class/hotspots explainers (which
// must not rank a test assertion helper as the repo's most central symbol) all
// classify through here. They previously carried three separate copies that had
// drifted apart in both directions, and a fourth consumer had no path check at all.
//
// Conservative by construction: an exact directory-segment match, or a filename
// convention that a tool enforces. A name that merely *looks* like a test —
// `ABTest.swift`, `test_job.rb`, `contest/`, `latest/` — is production code and
// stays in the graph.
func IsTestPath(p string) bool {
	if p == "" {
		return false
	}
	p = filepath.ToSlash(p)
	for _, seg := range strings.Split(p, "/") {
		if testSegments[seg] {
			return true
		}
	}
	base := p
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, suf := range testFileSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	// conftest.py is pytest's plugin/config file: the name is exact and reserved by
	// the runner, so unlike the `test_*.py` discovery prefix it cannot collide with
	// a production module.
	return base == "conftest.py"
}

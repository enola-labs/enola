package config

import (
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/pathglob"
)

// TestJavaTestGlobsScopeToTestSourceSets pins the Java half of the test-ignore
// contract: Maven and Gradle test sources are ignored for normal indexing AND
// matched by TestGlobs, so JavaExtractor.ExtractTestRefs recovers their references,
// and production files that merely end in Test, or sit in a package named test,
// stay indexed. The bundled mcp-arch.yaml decides every path the same way.
func TestJavaTestGlobsScopeToTestSourceSets(t *testing.T) {
	def := Default()
	bundled, err := Load(filepath.Join("..", "..", "mcp-arch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		test bool
	}{
		{"dubbo-common/src/test/java/org/apache/dubbo/common/URLTest.java", true},
		{"src/test/java/demo/support/Fixtures.java", true},
		{"app/src/androidTest/java/demo/ui/ScreenTest.java", true},
		{"service/src/integrationTest/java/demo/ApiIT.java", true},
		{"core/src/testFixtures/java/demo/FakeClock.java", true},
		{"app/src/testDemo/java/demo/DemoTest.java", true},
		{"app/src/androidTestDemo/java/demo/DemoScreenTest.java", true},
		{"api/src/main/java/demo/model/ABTest.java", false},
		{"app/src/main/java/demo/test/Seed.java", false},
		{"src/main/java/org/junit/TestCase.java", false},
		{"app/src/debug/java/demo/DebugMenu.java", false},
	} {
		ignored := pathglob.MatchAny(tc.path, def.Ignore)
		globbed := pathglob.MatchAny(tc.path, def.TestGlobs)
		if ignored != tc.test || globbed != tc.test {
			t.Errorf("%s: ignored=%v testGlob=%v, want both %v", tc.path, ignored, globbed, tc.test)
		}
		if tc.test && !facts.IsTestPath(tc.path) {
			t.Errorf("%s: matched by the test globs but facts.IsTestPath calls it production", tc.path)
		}
		if got, want := walkFate(bundled, tc.path), walkFate(def, tc.path); got != want {
			t.Errorf("%s: mcp-arch.yaml %s, defaults %s", tc.path, got, want)
		}
	}
}

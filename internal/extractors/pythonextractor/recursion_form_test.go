package pythonextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// recursiveBySuffix reports the recursive_self flag of the one symbol whose name ends
// in suffix, failing the test when there is not exactly one.
func recursiveBySuffix(t *testing.T, ff []facts.Fact, suffix string) bool {
	t.Helper()
	var hit []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			hit = append(hit, f)
		}
	}
	if len(hit) != 1 {
		var names []string
		for _, f := range ff {
			names = append(names, f.Name)
		}
		t.Fatalf("want one symbol ending %q, got %d of %v", suffix, len(hit), names)
	}
	return hit[0].Props["recursive_self"] == true
}

// A bare name inside a method is never that method: reaching it takes self. or cls.
func TestPyRecursion_BareCallInMethodIsNotSelfCall(t *testing.T) {
	ff := astExtract(t, "runner.py", `
class BaseJobRunner:
    @classmethod
    def most_recent_job(cls, session):
        from jobs.job import most_recent_job

        return most_recent_job(cls.job_type, session=session)
`, false)
	if recursiveBySuffix(t, ff, "BaseJobRunner.most_recent_job") {
		t.Errorf("most_recent_job(...) is the imported function, not the classmethod")
	}
}

func TestPyRecursion_RealSelfCallsStillFlagged(t *testing.T) {
	ff := astExtract(t, "tree.py", `
class Tree:
    def walk(self, node):
        for child in node.children:
            self.walk(child)

def redact(node):
    return [redact(c) for c in node]
`, false)
	for _, name := range []string{"Tree.walk", ".redact"} {
		if !recursiveBySuffix(t, ff, name) {
			t.Errorf("%s calls itself and is not flagged recursive_self", name)
		}
	}
}

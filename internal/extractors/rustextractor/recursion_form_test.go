package rustextractor

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

// An edge to the enclosing function is not a call to it. Each of these produced
// one, and each was reported as recursive.
func TestRustRecursion_SameNameIsNotSelfCall(t *testing.T) {
	ff := extractAST(t, `
struct Queue { list: Vec<u32> }

impl Drop for Queue {
    fn drop(&mut self) {
        while let Some(h) = self.list.pop() {
            drop(h)
        }
    }
}

struct Wheel { elapsed: u64 }

impl Wheel {
    fn level_for(&self, when: u64) -> usize {
        level_for(self.elapsed, when)
    }
}

fn level_for(elapsed: u64, when: u64) -> usize { (elapsed ^ when) as usize }

struct Command { std: Inner }

impl Command {
    fn arg(&mut self, arg: u32) -> &mut Command {
        self.std.arg(arg);
        self
    }
    fn max_error(mut self, max_error: f64) -> Self {
        self.std.set(max_error);
        self
    }
}

struct Notified(u32);

impl Notified {
    fn fmt(&self, fmt: &mut Formatter) -> Result {
        write!(fmt, "task::Notified({})", self.0)
    }
}
`)
	for _, name := range []string{"Queue.drop", "Wheel.level_for", "Command.arg", "Command.max_error", "Notified.fmt"} {
		if recursiveBySuffix(t, ff, name) {
			t.Errorf("%s is flagged recursive_self; it calls a different function of the same name", name)
		}
	}
}

func TestRustRecursion_RealSelfCallsStillFlagged(t *testing.T) {
	ff := extractAST(t, `
struct Tree { kids: Vec<u32> }

impl Tree {
    fn display(&self, depth: u32) {
        for k in &self.kids {
            self.display(depth + 1);
        }
    }
    fn count(n: u32) -> u32 {
        if n == 0 { 0 } else { Self::count(n - 1) + 1 }
    }
}

fn clean(n: u32) -> u32 {
    if n == 0 { 0 } else { clean(n - 1) }
}

fn in_macro(n: u32) -> Vec<u32> {
    vec![in_macro(n - 1).len() as u32]
}
`)
	for _, name := range []string{"Tree.display", "Tree.count", ".clean", ".in_macro"} {
		if !recursiveBySuffix(t, ff, name) {
			t.Errorf("%s calls itself and is not flagged recursive_self", name)
		}
	}
}

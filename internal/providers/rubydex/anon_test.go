package rubydex

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const anonymousFixture = "module Integration\n" +
	"  class Runner\n" +
	"    def create_session\n" +
	"      Class.new do\n" +
	"        Runner.new\n" +
	"        helper\n" +
	"        def inside; Integration::Runner.new; end\n" +
	"      end\n" +
	"    end\n" +
	"    def helper; end\n" +
	"  end\n" +
	"end\n" +
	"Mixin = Module.new do\n" +
	"  Runner.new\n" +
	"  def shared; Integration::Runner.new; end\n" +
	"end\n"

func anonymousFacts(t *testing.T, lib *Library) ([]string, Result) {
	t.Helper()
	repo := writeFiles(t, map[string]string{"Gemfile": "source \"https://rubygems.org\"\n", "app/runner.rb": anonymousFixture})
	result := Collect(context.Background(), lib, repo, nil)
	var names []string
	for _, f := range result.Facts {
		names = append(names, f.Name)
	}
	for _, f := range result.Prism {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names, result
}

func anonymousFactNames(t *testing.T, lib *Library) []string {
	t.Helper()
	names, _ := anonymousFacts(t, lib)
	return names
}

// The engine names a Class.new block by a hash of the absolute path and an
// offset, so a fact named with it differs between a clone, a worktree and CI.
// The call belongs to the method around the block, and the same repository at
// two paths gives the same names.
func TestCollect_ACallInAnAnonymousBlockBelongsToTheMethodAroundIt(t *testing.T) {
	lib := requireLibrary(t)
	first := anonymousFactNames(t, lib)
	second := anonymousFactNames(t, lib)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("names differ between two paths\n first: %v\nsecond: %v", first, second)
	}
	have := map[string]bool{}
	for _, name := range first {
		if strings.Contains(name, "<anonymous>") {
			t.Errorf("%s: an anonymous id is in a fact name", name)
		}
		have[name] = true
	}
	for _, want := range []string{
		"prism-new: Integration::Runner#create_session -> Class",
		"rubydex-call: Integration::Runner#create_session -> Class.new",
	} {
		if !have[want] {
			t.Errorf("missing %s in %v", want, first)
		}
	}
}

// A def inside the block runs when the anonymous class's method is called, so
// naming the method around the block as its caller would be a confident wrong
// answer. Nothing is written for it and the skip is counted.
func TestCollect_ACallInAMethodOfAnAnonymousClassNamesNoCaller(t *testing.T) {
	lib := requireLibrary(t)
	_, result := anonymousFacts(t, lib)
	for _, group := range [][]facts.Fact{result.Facts, result.Prism} {
		for _, f := range group {
			if f.Line == 7 {
				t.Errorf("%s (line %d): a call inside a method of an anonymous class is written", f.Name, f.Line)
			}
		}
	}
	for name, census := range map[string]facts.ProviderCensus{"rubydex": result.Census, "prism": result.PrismCensus} {
		counted := 0
		for _, cause := range census.SkipCauses {
			if cause.Cause == "caller is a method of an anonymous class" {
				counted = cause.Count
			}
		}
		if counted < 1 {
			t.Errorf("%s census = %+v, want the anonymous method's calls counted", name, census)
		}
	}
}

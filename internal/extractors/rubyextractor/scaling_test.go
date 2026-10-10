package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func rbMethod(t *testing.T, src, name string) facts.Fact {
	t.Helper()
	f, ok := symbolsByName(extractFileAST([]byte(src), "app/thing.rb", false, false))[name]
	if !ok {
		t.Fatalf("no symbol %s", name)
	}
	return f
}

func rbCallDepth(t *testing.T, f facts.Fact, callee string) (int, bool) {
	t.Helper()
	calls := rbStrSlice(f, "calls_in_scaling_loop")
	depths, _ := f.PropAny("calls_in_scaling_loop_depth").([]int)
	if len(depths) != len(calls) {
		t.Fatalf("calls_in_scaling_loop %v and its depths %v are not aligned", calls, depths)
	}
	for i, c := range calls {
		if c == callee {
			return depths[i], true
		}
	}
	return 0, false
}

// The exponent of a call is the nest around the call, not the deepest in the method.
func TestRbScaling_CallDepthIsTheLoopsAroundTheCall(t *testing.T) {
	f := rbMethod(t, `class Job
  def run(users, grid)
    users.each do |u|
      notify(u)
    end
    grid.each do |row|
      grid.each do |other|
        compare(row, other)
      end
    end
  end
end
`, "Job#run")
	if got := rbIntProp(t, f, "scaling_loop_depth"); got != 2 {
		t.Errorf("scaling_loop_depth = %d, want 2", got)
	}
	if d, ok := rbCallDepth(t, f, "notify"); !ok || d != 1 {
		t.Errorf("notify depth = %d (found=%v), want 1", d, ok)
	}
	if d, ok := rbCallDepth(t, f, "compare"); !ok || d != 2 {
		t.Errorf("compare depth = %d (found=%v), want 2", d, ok)
	}
}

// Parent then children: the inner loop walks what the outer one bound, so the
// nest visits each child once. It repeats, and adds no factor.
func TestRbScaling_LoopOverTheOuterElementAddsNoDepth(t *testing.T) {
	f := rbMethod(t, `class Export
  def run(lists)
    lists.each do |list|
      list.accounts.select(:name).each do |account|
        emit(list, account)
      end
    end
  end
end
`, "Export#run")
	if got := rbIntProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2: both are loops", got)
	}
	if got := rbIntProp(t, f, "scaling_loop_depth"); got != 1 {
		t.Errorf("scaling_loop_depth = %d, want 1", got)
	}
	if d, ok := rbCallDepth(t, f, "emit"); !ok || d != 1 {
		t.Errorf("emit depth = %d (found=%v), want 1: still once per account", d, ok)
	}
}

// A loop over an unrelated collection inside another is the product it looks like.
func TestRbScaling_IndependentInnerLoopStillScales(t *testing.T) {
	f := rbMethod(t, `class Match
  def run(users, others)
    users.each do |u|
      others.select { |o| o.id != u.id }.each do |o|
        pair(u, o)
      end
    end
  end
end
`, "Match#run")
	if got := rbIntProp(t, f, "scaling_loop_depth"); got != 2 {
		t.Errorf("scaling_loop_depth = %d, want 2: others is not reached through u", got)
	}
}

// One call per batch is the batching. One call per element of the batch is not.
func TestRbScaling_BatchIteratorCallIsNotPerElement(t *testing.T) {
	f := rbMethod(t, `class Purge
  def run(ids, rows)
    ids.each_slice(1000) do |batch|
      delete_rows(batch)
    end
    rows.each_slice(100) do |slice|
      slice.each do |row|
        save_row(row)
      end
    end
  end
end
`, "Purge#run")
	if !rbContains(rbStrSlice(f, "calls_in_loop"), "delete_rows") {
		t.Errorf("calls_in_loop lost delete_rows: %v", rbStrSlice(f, "calls_in_loop"))
	}
	if _, ok := rbCallDepth(t, f, "delete_rows"); ok {
		t.Errorf("delete_rows, made once per batch, is listed as a per-element call")
	}
	if d, ok := rbCallDepth(t, f, "save_row"); !ok || d != 1 {
		t.Errorf("save_row depth = %d (found=%v), want 1: one pass over the rows", d, ok)
	}
}

// A loop over a literal does not repeat with the input: its calls are in neither list.
func TestRbScaling_ConstantLoopCallsAreNotListed(t *testing.T) {
	src := `class Seed
  def run(xs)
    xs.each { |x| log(x) }
  end

  def fixed
    %w[a b].each { |n| log(n) }
  end
end
`
	if d, ok := rbCallDepth(t, rbMethod(t, src, "Seed#run"), "log"); !ok || d != 1 {
		t.Errorf("run: log depth = %d (found=%v), want 1", d, ok)
	}
	fixed := rbMethod(t, src, "Seed#fixed")
	if got := rbStrSlice(fixed, "calls_in_scaling_loop"); len(got) != 0 {
		t.Errorf("fixed: calls_in_scaling_loop = %v, want none", got)
	}
}

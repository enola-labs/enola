package orphans

import "testing"

// The count saturates at 2 and must be EXACT up to there, whatever order sources
// arrive in — that exactness is what lets the sources be thrown away.
func TestSummaryCountsDistinctSourcesExactly(t *testing.T) {
	r := make(refIndex)
	r.add("target", "a")
	if got := r["target"].n; got != 1 {
		t.Fatalf("n=%d after one source, want 1", got)
	}
	r.add("target", "a") // the same source again is not a second source
	if got := r["target"].n; got != 1 {
		t.Fatalf("n=%d after a repeated source, want 1", got)
	}
	r.add("target", "b")
	if got := r["target"].n; got != 2 {
		t.Fatalf("n=%d after a second distinct source, want 2", got)
	}
	r.add("target", "c")
	if got := r["target"].n; got != 2 {
		t.Fatalf("n=%d, want it to saturate at 2", got)
	}
}

// A name referenced only by itself is not referenced. This is the case the
// saturating count could have broken, and the reason `one` is kept.
func TestSelfReferenceIsNotUse(t *testing.T) {
	r := make(refIndex)
	r.add("Foo.bar", "Foo.bar")
	if r["Foo.bar"].referenced("Foo.bar") {
		t.Error("a symbol referencing only itself reads as referenced")
	}
	r.add("Foo.bar", "Other.baz")
	if !r["Foo.bar"].referenced("Foo.bar") {
		t.Error("a second, external source did not make it referenced")
	}
}

func TestUnknownNameIsNotReferenced(t *testing.T) {
	r := make(refIndex)
	if r["absent"].referenced("anything") {
		t.Error("a name nothing references reads as referenced")
	}
}

// The fold samples keep one source per OWNER, because that consumer discards a
// whole owner's sources at once. Keeping siblings would cost memory and answer
// nothing new.
func TestFoldSamplesKeepDistinctOwners(t *testing.T) {
	r := make(refIndex)
	for _, src := range []string{"Owner.a", "Owner.b", "Owner.c"} {
		r.add("target", src)
	}
	if got := r.sources("target"); len(got) != 1 {
		t.Fatalf("kept %v, want one source for a single owner", got)
	}
	r.add("target", "Other.a")
	if got := r.sources("target"); len(got) != 2 {
		t.Fatalf("kept %v, want a second owner's source too", got)
	}
}

// The bound is what makes the index small; it must actually hold.
func TestFoldSamplesAreBounded(t *testing.T) {
	r := make(refIndex)
	for _, o := range []string{"A", "B", "C", "D", "E", "F", "G"} {
		r.add("target", o+".m")
	}
	if got := len(r.sources("target")); got != foldSamples {
		t.Fatalf("kept %d sources, want the bound of %d", got, foldSamples)
	}
}

// Two owners is the minimum the fold's argument needs: it excludes one owner, so
// one must survive. Anything less and the bound would change the findings.
func TestTwoOwnersSurviveExcludingOne(t *testing.T) {
	r := make(refIndex)
	r.add("target", "Excluded.m")
	r.add("target", "Kept.m")
	var survived bool
	for _, src := range r.sources("target") {
		if ownerOf(src) != "Excluded" {
			survived = true
		}
	}
	if !survived {
		t.Error("excluding one owner left no source, so the fold would lose a credit")
	}
}

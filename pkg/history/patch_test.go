package history

import (
	"bytes"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func lines(ss ...string) []string { return ss }

func TestComputePatch_AndApply_RoundTrip(t *testing.T) {
	prev := lines("a", "b", "c")
	cur := lines("b", "c2", "d")

	p := ComputePatch(prev, cur)
	got, err := p.Apply(prev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "b,c2,d" {
		t.Fatalf("apply produced %v, want [b c2 d]", got)
	}
}

// Multisets, not sets. Two facts can serialize to the identical line — Swift declares the
// same class under mutually exclusive #if branches, a file imports one target twice — so
// the COUNT is part of the snapshot. Treating the input as a set would drop a duplicate
// silently and reconstruct a snapshot one line short of the truth.
func TestComputePatch_CountsDuplicates(t *testing.T) {
	prev := lines("dup", "dup", "dup", "x")
	cur := lines("dup", "x")

	p := ComputePatch(prev, cur)
	if len(p.Del) != 2 {
		t.Fatalf("want 2 removals (3 copies down to 1), got %d: %v", len(p.Del), p.Del)
	}
	got, err := p.Apply(prev)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("apply produced %d lines, want 2: %v", len(got), got)
	}
}

func TestComputePatch_AddingADuplicate(t *testing.T) {
	p := ComputePatch(lines("dup"), lines("dup", "dup"))
	if len(p.Add) != 1 || len(p.Del) != 0 {
		t.Fatalf("want exactly one addition, got add=%v del=%v", p.Add, p.Del)
	}
}

// Applying to the wrong parent must fail HERE, naming the offending line, rather than
// producing a plausible snapshot that fails a hash check two members later — or worse, one
// that happens to pass.
func TestPatch_ApplyToTheWrongParentIsAnError(t *testing.T) {
	p := ComputePatch(lines("a", "b"), lines("a"))
	if _, err := p.Apply(lines("x", "y")); err == nil {
		t.Fatal("removing a line the parent does not have must be an error")
	}
}

func TestPatch_EmptyAndSize(t *testing.T) {
	if !(Patch{}).Empty() {
		t.Error("a patch with no entries is empty")
	}
	if (Patch{Add: lines("x")}).Empty() {
		t.Error("a patch with an addition is not empty")
	}
	if got := (Patch{Add: lines("abc"), Del: lines("de")}).Size(); got != 9 {
		t.Errorf("Size() = %d, want 9 (3+2 chars + 2 bytes of framing each)", got)
	}
}

// A base is a patch against the empty set — not a special case, which is what keeps the
// encoder, decoder and apply path single.
func TestPatch_BaseIsAPatchAgainstNothing(t *testing.T) {
	all := lines("a", "b", "c")
	p := ComputePatch(nil, all)
	if len(p.Del) != 0 || len(p.Add) != 3 {
		t.Fatalf("a base must be pure additions, got add=%d del=%d", len(p.Add), len(p.Del))
	}
	got, err := p.Apply(nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("got %v", got)
	}
}

func TestRevision_EncodeDecodeRoundTrip(t *testing.T) {
	rev := Revision{
		Parent:    "sha256:parent",
		FactsHash: "sha256:facts",
		Receipt: facts.Receipt{
			SnapshotID:   "sha256:snap",
			EnolaVersion: "0.9.1",
			GeneratedAt:  "2026-08-03T10:00:00Z",
			RepoPath:     "/repo",
			Extractors:   []string{"go"},
			Explainers:   []string{"cycles"},
			ConfigHash:   "sha256:cfg",
			FactCount:    3,
		},
		Facts:    Patch{Add: lines(`{"kind":"symbol","name":"B"}`), Del: lines(`{"kind":"symbol","name":"A"}`)},
		Insights: Patch{Add: lines(`{"title":"x"}`)},
	}

	var buf bytes.Buffer
	if err := rev.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRevision(&buf)
	if err != nil {
		t.Fatal(err)
	}

	if got.Parent != rev.Parent || got.FactsHash != rev.FactsHash {
		t.Errorf("header lost: %+v", got)
	}
	if got.Receipt.EnolaVersion != "0.9.1" || got.Receipt.ConfigHash != "sha256:cfg" {
		t.Errorf("receipt lost: %+v", got.Receipt)
	}
	if len(got.Facts.Add) != 1 || got.Facts.Add[0] != rev.Facts.Add[0] {
		t.Errorf("fact additions lost: %v", got.Facts.Add)
	}
	if len(got.Facts.Del) != 1 || got.Facts.Del[0] != rev.Facts.Del[0] {
		t.Errorf("fact removals lost: %v", got.Facts.Del)
	}
	if len(got.Insights.Add) != 1 {
		t.Errorf("insight additions lost: %v", got.Insights.Add)
	}
}

// A base has no parent line at all, and must decode as one rather than as a header with an
// empty field — the difference decides whether the reader tries to chain.
func TestRevision_BaseHasNoParent(t *testing.T) {
	var buf bytes.Buffer
	rev := Revision{FactsHash: "sha256:x", Facts: Patch{Add: lines("a")}}
	if err := rev.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "parent") {
		t.Errorf("a base must not write a parent line:\n%s", buf.String())
	}
	got, err := DecodeRevision(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Parent != "" {
		t.Errorf("Parent = %q, want empty", got.Parent)
	}
}

// A fact line contains JSON, quotes, braces and (in enola's own graph) Unicode arrows. None
// of it may be mistaken for framing.
func TestRevision_SurvivesRealFactLines(t *testing.T) {
	real := []string{
		`{"kind":"dependency","name":"cmd/enola -\u003e context","file":"cmd/enola/main.go","line":4,"repo":"enola","props":{"language":"go","source":"stdlib"},"relations":[{"kind":"imports","target":"context"}]}`,
		`{"kind":"symbol","name":"Engine.WriteArtifacts","file":"internal/engine/engine.go","line":956,"props":{"symbol_kind":"method","signature":"func (e *Engine) WriteArtifacts(repoPath string) error"}}`,
	}
	var buf bytes.Buffer
	if err := (Revision{FactsHash: "sha256:x", Facts: Patch{Add: real}}).Encode(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRevision(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range real {
		if got.Facts.Add[i] != want {
			t.Errorf("line %d round-tripped wrong:\n got %s\nwant %s", i, got.Facts.Add[i], want)
		}
	}
}

// Add and Del are emitted sorted TOGETHER so the two halves of a line-only move land
// adjacent and gzip sees the redundancy — measured at a 20% saving on real snapshots, where
// 65 of 82 changed lines are such moves.
func TestRevision_InterleavesAddsAndRemovesByLine(t *testing.T) {
	rev := Revision{
		FactsHash: "sha256:x",
		Facts: Patch{
			Del: lines(`{"name":"A","line":10}`, `{"name":"B","line":20}`),
			Add: lines(`{"name":"A","line":11}`, `{"name":"B","line":21}`),
		},
	}
	var buf bytes.Buffer
	if err := rev.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	posA := strings.Index(body, `-{"name":"A","line":10}`)
	posAnew := strings.Index(body, `+{"name":"A","line":11}`)
	posB := strings.Index(body, `-{"name":"B","line":20}`)
	if posA >= posAnew || posAnew >= posB {
		t.Errorf("want A's pair adjacent and before B's:\n%s", body)
	}
}

// An unknown header field is an error, not something to skip. This is an integrity format:
// quietly ignoring a field a newer writer considered load-bearing is how a reader renders a
// past that never happened. The version in the magic line is the way to change it.
func TestDecodeRevision_RejectsWhatItDoesNotUnderstand(t *testing.T) {
	for name, body := range map[string]string{
		"bad magic":     "something else 1\nfacts sha256:x\nreceipt 2\n{}\nfacts\n",
		"unknown field": revisionMagic + "\nmystery yes\nfacts sha256:x\nreceipt 2\n{}\nfacts\n",
		"bad sign":      revisionMagic + "\nfacts sha256:x\nreceipt 2\n{}\nfacts\n?nope\n",
		"truncated":     revisionMagic + "\nfacts sha256:x\nreceipt 99\n{}\n",
	} {
		if _, err := DecodeRevision(strings.NewReader(body)); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}

func TestHashLines_MatchesTheFileItDescribes(t *testing.T) {
	// The hash must be over the bytes facts.jsonl actually holds — each line plus a
	// newline — so it is directly comparable with the receipt's recorded output hash.
	a := HashLines(lines("one", "two"))
	b := HashLines(lines("one", "two"))
	if a != b {
		t.Fatal("hashing is not deterministic")
	}
	if HashLines(lines("one", "two")) == HashLines(lines("two", "one")) {
		t.Error("order must affect the hash — facts.jsonl is a sorted file, not a set")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Errorf("hash %q must carry the sha256: prefix every other enola hash does", a)
	}
}

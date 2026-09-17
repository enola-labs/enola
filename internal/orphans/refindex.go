package orphans

// refIndex answers the only two questions the analyzer asks about who references a
// name, without storing everyone who does.
//
// It replaces a map[string]map[string]struct{} that held every source per name.
// On rust-lang/rust that index carried 120,897,613 entries over 1,625,276
// relations — 75 per relation — because the short-form folding keys on bare names
// and a UI test corpus declares `Foo`, `T` and `Struct` in tens of thousands of
// files. 9,984 names carried a thousand sources or more. It retained 5 GiB, which
// was the whole of the analyzer's 6x memory cost and enough to fail the
// repository's own memory ratchet.
//
// Neither consumer ever read the sets. Both ask "is there at least one source
// satisfying a predicate", so a bounded summary answers them EXACTLY rather than
// approximately. That claim is the whole design and each field below carries the
// argument for why it holds.
type refIndex map[string]*refSummary

// foldSamples is how many sources a summary keeps for the member-use fold.
//
// Four is not a guess. The fold's predicate excludes the sources belonging to ONE
// owner, plus the owner's own name. Keeping one source per distinct owner means
// excluding an owner removes at most one of them, so if two or more owners
// reference the name, at least one survives — which is all the fold needs, because
// crediting an owner is idempotent. Four leaves headroom over the two the argument
// requires.
const foldSamples = 4

type refSummary struct {
	// n is the number of DISTINCT sources, saturating at 2. It saturates because no
	// caller can distinguish 2 from 200: referencedByOther asks whether any source
	// differs from one given name, and two distinct sources cannot both equal it.
	//
	// Saturating is also what makes the count exact without storing the sources. n
	// reaches 2 only when a source differs from the first one recorded, so it is
	// never inflated by a repeated source, whatever order they arrive in. A plain
	// counter would have needed the set back to deduplicate.
	n int32

	// one is the first source recorded, and is read only while n == 1. It is what
	// lets the n == 1 case stay exact: a name referenced once, by itself, is not
	// referenced.
	one string

	// fold holds up to foldSamples sources with DISTINCT owners, for
	// foldMemberUseIntoOwner. Sources sharing an owner are interchangeable to that
	// consumer — it discards all of one owner's at once — so keeping a second from
	// the same owner would cost memory and answer nothing new.
	fold []string
}

// referenced reports whether any source other than self references this name.
func (s *refSummary) referenced(self string) bool {
	if s == nil {
		return false
	}
	if s.n >= 2 {
		return true
	}
	return s.one != self
}

// add records that source references name, maintaining both summaries.
func (r refIndex) add(name, source string) {
	if name == "" {
		return
	}
	s := r[name]
	if s == nil {
		r[name] = &refSummary{n: 1, one: source, fold: []string{source}}
		return
	}
	if s.n == 1 && s.one != source {
		s.n = 2
	}
	if len(s.fold) < foldSamples {
		owner := ownerOf(source)
		for _, have := range s.fold {
			if have == source || ownerOf(have) == owner {
				return
			}
		}
		s.fold = append(s.fold, source)
	}
}

// sources returns the sampled sources for the member-use fold. It is deliberately
// NOT a complete list, and only that consumer may read it; see refSummary.fold.
func (r refIndex) sources(name string) []string {
	if s := r[name]; s != nil {
		return s.fold
	}
	return nil
}

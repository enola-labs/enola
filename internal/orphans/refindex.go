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

	// fold holds up to foldSamples PRODUCTION sources with DISTINCT owners, for
	// foldMemberUseIntoOwner. Sources sharing an owner are interchangeable to that
	// consumer — it discards all of one owner's at once — so keeping a second from
	// the same owner would cost memory and answer nothing new.
	fold []string

	// prodN and prodOne are n and one restricted to production sources: every source
	// except a test file's reference (see addTest). They answer "is this used outside
	// its tests", and saturate and stay exact for the same reasons n and one do.
	prodN   int32
	prodOne string

	// testFold is fold for test sources. It is kept apart so the member-use fold can
	// credit a type with its members' test use as TEST use: merged into one list, a
	// type whose member only specs call would read as used in production. The bound
	// argument above holds for each list on its own.
	testFold []string
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

// referencedInProduction is referenced, counting production sources only.
func (s *refSummary) referencedInProduction(self string) bool {
	if s == nil || s.prodN == 0 {
		return false
	}
	if s.prodN >= 2 {
		return true
	}
	return s.prodOne != self
}

// add records that a production source references name, maintaining every summary.
func (r refIndex) add(name, source string) { r.record(name, source, false) }

// addTest records that a test source (a spec or test file) references name. It
// counts toward referenced but not toward referencedInProduction, which is what
// separates a symbol only its tests call from one the application uses.
func (r refIndex) addTest(name, source string) { r.record(name, source, true) }

func (r refIndex) record(name, source string, test bool) {
	if name == "" {
		return
	}
	s := r[name]
	if s == nil {
		s = &refSummary{n: 1, one: source}
		r[name] = s
	} else if s.n == 1 && s.one != source {
		s.n = 2
	}
	if test {
		s.testFold = addSample(s.testFold, source)
		return
	}
	switch {
	case s.prodN == 0:
		s.prodN, s.prodOne = 1, source
	case s.prodN == 1 && s.prodOne != source:
		s.prodN = 2
	}
	s.fold = addSample(s.fold, source)
}

// addSample appends source unless the list is full or already holds a source
// with the same owner.
func addSample(list []string, source string) []string {
	if len(list) >= foldSamples {
		return list
	}
	owner := ownerOf(source)
	for _, have := range list {
		if have == source || ownerOf(have) == owner {
			return list
		}
	}
	return append(list, source)
}

// sources returns the sampled production sources for the member-use fold. It is
// deliberately NOT a complete list, and only that consumer may read it; see
// refSummary.fold.
func (r refIndex) sources(name string) []string {
	if s := r[name]; s != nil {
		return s.fold
	}
	return nil
}

// testSources is sources for test sources; see refSummary.testFold.
func (r refIndex) testSources(name string) []string {
	if s := r[name]; s != nil {
		return s.testFold
	}
	return nil
}

package orphans

import (
	"sort"
	"strings"
)

// prefixSet answers "does any of these prefixes start s" with one binary search.
//
// The dynamic-dispatch prefixes are gathered from every file reference in the
// store, so a cluster repeats the same handful many times over; checking each
// function's name against the raw list cost two seconds at a hundred repositories.
// The set keeps each prefix once and drops any that a shorter one already covers,
// which leaves it prefix-free: then the only candidate for a match is the greatest
// prefix not after s, because any prefix between a match p and s would itself
// start with p.
type prefixSet []string

func newPrefixSet(prefixes []string) prefixSet {
	ps := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if p != "" {
			ps = append(ps, p)
		}
	}
	sort.Strings(ps)
	out := ps[:0]
	for _, p := range ps {
		// Sorted, so a prefix that covers p is the last one kept, if any.
		if len(out) > 0 && strings.HasPrefix(p, out[len(out)-1]) {
			continue
		}
		out = append(out, p)
	}
	return prefixSet(out)
}

func (ps prefixSet) matches(s string) bool {
	// The first index whose prefix sorts after s; the candidate is the one before.
	i := sort.Search(len(ps), func(i int) bool { return ps[i] > s })
	return i > 0 && strings.HasPrefix(s, ps[i-1])
}

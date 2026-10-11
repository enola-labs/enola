package pathglob

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// Len reports how many patterns the set holds.
func (g *Set) Len() int {
	if g == nil {
		return 0
	}
	return len(g.pats)
}

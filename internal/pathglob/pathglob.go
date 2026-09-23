// Package pathglob matches forward-slash repository paths against the ignore and
// test glob lists the config declares.
package pathglob

import (
	"github.com/enola-labs/enola/internal/factpath"
	"path/filepath"
	"slices"
	"strings"
)

// MatchAny reports whether a forward-slash path matches any of the patterns.
// It is the single matcher behind both the ignore list and the test globs, so a
// file the two lists disagree about cannot exist: an ignored file that stops being
// a test necessarily stops being ignored.
func MatchAny(relPath string, patterns []string) bool {
	_, ok := Match(relPath, patterns)
	return ok
}

// matchGlob returns the first pattern that matches relPath. The receipt records it
// beside the skipped path, so "why is this file missing from the graph?" is a
// lookup rather than an investigation. Supported forms:
//
//	vendor/**                 anchored directory prefix
//	**/build/**               a directory named "build" at any depth
//	**/*.Tests/**             a directory whose NAME matches a glob, at any depth
//	**/*_test.go              a basename glob at any depth
//	**/spec/**/*_spec.rb      a basename glob under a directory named "spec"
//	**/*.Tests/**/*.cs        a basename glob under a glob-named directory
//	**/src/test/**/*.scala    a basename glob under a directory PATH at any depth
//
// The last two are the only forms that constrain directory and filename together;
// see matchDirScopedGlob for why the Ruby test globs need it and why Scala's need
// two directory segments rather than one.
//
// The directory segment may itself be a glob. A literal is the common case and is
// unaffected — filepath.Match on a pattern with no metacharacters is equality — but
// .NET needs the general form: the dominant solution layout puts a test project in
// `MyApp.Tests/` beside `MyApp/` rather than under a `tests/` directory, so no
// literal segment names it.
func Match(relPath string, patterns []string) (string, bool) {
	for _, pattern := range patterns {
		// "<prefix>/**/<fileglob>". Handled first and exclusively: the branches
		// below would match such a pattern only when exactly one directory sits
		// between prefix and file, which is an artifact of filepath.Match reading
		// "**" as "*", not a rule anyone intended.
		if i := strings.Index(pattern, "/**/"); i >= 0 {
			prefix, fileGlob := pattern[:i], pattern[i+len("/**/"):]
			if !strings.Contains(fileGlob, "/") {
				if matchDirScopedGlob(relPath, prefix, fileGlob) {
					return pattern, true
				}
				continue
			}
		}
		if strings.HasPrefix(pattern, "**/") && strings.HasSuffix(pattern, "/**") {
			seg := strings.TrimSuffix(strings.TrimPrefix(pattern, "**/"), "/**")
			if seg != "" && !strings.Contains(seg, "/") {
				for _, part := range strings.Split(relPath, "/") {
					if matchSegment(seg, part) {
						return pattern, true
					}
				}
			}
		}
		if strings.HasSuffix(pattern, "/**") {
			dirPrefix := strings.TrimSuffix(pattern, "/**")
			if relPath == dirPrefix || strings.HasPrefix(relPath, dirPrefix+"/") {
				return pattern, true
			}
		}
		if m, err := factpath.Match(pattern, relPath); err == nil && m {
			return pattern, true
		}
		if strings.HasPrefix(pattern, "**/") {
			sub := strings.TrimPrefix(pattern, "**/")
			if m, err := factpath.Match(sub, filepath.Base(relPath)); err == nil && m {
				return pattern, true
			}
			if m, err := factpath.Match(sub, relPath); err == nil && m {
				return pattern, true
			}
		}
	}
	return "", false
}

// matchSegment compares one path segment against a pattern segment. A pattern
// with no metacharacters costs an equality check inside filepath.Match, so the
// literal case — every pattern that existed before .NET — behaves exactly as it
// did. An unparseable pattern yields ErrBadPattern and therefore no match, which
// is the safe direction: a malformed ignore entry hides nothing.
func matchSegment(pattern, part string) bool {
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern == part
	}
	m, err := factpath.Match(pattern, part)
	return err == nil && m
}

// matchDirScopedGlob reports whether relPath's basename matches fileGlob AND
// prefix names one of its ancestor directories ("**/<seg>" for a segment at any
// depth, otherwise an anchored literal path).
//
// A filename alone cannot classify a Ruby test. `lib/foo_test.rb` is one and
// `app/jobs/cache_warmup_ab_test.rb` is a production A/B-test job, yet both end in
// the token `test`; matching on the suffix deleted the latter from the graph
// entirely. Ruby settles it by convention — RSpec requires spec/, Minitest defaults
// to test/ — so the directory segment is the signal, and this predicate lets a
// single pattern demand both halves.
//
// A prefix after "**/" may name SEVERAL consecutive segments, which Scala needs and
// a single segment cannot express. sbt puts test sources under `src/test/`, but a
// directory merely NAMED `test` is routinely production: zio's `test-magnolia`
// module compiles `src/main/scala-3/zio/test/magnolia/*.scala`, and across the
// benchmark corpus a one-segment `**/test/**/*.scala` would have deleted 183
// production files — 175 of them ZIO's own test LIBRARY, whose package is literally
// `zio.test`. Demanding the pair `src/test` distinguishes the source set from the
// name, and leaves every existing one-segment pattern behaving exactly as before
// (a single segment is the length-1 case of the same scan).
//
// Because every element of dirSegs is by construction an ancestor of the basename,
// segment matching alone places the file under the directory: no depth bookkeeping,
// and "spec/user_spec.rb" (zero intervening directories) falls out for free.
func matchDirScopedGlob(relPath, prefix, fileGlob string) bool {
	segs := strings.Split(relPath, "/")
	if len(segs) < 2 {
		return false // no directory component, so no prefix can name an ancestor
	}
	dirSegs, base := segs[:len(segs)-1], segs[len(segs)-1]

	if m, err := factpath.Match(fileGlob, base); err != nil || !m {
		return false
	}
	if seg, ok := strings.CutPrefix(prefix, "**/"); ok {
		if seg == "" {
			return false
		}
		want := strings.Split(seg, "/")
		if len(want) == 1 {
			return slices.ContainsFunc(dirSegs, func(part string) bool {
				return matchSegment(want[0], part)
			})
		}
		return containsSegmentRun(dirSegs, want)
	}
	if prefix == "**" {
		return true // any directory
	}
	return strings.HasPrefix(relPath, prefix+"/")
}

// containsSegmentRun reports whether want appears as a run of CONSECUTIVE segments
// anywhere in dirSegs. Consecutive rather than merely present in order: `src/test`
// must mean a test source set, not a `src` directory that happens to have a `test`
// somewhere beneath it — which is the distinction that keeps zio's
// `src/main/scala-3/zio/test/` out of the test globs.
func containsSegmentRun(dirSegs, want []string) bool {
	if len(want) == 0 || len(want) > len(dirSegs) {
		return false
	}
	for i := 0; i+len(want) <= len(dirSegs); i++ {
		matched := true
		for j, w := range want {
			if !matchSegment(w, dirSegs[i+j]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// --- compiled matching ------------------------------------------------------
//
// Match analyses the SHAPE of every pattern on every call: which of the five
// forms it is, where its "/**/" sits, what its directory prefix splits into. None of
// that depends on the path being tested, and the walk tests every pattern against
// every entry it visits. With the bundled config that is 114 ignore patterns per
// directory entry, 49 of which re-split the path before they look at the basename.
//
// Compile does the shape analysis once and Set.Match reuses it, splitting
// the path at most once per call and never concatenating a string to test a prefix.
// The branch order is Match's, pattern for pattern, because the first matching
// pattern is what the receipt records beside a skipped path.
//
// A Set is immutable once built and safe for concurrent use.
type Set struct {
	pats []compiledGlob
}

// compiledGlob is one pattern with its shape resolved. The flags are not exclusive:
// `**/build/**` is all three of segAnyDepth, dirPrefixed and starPrefixed, and Match
// tries them in that order, which is the order Match tries them in.
type compiledGlob struct {
	pattern string

	// dirScoped is "<prefix>/**/<fileGlob>" where fileGlob names no directory. It is
	// exclusive: Match `continue`s past the other branches for such a pattern.
	dirScoped bool
	dsPrefix  string   // the part before "/**/"
	dsFile    string   // the basename glob after it
	dsWant    []string // dsPrefix after "**/", split into segments
	dsNoSeg   bool     // dsPrefix was exactly "**/", which matches nothing
	dsAnyDir  bool     // dsPrefix was exactly "**"
	dsTail    string   // dsFile's literal tail; see literalTail

	segAnyDepth bool // "**/<seg>/**" with seg naming one segment
	seg         string

	dirPrefixed bool // "<dir>/**"
	dirPrefix   string

	starPrefixed bool // "**/<sub>"
	sub          string

	// tail and subTail are literal tails (see literalTail) for the two branches
	// that call filepath.Match: the whole pattern against the path, and `sub`
	// against the basename and the path. They are computed from DIFFERENT strings
	// and must not be shared — `**/x` has tail "/x" and subTail "x", and the path
	// "x" matches it through the sub branch while failing the full-pattern tail.
	tail    string
	subTail string
}

// literalTail returns the part of a glob that any string it matches must end with:
// whatever follows its last metacharacter, or the whole thing when it has none (a
// pattern without metacharacters is compared for equality, so the subject ends with
// it too). Conservative by construction, since a stray "]" or an escaped
// metacharacter only shortens the tail, and a shorter tail rejects less.
func literalTail(pattern string) string {
	if i := strings.LastIndexAny(pattern, "*?[]"); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// Compile resolves each pattern's shape once, for a list that will be matched
// against many paths. Compiling a list to test one path is slower than Match, not
// faster: this is for the walk, not for one-off questions.
func Compile(patterns []string) *Set {
	g := &Set{pats: make([]compiledGlob, 0, len(patterns))}
	for _, pattern := range patterns {
		c := compiledGlob{pattern: pattern}
		if i := strings.Index(pattern, "/**/"); i >= 0 {
			prefix, fileGlob := pattern[:i], pattern[i+len("/**/"):]
			if !strings.Contains(fileGlob, "/") {
				c.dirScoped, c.dsPrefix, c.dsFile = true, prefix, fileGlob
				c.dsTail = literalTail(fileGlob)
				switch seg, cut := strings.CutPrefix(prefix, "**/"); {
				case cut && seg == "":
					c.dsNoSeg = true
				case cut:
					c.dsWant = strings.Split(seg, "/")
				case prefix == "**":
					c.dsAnyDir = true
				}
				g.pats = append(g.pats, c)
				continue
			}
		}
		if strings.HasPrefix(pattern, "**/") && strings.HasSuffix(pattern, "/**") {
			if seg := strings.TrimSuffix(strings.TrimPrefix(pattern, "**/"), "/**"); seg != "" && !strings.Contains(seg, "/") {
				c.segAnyDepth, c.seg = true, seg
			}
		}
		if strings.HasSuffix(pattern, "/**") {
			c.dirPrefixed, c.dirPrefix = true, strings.TrimSuffix(pattern, "/**")
		}
		if strings.HasPrefix(pattern, "**/") {
			c.starPrefixed, c.sub = true, strings.TrimPrefix(pattern, "**/")
		}
		c.tail = literalTail(pattern)
		if c.starPrefixed {
			c.subTail = literalTail(c.sub)
		}
		g.pats = append(g.pats, c)
	}
	return g
}

// Len reports how many patterns the set holds.
func (g *Set) Len() int {
	if g == nil {
		return 0
	}
	return len(g.pats)
}

// Match returns the first pattern in the set that matches relPath, which must be in
// fact-path (forward-slash) form. It is Match's answer, reached without redoing
// the shape analysis.
func (g *Set) Match(relPath string) (string, bool) {
	if g == nil || len(g.pats) == 0 {
		return "", false
	}
	// Split lazily: a set of nothing but basename globs never needs the segments, and
	// a path is far more often rejected by every pattern than matched by one.
	var segs []string
	haveSegs := false
	segments := func() []string {
		if !haveSegs {
			segs, haveSegs = strings.Split(relPath, "/"), true
		}
		return segs
	}

	for i := range g.pats {
		c := &g.pats[i]
		if c.dirScoped {
			// The basename is a suffix of the path, so a basename glob that cannot
			// end this path cannot match it — and rejecting here skips the split.
			if c.dsTail != "" && !strings.HasSuffix(relPath, c.dsTail) {
				continue
			}
			if c.matchDirScoped(relPath, segments()) {
				return c.pattern, true
			}
			continue
		}
		if c.segAnyDepth {
			for _, part := range segments() {
				if matchSegment(c.seg, part) {
					return c.pattern, true
				}
			}
		}
		if c.dirPrefixed && (relPath == c.dirPrefix || hasDirPrefix(relPath, c.dirPrefix)) {
			return c.pattern, true
		}
		// Both remaining branches call filepath.Match against a subject that ends
		// where relPath ends, so a suffix test rejects each without running it.
		if c.tail == "" || strings.HasSuffix(relPath, c.tail) {
			if m, err := factpath.Match(c.pattern, relPath); err == nil && m {
				return c.pattern, true
			}
		}
		if c.starPrefixed && (c.subTail == "" || strings.HasSuffix(relPath, c.subTail)) {
			if m, err := factpath.Match(c.sub, filepath.Base(relPath)); err == nil && m {
				return c.pattern, true
			}
			if m, err := factpath.Match(c.sub, relPath); err == nil && m {
				return c.pattern, true
			}
		}
	}
	return "", false
}

// MatchAny reports whether any pattern in the set matches relPath.
func (g *Set) MatchAny(relPath string) bool {
	_, ok := g.Match(relPath)
	return ok
}

// matchDirScoped is matchDirScopedGlob over the pre-resolved prefix.
func (c *compiledGlob) matchDirScoped(relPath string, segs []string) bool {
	if len(segs) < 2 {
		return false // no directory component, so no prefix can name an ancestor
	}
	dirSegs, base := segs[:len(segs)-1], segs[len(segs)-1]
	if m, err := factpath.Match(c.dsFile, base); err != nil || !m {
		return false
	}
	switch {
	case c.dsNoSeg:
		return false
	case len(c.dsWant) == 1:
		return slices.ContainsFunc(dirSegs, func(part string) bool {
			return matchSegment(c.dsWant[0], part)
		})
	case len(c.dsWant) > 1:
		return containsSegmentRun(dirSegs, c.dsWant)
	case c.dsAnyDir:
		return true
	}
	return hasDirPrefix(relPath, c.dsPrefix)
}

// hasDirPrefix reports whether dir names an ancestor directory of relPath. It is
// `strings.HasPrefix(relPath, dir+"/")` without building dir+"/" on every call.
func hasDirPrefix(relPath, dir string) bool {
	return len(relPath) > len(dir) && relPath[len(dir)] == '/' && relPath[:len(dir)] == dir
}

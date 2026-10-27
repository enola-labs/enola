package locate

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// maxRange caps the lines one location range may span, so a typo such as `a.ts:1-999999`
// costs a bounded walk.
const maxRange = 20000

// LocationResult is what one location resolved to.
type LocationResult struct {
	Location string `json:"location"`
	// File is the graph's name for the file, when the given path resolved to one.
	File  string `json:"file,omitempty"`
	Match string `json:"match"`
	// Symbols are the innermost symbols holding the location: one for a line, every
	// distinct one in order for a range.
	Symbols []Symbol `json:"symbols,omitempty"`
	// Candidates are the graph files a path matched when it matched more than one.
	Candidates []string `json:"candidates,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// Locations resolves each `file:line` or `file:start-end`.
func (ix *Index) Locations(locs []string) []LocationResult {
	out := make([]LocationResult, 0, len(locs))
	for _, loc := range locs {
		out = append(out, ix.location(loc))
	}
	return out
}

func (ix *Index) location(loc string) LocationResult {
	r := LocationResult{Location: loc}
	file, start, end, err := ParseLocation(loc)
	if err != nil {
		r.Match, r.Error = MatchNoFile, err.Error()
		return r
	}
	resolved, cands := ix.ResolveFile(file)
	if resolved == "" {
		r.Match, r.Candidates = MatchNoFile, cands
		return r
	}
	r.File = resolved
	r.Match = MatchNone
	seen := map[string]bool{}
	for line := start; line <= end; line++ {
		s, m := ix.At(resolved, line)
		if m == MatchNone {
			continue
		}
		r.Match = m
		if key := s.ID + "\x00" + strconv.Itoa(s.Line); !seen[key] {
			seen[key] = true
			r.Symbols = append(r.Symbols, s)
		}
	}
	return r
}

// ParseLocation splits `file:line` or `file:start-end`.
func ParseLocation(loc string) (file string, start, end int, err error) {
	i := strings.LastIndexByte(loc, ':')
	if i <= 0 {
		return "", 0, 0, fmt.Errorf("%q is not file:line", loc)
	}
	file, spec := loc[:i], loc[i+1:]
	a, b, isRange := strings.Cut(spec, "-")
	if start, err = strconv.Atoi(a); err != nil || start <= 0 {
		return "", 0, 0, fmt.Errorf("%q: %q is not a line number", loc, a)
	}
	end = start
	if isRange {
		if end, err = strconv.Atoi(b); err != nil || end < start {
			return "", 0, 0, fmt.Errorf("%q: %q is not a line range", loc, spec)
		}
	}
	if end-start >= maxRange {
		return "", 0, 0, fmt.Errorf("%q: a range is at most %d lines", loc, maxRange)
	}
	return file, start, end, nil
}

// Sides of a diff.
const (
	// SideNew reads the lines a diff added, against the graph of the tree after it: what
	// a change touched, for a snapshot taken with the change applied.
	SideNew = "new"
	// SideOld reads the lines a diff removed, against the graph of the tree before it:
	// what a change touched, for a snapshot of its parent.
	SideOld = "old"
)

// DiffResult is the symbols a diff touched.
type DiffResult struct {
	Side  string     `json:"side"`
	Files []DiffFile `json:"files"`
	// Symbols counts the distinct symbols across all files.
	Symbols int `json:"symbols"`
}

// DiffFile is one file of a diff.
type DiffFile struct {
	Path       string       `json:"path"`
	File       string       `json:"file,omitempty"`
	Match      string       `json:"match"`
	Symbols    []DiffSymbol `json:"symbols,omitempty"`
	Outside    int          `json:"outside,omitempty"`
	Candidates []string     `json:"candidates,omitempty"`
}

// DiffSymbol is a symbol and how many of the diff's lines fell in it.
type DiffSymbol struct {
	Symbol
	Lines int `json:"lines"`
}

// Diff maps a unified diff to the symbols its changes fall in, on one side.
//
// A changed line on the chosen side is credited to the symbol holding it. A run of lines
// that exists only on the other side (a pure deletion, read on the new side) has no line
// of its own here; it is credited only when the lines on both sides of where it was are
// in one symbol, because a deletion between two functions, or of a whole function, is
// not a change to either neighbor. Otherwise it is counted as Outside.
func (ix *Index) Diff(text, side string) (DiffResult, error) {
	if side == "" {
		side = SideNew
	}
	if side != SideNew && side != SideOld {
		return DiffResult{}, fmt.Errorf("side is %q or %q, not %q", SideNew, SideOld, side)
	}
	mine, theirs := byte('+'), byte('-')
	if side == SideOld {
		mine, theirs = '-', '+'
	}

	res := DiffResult{Side: side}
	distinct := map[string]bool{}
	var cur *fileAcc
	var accs []*fileAcc
	// pending is a run of the other side's lines. A run directly against one of this
	// side's lines, before or after it, is the other half of a replacement and already
	// credited through this side; only a run between context lines is a pure deletion.
	pending := 0
	afterMine := false // the run started right after one of this side's lines
	lastMine := false
	line := 0 // the next line number on this side

	settle := func(followedByMine bool) {
		if pending > 0 && cur != nil && !followedByMine && !afterMine {
			cur.between(line, pending)
		}
		pending = 0
	}

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var oldPath string
	oldLeft, newLeft := 0, 0 // lines the current hunk still holds, per side
	for sc.Scan() {
		l := sc.Text()
		if oldLeft > 0 || newLeft > 0 {
			// Inside a hunk the header counts say what each line is, so a removed line
			// that happens to read "--- x" is not mistaken for the next file.
			switch {
			case strings.HasPrefix(l, "\\"):
				continue
			case strings.HasPrefix(l, "+"):
				newLeft--
			case strings.HasPrefix(l, "-"):
				oldLeft--
			default: // context; a tool that strips trailing space leaves it empty
				oldLeft--
				newLeft--
			}
			if cur == nil {
				continue
			}
			switch {
			case l != "" && l[0] == mine:
				settle(true)
				cur.touch(line)
				line++
				lastMine = true
			case l != "" && l[0] == theirs:
				if pending == 0 {
					afterMine = lastMine
				}
				pending++
				lastMine = false
			default:
				settle(false)
				line++
				lastMine = false
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "--- "):
			settle(false)
			cur, oldPath = nil, diffPath(l[4:], "a/")
		case strings.HasPrefix(l, "+++ "):
			p := diffPath(l[4:], "b/")
			if side == SideOld {
				p = oldPath
			}
			if cur = ix.newAcc(p); cur != nil {
				accs = append(accs, cur)
			}
		case strings.HasPrefix(l, "@@ "):
			settle(false)
			lastMine = false
			h, err := parseHunk(l)
			if err != nil {
				return DiffResult{}, err
			}
			oldLeft, newLeft = h.oldCount, h.newCount
			line = h.newStart
			if side == SideOld {
				line = h.oldStart
			}
		}
	}
	settle(false)
	if err := sc.Err(); err != nil {
		return DiffResult{}, err
	}
	for _, a := range accs {
		f := a.result()
		for _, s := range f.Symbols {
			distinct[s.ID+"\x00"+strconv.Itoa(s.Line)] = true
		}
		res.Files = append(res.Files, f)
	}
	res.Symbols = len(distinct)
	return res, nil
}

// fileAcc accumulates one file of a diff.
type fileAcc struct {
	ix      *Index
	out     DiffFile
	order   []string
	symbols map[string]*DiffSymbol
}

func (ix *Index) newAcc(p string) *fileAcc {
	if p == "" {
		return nil // the file does not exist on this side
	}
	a := &fileAcc{ix: ix, out: DiffFile{Path: p}, symbols: map[string]*DiffSymbol{}}
	a.out.File, a.out.Candidates = ix.ResolveFile(p)
	if a.out.File == "" {
		a.out.Match = MatchNoFile
	}
	return a
}

func (a *fileAcc) credit(s Symbol, m string, n int) {
	if a.out.Match == "" || a.out.Match == MatchNone || m == MatchNearest {
		a.out.Match = m
	}
	key := s.ID + "\x00" + strconv.Itoa(s.Line)
	ds, ok := a.symbols[key]
	if !ok {
		ds = &DiffSymbol{Symbol: s}
		a.symbols[key] = ds
		a.order = append(a.order, key)
	}
	ds.Lines += n
}

func (a *fileAcc) touch(line int) {
	if a.out.File == "" {
		return
	}
	s, m := a.ix.At(a.out.File, line)
	if m == MatchNone {
		a.out.Outside++
		return
	}
	a.credit(s, m, 1)
}

// between credits n lines that sat between line-1 and line on this side.
func (a *fileAcc) between(line, n int) {
	if a.out.File == "" {
		return
	}
	before, mb := a.ix.At(a.out.File, line-1)
	after, ma := a.ix.At(a.out.File, line)
	if mb == MatchSpan && ma == MatchSpan && before.ID == after.ID && before.Line == after.Line {
		a.credit(before, mb, n)
		return
	}
	a.out.Outside += n
}

func (a *fileAcc) result() DiffFile {
	if a.out.Match == "" {
		a.out.Match = MatchNone
	}
	for _, k := range a.order {
		a.out.Symbols = append(a.out.Symbols, *a.symbols[k])
	}
	return a.out
}

// diffPath reads the path of a `---` or `+++` line: "" for /dev/null, the git prefix
// dropped, a trailing timestamp (diff -u) cut.
func diffPath(s, prefix string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, prefix)
}

type hunk struct{ oldStart, oldCount, newStart, newCount int }

// parseHunk reads `@@ -a,b +c,d @@`. An omitted count is 1.
func parseHunk(h string) (hunk, error) {
	bad := fmt.Errorf("malformed hunk header %q", h)
	fields := strings.Fields(h)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return hunk{}, bad
	}
	parse := func(f string) (start, count int, err error) {
		a, b, hasCount := strings.Cut(f[1:], ",")
		if start, err = strconv.Atoi(a); err != nil {
			return 0, 0, err
		}
		count = 1
		if hasCount {
			count, err = strconv.Atoi(b)
		}
		return start, count, err
	}
	var x hunk
	var err1, err2 error
	x.oldStart, x.oldCount, err1 = parse(fields[1])
	x.newStart, x.newCount, err2 = parse(fields[2])
	if err1 != nil || err2 != nil {
		return hunk{}, bad
	}
	return x, nil
}

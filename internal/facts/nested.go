package facts

import (
	"sort"
	"strconv"
	"strings"
)

// PropNested lists the named functions nested in a symbol that have no symbol of their own:
// a Go closure bound to a variable, a TypeScript local function, a Rust fn inside a fn. Each
// entry is "<name>:<line>-<end_line>". Without it a line in one of them is located as the
// symbol around it with nothing to say so, and a reader that keyed a note on the nested
// function attaches it one level too high.
//
// It is set only on symbols holding such a function, on the innermost one. Like
// PropBodyHash it says nothing about the architecture.
const PropNested = "nested"

// Nested is one named nested function without a symbol of its own.
type Nested struct {
	Name    string `json:"name"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
}

func (n Nested) String() string {
	return n.Name + ":" + strconv.Itoa(n.Line) + "-" + strconv.Itoa(n.EndLine)
}

// AttachNested records each of nested, found in file, on the innermost symbol of ff in that
// file whose span holds it. One with the lines of a symbol is that symbol and is skipped, and
// so is one no symbol holds.
func AttachNested(ff []Fact, file string, nested []Nested) {
	if len(nested) == 0 {
		return
	}
	type span struct{ line, end int }
	symbols := map[span]bool{}
	for i := range ff {
		if ff[i].Kind == KindSymbol && ff[i].File == file {
			symbols[span{ff[i].Line, ff[i].EndLine}] = true
		}
	}
	held := map[int][]string{}
	for _, n := range nested {
		if n.Name == "" || n.Line <= 0 || n.EndLine < n.Line || symbols[span{n.Line, n.EndLine}] {
			continue
		}
		best := -1
		for i := range ff {
			f := &ff[i]
			if f.Kind != KindSymbol || f.File != file || f.EndLine < f.Line || f.SpanStart() > n.Line || f.EndLine < n.EndLine {
				continue
			}
			if best < 0 || f.EndLine-f.SpanStart() < ff[best].EndLine-ff[best].SpanStart() {
				best = i
			}
		}
		if best >= 0 {
			held[best] = append(held[best], n.String())
		}
	}
	for i, list := range held {
		sort.Strings(list)
		ff[i].SetProp(PropNested, list)
	}
}

// NestedOf returns the nested functions recorded on f, in line order. A fact read back from
// JSON holds the list as []any.
func NestedOf(f *Fact) []Nested {
	var raw []string
	switch v := f.PropAny(PropNested).(type) {
	case []string:
		raw = v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	out := make([]Nested, 0, len(raw))
	for _, s := range raw {
		i := strings.LastIndexByte(s, ':')
		if i <= 0 {
			continue
		}
		a, b, ok := strings.Cut(s[i+1:], "-")
		line, err1 := strconv.Atoi(a)
		end, err2 := strconv.Atoi(b)
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		out = append(out, Nested{Name: s[:i], Line: line, EndLine: end})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

package tsextractor

import (
	"regexp"
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

// How a reachable client call is triggered.
//
// Reachability says whether the application can run a call; how often it does depends
// on what runs it. A poll on a timer in the app shell sends its request every minute
// for every open tab; an export button sends one when someone clicks it. The pass
// carries, beside whether a function is reached, the strongest trigger it is reached
// by, read from the code alone:
//
//   - poll: the function is a timer's callback (setInterval, a useInterval hook) or a
//     data hook's fetcher next to a refetch interval (react-query's refetchInterval,
//     SWR's refreshInterval). The interval is kept when the code states it: a number,
//     a product or sum of numbers, or a constant the file declares as one.
//   - mount: it runs as a page or the app loads: a page's render or effect, or
//     anything they call.
//   - action: it runs from an event handler: a JSX on* attribute or an
//     addEventListener listener.
//
// A trigger passes along a call unchanged: a function a poll calls is polled. Where it
// starts sets whether it runs on every page: the root layout, the pages router's _app,
// the app's entry file and middleware run whichever page is open; a page runs on that
// page only.

// useKind is how one name is used where it is written, ordered by how much traffic
// it passes on: an event handler passes on less than a plain call, a timer more.
type useKind uint8

const (
	useAction useKind = iota + 1
	usePlain
	usePoll
)

// useMode is a use and, for a poll, its interval in milliseconds (0 when not stated).
type useMode struct {
	kind useKind
	ms   int
}

var plainUse = useMode{kind: usePlain}

// stronger reports whether a passes on more traffic than b.
func (a useMode) stronger(b useMode) bool {
	if a.kind != b.kind {
		return a.kind > b.kind
	}
	return shorterInterval(a.ms, b.ms)
}

// rank orders what reaches a function. rankReached is reached by name only: through a
// use that need not call it, a name a parameter binds, or a name several files declare.
// It counts for reachability and says nothing about how the function is triggered.
type rank uint8

const (
	rankNone rank = iota
	rankReached
	rankAction
	rankMount
	rankPoll
)

// trigger is what reaches a function: its rank, whether it runs on every page, and a
// poll's interval.
type trigger struct {
	rank      rank
	everyPage bool
	ms        int
}

// reachedOnly is what a use passes on when it cannot carry a trigger.
func (t trigger) reachedOnly() trigger {
	if t.rank == rankNone {
		return t
	}
	return trigger{rank: rankReached}
}

// better reports whether a means more traffic than b: a higher rank, then running on
// every page, then a stated, shorter interval.
func (a trigger) better(b trigger) bool {
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	if a.everyPage != b.everyPage {
		return a.everyPage
	}
	return shorterInterval(a.ms, b.ms)
}

// shorterInterval orders poll intervals: a stated one before an unstated one, a
// shorter before a longer.
func shorterInterval(a, b int) bool {
	switch {
	case a == b:
		return false
	case b == 0:
		return true
	case a == 0:
		return false
	}
	return a < b
}

// through is the trigger a use passes on from a function reached by t.
func (t trigger) through(m useMode) trigger {
	if t.rank == rankNone {
		return t
	}
	switch m.kind {
	case useAction:
		return trigger{rank: rankAction, everyPage: t.everyPage}
	case usePoll:
		return trigger{rank: rankPoll, everyPage: t.everyPage, ms: m.ms}
	}
	return t
}

// name is the PropClientTrigger value.
func (t trigger) name() string {
	switch t.rank {
	case rankPoll:
		return "poll"
	case rankMount:
		return "mount"
	case rankAction:
		return "action"
	}
	return ""
}

// modeSpan is the lines of a region whose code runs under one use: an inline event
// handler or timer callback. A request written inside it takes that use.
type modeSpan struct {
	start, end int
	mode       useMode
}

// innermostMode is the use of the narrowest region around a line, if any.
func innermostMode(spans []modeSpan, line int) (useMode, bool) {
	var best modeSpan
	found := false
	for _, sp := range spans {
		if sp.start <= line && line <= sp.end && (!found || sp.end-sp.start < best.end-best.start) {
			best, found = sp, true
		}
	}
	return best.mode, found
}

// isEventAttribute reports a JSX attribute name for an event handler: on followed by
// an upper-case letter (onClick, onSubmit).
func isEventAttribute(name string) bool {
	return len(name) > 2 && strings.HasPrefix(name, "on") && name[2] >= 'A' && name[2] <= 'Z'
}

// timerCallees are the calls whose first argument runs on a timer.
var timerCallees = map[string]bool{
	"setInterval": true, "window.setInterval": true, "globalThis.setInterval": true, "useInterval": true,
}

// refetchKeys name a data hook's refetch interval; fetcherKeys the function it refetches with.
var (
	refetchKeys = map[string]bool{"refetchInterval": true, "refreshInterval": true}
	fetcherKeys = map[string]bool{"queryFn": true, "fetcher": true}
)

// swrCallees take (key, fetcher, options).
var swrCallees = map[string]bool{"useSWR": true, "useSWRImmutable": true}

var numericConst = regexp.MustCompile(`^[0-9_]+(\.[0-9]+)?$`)

// intervalMs evaluates an interval argument when the code states it: a number, a
// product or sum of such, in parentheses or not, or a constant the file declares as
// one. 0 when it does not.
func intervalMs(kinds *tsutil.KindTable, n *sitter.Node, src []byte, consts map[string]int) int {
	if n == nil {
		return 0
	}
	switch kindOf(kinds, n) {
	case "number":
		t := strings.ReplaceAll(nodeText(n, src), "_", "")
		if !numericConst.MatchString(t) {
			return 0
		}
		v, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0
		}
		return int(v)
	case "parenthesized_expression":
		if n.NamedChildCount() == 1 {
			return intervalMs(kinds, n.NamedChild(0), src, consts)
		}
	case "identifier":
		return consts[nodeText(n, src)]
	case "binary_expression":
		l := intervalMs(kinds, n.ChildByFieldName("left"), src, consts)
		r := intervalMs(kinds, n.ChildByFieldName("right"), src, consts)
		if l == 0 || r == 0 {
			return 0
		}
		switch op := n.ChildByFieldName("operator"); {
		case op == nil:
		case nodeText(op, src) == "*":
			return l * r
		case nodeText(op, src) == "+":
			return l + r
		}
	}
	return 0
}

// fileConsts collects the numbers a file declares at top level as constants
// (`const POLL_MS = 60 * 1000`), so an interval named by one can be stated.
func fileConsts(kinds *tsutil.KindTable, root *sitter.Node, src []byte) map[string]int {
	consts := map[string]int{}
	for i := range root.NamedChildCount() {
		stmt := root.NamedChild(i)
		if kindOf(kinds, stmt) == "export_statement" {
			if d := stmt.ChildByFieldName("declaration"); d != nil {
				stmt = d
			}
		}
		if kindOf(kinds, stmt) != "lexical_declaration" || !strings.HasPrefix(nodeText(stmt, src), "const") {
			continue
		}
		for j := range stmt.NamedChildCount() {
			d := stmt.NamedChild(j)
			if kindOf(kinds, d) != "variable_declarator" {
				continue
			}
			name, value := d.ChildByFieldName("name"), d.ChildByFieldName("value")
			if name == nil || value == nil || kindOf(kinds, name) != "identifier" {
				continue
			}
			if v := intervalMs(kinds, value, src, consts); v > 0 {
				consts[nodeText(name, src)] = v
			}
		}
	}
	return consts
}

// pairKey is an object pair's key as written.
func pairKey(kinds *tsutil.KindTable, pair *sitter.Node, src []byte) string {
	if k := pair.ChildByFieldName("key"); k != nil {
		return strings.Trim(nodeText(k, src), `"'`)
	}
	return ""
}

// refetchInterval is an object's refetch interval when it has one: (true, ms).
func refetchInterval(kinds *tsutil.KindTable, obj *sitter.Node, src []byte, consts map[string]int) (bool, int) {
	if obj == nil || kindOf(kinds, obj) != "object" {
		return false, 0
	}
	for i := range obj.NamedChildCount() {
		p := obj.NamedChild(i)
		if kindOf(kinds, p) == "pair" && refetchKeys[pairKey(kinds, p, src)] {
			return true, intervalMs(kinds, p.ChildByFieldName("value"), src, consts)
		}
	}
	return false, 0
}

// callsOrHandsOver reports a use that calls the name or hands it over to be run: a
// callee (f(), x.f()), an argument or an option value (setInterval(f), { queryFn: f }),
// a constructor, a JSX attribute value or child expression, or a rendered component.
// Only such a use carries a trigger; any other mention of a name (a type, a property
// read) only reaches it.
func callsOrHandsOver(kinds *tsutil.KindTable, n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	same := func(c *sitter.Node) bool {
		return c != nil && c.StartByte() == n.StartByte() && c.EndByte() == n.EndByte()
	}
	switch kindOf(kinds, p) {
	case "call_expression":
		return same(p.ChildByFieldName("function"))
	case "new_expression":
		return same(p.ChildByFieldName("constructor"))
	case "member_expression":
		if !same(p.ChildByFieldName("property")) {
			return false
		}
		gp := p.Parent()
		return gp != nil && kindOf(kinds, gp) == "call_expression" && func() bool {
			f := gp.ChildByFieldName("function")
			return f != nil && f.StartByte() == p.StartByte() && f.EndByte() == p.EndByte()
		}()
	case "arguments", "jsx_expression":
		return kindOf(kinds, n) == "identifier"
	case "pair":
		return kindOf(kinds, n) == "identifier" && same(p.ChildByFieldName("value"))
	case "jsx_opening_element", "jsx_self_closing_element":
		return same(p.ChildByFieldName("name"))
	}
	return false
}

// boundAsParameter reports an identifier that names a parameter of a function around
// it: zustand's get in create((set, get) => …), a Promise's resolve. Such a name is
// local to the call and names no function of the module, however many share it.
func boundAsParameter(kinds *tsutil.KindTable, n *sitter.Node, src []byte) bool {
	if kindOf(kinds, n) != "identifier" {
		return false
	}
	name := nodeText(n, src)
	for fn := enclosingFunction(kinds, n); fn != nil; fn = enclosingFunction(kinds, fn) {
		params := fn.ChildByFieldName("parameters")
		if params == nil {
			params = fn.ChildByFieldName("parameter")
		}
		if params != nil && declaresName(kinds, params, src, name) {
			return true
		}
	}
	return false
}

// declaresName reports a parameter list that binds name, directly or by destructuring.
func declaresName(kinds *tsutil.KindTable, n *sitter.Node, src []byte, name string) bool {
	switch kindOf(kinds, n) {
	case "identifier", "shorthand_property_identifier_pattern":
		return nodeText(n, src) == name
	case "type_annotation", "type_identifier":
		return false
	}
	for i := range n.NamedChildCount() {
		c := n.NamedChild(i)
		// A default value is an expression, not a binding.
		if p := n.ChildByFieldName("value"); p != nil && p.StartByte() == c.StartByte() && kindOf(kinds, n) != "pair_pattern" {
			continue
		}
		if declaresName(kinds, c, src, name) {
			return true
		}
	}
	return false
}

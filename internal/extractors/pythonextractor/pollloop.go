package pythonextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Round loops: a loop that goes round again because of what the round fetched,
// or after waiting, and not because there is a next element.
//
//	while True:                                        # a poll
//	    status = hook.get_job_status(job_id)
//	    if status in TERMINAL: break
//	    time.sleep(interval)
//
//	for attempt in range(retries):                     # a retry
//	    try: return client.fetch(url)
//	    except Timeout: time.sleep(backoff)
//
//	for attempt in retrying:                           # the same, by tenacity
//	    with attempt: client.fetch(url)
//
//	while request is not None:                         # a cursor
//	    response = request.execute()
//	    request = api.list_next(request, response)
//
// Read as a loop over data, the call in each is a query per element. But there is
// no element. A poll asks the same question until the answer changes, a retry
// makes one request until it succeeds, and a cursor fetches one page per round,
// which is the batched read. The number of rounds is set by time, by luck, or by
// the page size, and no rewrite of the loop removes a round trip.
//
// So a round loop adds no nesting, and a call at its own level is not an N+1
// candidate, as with the round of a batch loop (batchloop.go). Two things stay
// as they were. A loop INSIDE a round still walks what the round fetched, and a
// call in that one runs once per element. And a round loop inside a loop over
// data runs once per element of that loop, so its call is a candidate there, at
// that loop's depth.
//
// What keeps a walk out. A loop that takes an element off something each round
// (`item = queue.pop()`, `row = cursor.fetchone()`, `work = inbox.get()`) is a
// walk written as a while,
// and the call after it does run once per element. And a chain walk (`node =
// node.parent()`), whose next step depends on nothing but the last one, has no
// response driving it and stays a candidate.

// pyElementTakers are the calls that yield one element of what they are called
// on. A while loop that binds from one is walking a collection.
var pyElementTakers = map[string]bool{
	"pop": true, "popleft": true, "popitem": true, "fetchone": true, "readline": true,
	"next": true, "__next__": true, "anext": true, "get_nowait": true,
}

// pyTakesElement reports whether a call yields one element of its receiver: one
// of pyElementTakers, or a `get` with no positional argument, which is a queue's
// (`work = inbox.get()`, `inbox.get(timeout=1)`) where a mapping's names a key.
func pyTakesElement(call *sitter.Node, src []byte) bool {
	name := pyCalleeName(call, src)
	if pyElementTakers[name] {
		return true
	}
	if name != "get" {
		return false
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return true
	}
	for i := uint(0); i < args.NamedChildCount(); i++ {
		if kindOf(args.NamedChild(i)) != "keyword_argument" {
			return false
		}
	}
	return true
}

// pyRoundLoop reports whether a for/while statement is a round loop.
func pyRoundLoop(node *sitter.Node, src []byte) bool {
	body := node.ChildByFieldName("body")
	if body == nil {
		return false
	}
	switch kindOf(node) {
	case "while_statement":
		return pyWhileIsRound(node.ChildByFieldName("condition"), body, src)
	case "for_statement":
		return pyForIsRetry(node, body, src)
	}
	return false
}

// pyForIsRetry reports whether a for statement counts attempts: it walks a
// counter and sleeps, it sleeps for what it iterates, or its element is the
// attempt a retry library hands out to be entered.
func pyForIsRetry(node, body *sitter.Node, src []byte) bool {
	left, right := node.ChildByFieldName("left"), node.ChildByFieldName("right")
	if left == nil || right == nil || kindOf(left) != "identifier" {
		return false
	}
	loopVar := pyText(left, src)
	if pyEntersName(body, src, loopVar) {
		return true
	}
	sleeps := pyOwnSleeps(body, src)
	if len(sleeps) == 0 {
		return false
	}
	for _, s := range sleeps {
		if pyMentionsAny(s.ChildByFieldName("arguments"), src, map[string]bool{loopVar: true}) {
			return true
		}
	}
	if kindOf(right) != "call" {
		return false
	}
	switch pyCalleeName(right, src) {
	case "range", "count":
		// `range(len(items))` walks items by index.
		return !pyCallsNamed(right.ChildByFieldName("arguments"), src, "len")
	}
	return false
}

// pyWhileIsRound reports whether a while statement is a poll or a cursor.
func pyWhileIsRound(cond, body *sitter.Node, src []byte) bool {
	r := pyRoundFetches(cond, body, src)
	if r.walks {
		return false
	}
	if len(pyOwnSleeps(body, src)) > 0 {
		return true
	}
	// `while (chunk := f.read(n)):` binds what it fetched in its own test.
	if r.inCondition {
		return true
	}
	// `while request is not None:` where the body moves request on by the
	// response: the test reads a local the round derived from what it fetched.
	if cond != nil && pyMentionsAny(cond, src, r.derived) {
		return true
	}
	// `while True:` left by a test of what the round fetched. A loop with a
	// test of its own is governed by that test, and an exit from it is a walk
	// that stops early.
	return pyIsTrue(cond, src) && pyLeavesOn(body, src, r.fetched)
}

// pyIsTrue reports whether a while condition is the constant `True` or a
// non-zero integer.
func pyIsTrue(cond *sitter.Node, src []byte) bool {
	if cond == nil {
		return false
	}
	switch kindOf(cond) {
	case "true":
		return true
	case "integer":
		return pyText(cond, src) != "0"
	}
	return false
}

// pyRound is what a while loop binds each round.
type pyRound struct {
	// fetched are the locals bound from a call, or from an expression over such
	// a local, in the condition and at the level of the loop's own body.
	fetched map[string]bool
	// derived are those bound from an expression that reads ANOTHER fetched
	// local: `request = api.list_next(request, response)`. A local moved on by
	// nothing but itself (`node = parent(node)`) is a chain's cursor, and no
	// response drives it.
	derived map[string]bool
	// inCondition is set when the condition itself binds from a call.
	inCondition bool
	// walks is set when a local is bound by taking an element of something.
	walks bool
}

// pyRoundFetches reads the bindings of one round of a while loop.
func pyRoundFetches(cond, body *sitter.Node, src []byte) pyRound {
	r := pyRound{fetched: make(map[string]bool), derived: make(map[string]bool)}
	inCond := false
	bind := func(target, value *sitter.Node) {
		if target == nil || value == nil || kindOf(target) != "identifier" {
			return
		}
		name := pyText(target, src)
		value = pyUnwrapValue(value)
		if value == nil {
			return
		}
		isCall := kindOf(value) == "call"
		if isCall && pyTakesElement(value, src) && !r.fetched[pyRootName(value, src)] {
			r.walks = true
			return
		}
		others := make(map[string]bool, len(r.fetched))
		for f := range r.fetched {
			if f != name {
				others[f] = true
			}
		}
		readsOther := pyMentionsAny(value, src, others)
		if readsOther {
			r.derived[name] = true
		}
		if isCall || readsOther {
			r.fetched[name] = true
		}
		if isCall && inCond {
			r.inCondition = true
		}
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "for_statement", "while_statement", "function_definition", "class_definition", "lambda":
			return
		case "assignment":
			bind(n.ChildByFieldName("left"), n.ChildByFieldName("right"))
		case "named_expression":
			bind(n.ChildByFieldName("name"), n.ChildByFieldName("value"))
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	inCond = true
	walk(cond)
	inCond = false
	// Twice, so a local derived from one that is bound further down is seen.
	walk(body)
	walk(body)
	return r
}

// pyLeavesOn reports whether the loop's own body has an `if` that tests one of
// names and whose block leaves the loop.
func pyLeavesOn(n *sitter.Node, src []byte, names map[string]bool) bool {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		switch kindOf(c) {
		case "for_statement", "while_statement", "function_definition", "class_definition", "decorated_definition":
			continue
		case "if_statement":
			if pyMentionsAny(c.ChildByFieldName("condition"), src, names) && pyBlockLeaves(c.ChildByFieldName("consequence")) {
				return true
			}
		}
		if pyLeavesOn(c, src, names) {
			return true
		}
	}
	return false
}

// pyBlockLeaves reports whether a block has, among its own statements, a break,
// a return or a raise.
func pyBlockLeaves(block *sitter.Node) bool {
	if block == nil {
		return false
	}
	for i := uint(0); i < block.NamedChildCount(); i++ {
		switch kindOf(block.NamedChild(i)) {
		case "break_statement", "return_statement", "raise_statement":
			return true
		}
	}
	return false
}

// pyOwnSleeps returns the calls to a `sleep` in a loop body, outside any loop or
// definition nested in it.
func pyOwnSleeps(n *sitter.Node, src []byte) []*sitter.Node {
	var out []*sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "for_statement", "while_statement", "function_definition", "class_definition", "lambda":
			return
		case "call":
			if pyCalleeName(n, src) == "sleep" {
				out = append(out, n)
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(n)
	return out
}

// pyEntersName reports whether a loop body has, among its own statements, a
// `with name:` (or `async with`).
func pyEntersName(body *sitter.Node, src []byte, name string) bool {
	for i := uint(0); i < body.NamedChildCount(); i++ {
		st := body.NamedChild(i)
		if kindOf(st) != "with_statement" {
			continue
		}
		found := false
		var walk func(n *sitter.Node)
		walk = func(n *sitter.Node) {
			if n == nil || found || kindOf(n) == "block" {
				return
			}
			if kindOf(n) == "with_item" {
				if v := n.ChildByFieldName("value"); v != nil && kindOf(v) == "identifier" && pyText(v, src) == name {
					found = true
				}
				return
			}
			for i := uint(0); i < n.NamedChildCount(); i++ {
				walk(n.NamedChild(i))
			}
		}
		walk(st)
		if found {
			return true
		}
	}
	return false
}

// pyCalleeName returns the last name of what a call expression calls: `sleep` for
// `time.sleep(1)` and for `await asyncio.sleep(1)`'s call.
func pyCalleeName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch kindOf(fn) {
	case "identifier":
		return pyText(fn, src)
	case "attribute":
		if attr := fn.ChildByFieldName("attribute"); attr != nil {
			return pyText(attr, src)
		}
	}
	return ""
}

// pyMentionsAny reports whether an expression reads one of names.
func pyMentionsAny(n *sitter.Node, src []byte, names map[string]bool) bool {
	if n == nil || len(names) == 0 {
		return false
	}
	switch kindOf(n) {
	case "identifier":
		return names[pyText(n, src)]
	case "attribute":
		// `x.y` reads x; y is a member name, not a variable.
		return pyMentionsAny(n.ChildByFieldName("object"), src, names)
	case "keyword_argument":
		return pyMentionsAny(n.ChildByFieldName("value"), src, names)
	case "lambda", "function_definition":
		return false
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if pyMentionsAny(n.NamedChild(i), src, names) {
			return true
		}
	}
	return false
}

// pyCallsNamed reports whether an expression contains a call to name.
func pyCallsNamed(n *sitter.Node, src []byte, name string) bool {
	if n == nil {
		return false
	}
	if kindOf(n) == "call" && pyCalleeName(n, src) == name {
		return true
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if pyCallsNamed(n.NamedChild(i), src, name) {
			return true
		}
	}
	return false
}

package pythonextractor

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// A call the loop does not survive.
//
//	for table in tables:
//	    if len(tables) == 1:
//	        obj = hook.get_object(table["id"])   # at most once: the loop ends
//	        break
//
// The call sits in a loop and runs at most once each time the loop is entered,
// because the block it is in ends by leaving the loop. Counted as a call per
// iteration it read as an N+1 on the found-it branch of every search loop.
//
// It is goextractor/terminal.go's rule. A block leaves the loop when its last
// statement is a break, a return or a raise. Only the innermost loop is
// considered: a return leaves the outer loops as well, and that is not claimed.
// And nothing in the block may go round again first: a body that ends in a
// return after a `continue` is the "first match" loop, whose opening call runs on
// every element that does not match. The body of a `try` goes round again by
// raising, and is never such a block.

// pySpan is a half-open range of source bytes.
type pySpan struct{ start, end uint }

// pyTerminalBlocks returns the blocks within a loop body that end by leaving the
// loop. Nested loops and definitions are not entered: a nested loop has blocks of
// its own, and a definition runs when it is called.
func pyTerminalBlocks(body *sitter.Node) []pySpan {
	var out []pySpan
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "for_statement", "while_statement", "function_definition", "class_definition", "lambda",
			"list_comprehension", "set_comprehension", "dictionary_comprehension", "generator_expression":
			return
		case "try_statement":
			// The body of a try is not such a block, whatever it ends with. A call
			// in it may raise, the handler may go round, and the next element is
			// tried: `for host in hosts: try: c = connect(host); return c; except
			// OSError: pass` connects once per host until one answers.
			for i := uint(0); i < n.NamedChildCount(); i++ {
				c := n.NamedChild(i)
				if kindOf(c) == "block" {
					for j := uint(0); j < c.NamedChildCount(); j++ {
						walk(c.NamedChild(j))
					}
					continue
				}
				walk(c)
			}
			return
		case "block":
			if count := n.NamedChildCount(); count > 0 {
				last := n.NamedChild(count - 1)
				switch kindOf(last) {
				case "break_statement", "return_statement", "raise_statement":
					if !pyContinues(n) {
						out = append(out, pySpan{n.StartByte(), n.EndByte()})
					}
				}
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	return out
}

// pyContinues reports whether a block has a `continue` of the loop it is in.
func pyContinues(n *sitter.Node) bool {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		switch kindOf(c) {
		case "continue_statement":
			return true
		case "for_statement", "while_statement", "function_definition", "class_definition", "lambda":
			continue
		}
		if pyContinues(c) {
			return true
		}
	}
	return false
}

// holdsTerminal reports whether a call starting at pos sits in a block of the
// loop that ends by leaving it.
func (s *pyLoopScope) holdsTerminal(pos uint) bool {
	for _, t := range s.terminal {
		if pos >= t.start && pos < t.end {
			return true
		}
	}
	return false
}

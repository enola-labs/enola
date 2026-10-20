package dotnetextractor

import "bytes"

// activeBranches returns src as the compiler would parse it under one build
// configuration: of every #if/#elif/#else chain only the first branch is kept, and
// the conditional directive lines themselves are blanked.
//
// tree-sitter accepts a preprocessor directive only in certain syntactic positions,
// and C# puts one anywhere: inside a base list, around a member's attributes, between
// an expression-bodied member's `=>` and its two alternative bodies. With every branch
// left in place those files are not valid C# at all, the parse turns into one ERROR
// node and the file loses its symbols. The compiler never sees that text: it evaluates
// the directives first and parses one configuration. This does the same, with one
// fixed rule instead of a set of defined symbols, so no target framework is chosen
// on anyone's behalf and the result does not depend on the build.
//
// The first branch is the configuration because it is the one a reader sees first and
// the one a `#if !NET` polyfill or a `#if NET` fast path writes as the main line; the
// only exception is a condition that is literally `false`, which the compiler would
// never take. Code that exists only in a later branch is not seen.
//
// Every removed byte becomes a space and every line break stays, so the result has the
// same length, lines and columns as src and node positions need no translation.
// Directives are recognised where the compiler recognises them: at the start of a line,
// outside block comments and multi-line verbatim or raw strings. Inside a skipped
// section only directive lines are read, as the compiler does.
func activeBranches(src []byte) []byte {
	if !bytes.Contains(src, []byte("#if")) {
		return src
	}
	out := make([]byte, len(src))
	copy(out, src)

	type chain struct {
		parent bool // the enclosing section is kept
		taken  bool // a branch of this chain has been kept, or none may be
	}
	var stack []chain
	active := true
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			if out[i] != '\n' && out[i] != '\r' {
				out[i] = ' '
			}
		}
	}

	state := lexCode
	rawQuotes := 0
	lineStart := true

	for i := 0; i < len(src); {
		c := src[i]
		if c == '\n' {
			lineStart = true
			i++
			continue
		}
		if lineStart && (state == lexCode || !active) && (c == ' ' || c == '\t') {
			i++
			continue
		}
		if lineStart && (state == lexCode || !active) && c == '#' {
			end := bytes.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src)
			} else {
				end += i
			}
			kw, cond := directive(src[i+1 : end])
			switch kw {
			case "if":
				f := chain{parent: active}
				active = f.parent && !isFalse(cond)
				f.taken = active || !f.parent
				stack = append(stack, f)
				blank(i, end)
			case "elif", "else":
				if len(stack) == 0 {
					break
				}
				f := &stack[len(stack)-1]
				active = !f.taken && (kw == "else" || !isFalse(cond))
				f.taken = f.taken || active
				blank(i, end)
			case "endif":
				if len(stack) == 0 {
					break
				}
				active = stack[len(stack)-1].parent
				stack = stack[:len(stack)-1]
				blank(i, end)
			default:
				if !active {
					blank(i, end)
				}
			}
			i = end
			continue
		}
		lineStart = false

		if !active {
			end := bytes.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src)
			} else {
				end += i
			}
			blank(i, end)
			i = end
			continue
		}

		switch state {
		case lexBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state = lexCode
				i += 2
				continue
			}
			i++
		case lexVerbatimString:
			if c == '"' {
				if i+1 < len(src) && src[i+1] == '"' {
					i += 2
					continue
				}
				state = lexCode
			}
			i++
		case lexRawString:
			if n := quoteRun(src, i); n >= rawQuotes {
				state = lexCode
				i += rawQuotes
				continue
			}
			i++
		default:
			i = scanCode(src, i, &state, &rawQuotes)
		}
	}
	return out
}

// Lexer states activeBranches tracks between lines: only a block comment and a
// verbatim or raw string can carry a line break, and a `#` at the start of a line
// inside one of them is not a directive.
const (
	lexCode = iota
	lexBlockComment
	lexVerbatimString
	lexRawString
)

// scanCode advances past one token-sized piece of ordinary code starting at i,
// entering a multi-line state when one opens. A line comment and a single-line
// string or character literal are skipped whole, so a `/*` or `"` inside them
// opens nothing.
func scanCode(src []byte, i int, state *int, rawQuotes *int) int {
	c := src[i]
	switch {
	case c == '/' && i+1 < len(src) && src[i+1] == '/':
		if end := bytes.IndexByte(src[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(src)
	case c == '/' && i+1 < len(src) && src[i+1] == '*':
		*state = lexBlockComment
		return i + 2
	case c == '@' || c == '$':
		// A verbatim (`@"`, `$@"`, `@$"`) or raw (`$"""`) string prefix.
		j := i
		verbatim := false
		for j < len(src) && (src[j] == '@' || src[j] == '$') {
			verbatim = verbatim || src[j] == '@'
			j++
		}
		if j < len(src) && src[j] == '"' {
			if n := quoteRun(src, j); n >= 3 {
				*state, *rawQuotes = lexRawString, n
				return j + n
			}
			if verbatim {
				*state = lexVerbatimString
				return j + 1
			}
			return skipString(src, j, '"')
		}
		return i + 1
	case c == '"':
		if n := quoteRun(src, i); n >= 3 {
			*state, *rawQuotes = lexRawString, n
			return i + n
		}
		return skipString(src, i, '"')
	case c == '\'':
		return skipString(src, i, '\'')
	}
	return i + 1
}

// skipString returns the index just past a single-line literal opened at i by
// quote, honouring backslash escapes. It stops at the end of the line, where an
// unterminated literal ends too.
func skipString(src []byte, i int, quote byte) int {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case quote:
			return j + 1
		case '\n':
			return j
		}
	}
	return len(src)
}

// quoteRun counts the consecutive double quotes starting at i.
func quoteRun(src []byte, i int) int {
	n := 0
	for i+n < len(src) && src[i+n] == '"' {
		n++
	}
	return n
}

// directive splits the text after a line's `#` into its keyword and the rest,
// for the four conditional directives; any other directive returns "".
func directive(line []byte) (string, string) {
	line = bytes.TrimLeft(line, " \t")
	end := 0
	for end < len(line) && (line[end] >= 'a' && line[end] <= 'z') {
		end++
	}
	switch kw := string(line[:end]); kw {
	case "if", "elif", "else", "endif":
		return kw, string(bytes.TrimSpace(stripLineComment(line[end:])))
	}
	return "", ""
}

func stripLineComment(b []byte) []byte {
	if i := bytes.Index(b, []byte("//")); i >= 0 {
		return b[:i]
	}
	return b
}

// isFalse reports whether a condition is the literal `false`, the one condition the
// compiler is certain to skip.
func isFalse(cond string) bool {
	return cond == "false" || cond == "(false)"
}

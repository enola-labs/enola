package kotlinextractor

import (
	"bytes"
	"regexp"
)

// The grammar reads the line after a property declaration as part of it in two
// shapes it gets wrong, and loses the enclosing function or class to one ERROR node:
//
//	val svc: Svc by inject()
//	get("/path") { svc.load() }      // a call, read as the property's getter
//
//	val state: StateFlow<Boolean>
//	    field = MutableStateFlow(false)  // a Kotlin 2 explicit backing field
//
// An accessor's parameter list is `get()` or `set(value[: T])`, so a get( or set(
// call with any other arguments cannot be one; writing `;` over the blank before it
// keeps its meaning. The `field` keyword of a backing field is blanked, so its
// initializer reads as the property's own and its calls stay with the class. Both
// rewrites keep every byte offset, like separateSameLineCloses.

// propertyHeader is a property declaration line with a type and nothing after it:
// no initializer, no delegate, no accessor.
var propertyHeader = regexp.MustCompile(`^\s*(?:(?:@[\w.]+(?:\([^)\n]*\))?|private|protected|internal|public|override|open|final|abstract|lateinit|actual|expect)\s+)*(?:val|var)\s+[\w.<>]+\s*:[^=\n]*$`)

// separatePropertyTails writes the rewrites above into a copy of src, outside
// strings and comments.
func separatePropertyTails(src []byte) ([]byte, bool) {
	out := make([]byte, len(src))
	copy(out, src)
	changed := false
	var last byte // the last significant byte outside comments
	prevLine := []byte(nil)
	lineStart := 0
	for i := 0; i < len(src); i++ {
		if i == lineStart {
			if applyPropertyTail(src, out, i, last, prevLine) {
				changed = true
			}
		}
		switch {
		case src[i] == '\n':
			if line := bytes.TrimSpace(src[lineStart:i]); len(line) > 0 {
				prevLine = src[lineStart:i]
			}
			lineStart = i + 1
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			for i+1 < len(src) && src[i+1] != '\n' {
				i++
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
				i++
			}
			i++
			prevLine = nil
		case src[i] == '"' && i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"':
			i += 3
			for i+2 < len(src) && (src[i] != '"' || src[i+1] != '"' || src[i+2] != '"') {
				i++
			}
			i += 2
			last = '"'
			prevLine = nil
		case src[i] == '"' || src[i] == '\'':
			q := src[i]
			for i++; i < len(src) && src[i] != q && src[i] != '\n'; i++ {
				if src[i] == '\\' {
					i++
				}
			}
			last = q
		case src[i] == '>' && i > 0 && src[i-1] == '-':
			last = '-'
		case src[i] != ' ' && src[i] != '\t' && src[i] != '\r':
			last = src[i]
		}
	}
	return out, changed
}

// applyPropertyTail rewrites the line starting at start when it is a get( or set(
// call after a finished statement, or a backing field after a property header.
func applyPropertyTail(src, out []byte, start int, last byte, prevLine []byte) bool {
	j := start
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	rest := src[j:]
	switch {
	case bytes.HasPrefix(rest, []byte("get(")) || bytes.HasPrefix(rest, []byte("set(")):
		if j == start || !endsStatement(last) || accessorArgs(rest[:3], rest[4:]) {
			return false
		}
		out[j-1] = ';'
		return true
	case bytes.HasPrefix(rest, []byte("field")):
		k := 5
		for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t') {
			k++
		}
		if k == 5 || k >= len(rest) || rest[k] != '=' || (k+1 < len(rest) && rest[k+1] == '=') {
			return false
		}
		if prevLine == nil || !propertyHeader.Match(prevLine) || bytes.Contains(prevLine, []byte(" by ")) {
			return false
		}
		copy(out[j:j+5], "     ")
		return true
	}
	return false
}

// endsStatement reports whether a line ending in b is a finished expression, so a
// line after it starts a new statement.
func endsStatement(b byte) bool {
	switch {
	case b == '_', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return bytes.IndexByte([]byte(")]}\"'`?!>"), b) >= 0
}

// accessorArgs reports whether the arguments after get( or set( could be an
// accessor's parameter list: empty for get, one parameter name for set.
func accessorArgs(name, args []byte) bool {
	args = bytes.TrimLeft(args, " \t")
	if string(name) == "get" {
		return len(args) > 0 && args[0] == ')'
	}
	k := 0
	for k < len(args) && (args[k] == '_' || args[k] >= 'a' && args[k] <= 'z' || args[k] >= 'A' && args[k] <= 'Z' || k > 0 && args[k] >= '0' && args[k] <= '9') {
		k++
	}
	if k == 0 {
		return false
	}
	args = bytes.TrimLeft(args[k:], " \t")
	return len(args) > 0 && (args[0] == ')' || args[0] == ':')
}

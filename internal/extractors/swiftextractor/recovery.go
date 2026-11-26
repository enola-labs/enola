package swiftextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// The vendored grammar predates three newer Swift forms, and each can cost a whole
// declaration rather than one line:
//
//   - the `sending` type specifier (`init(reader: consuming sending Reader)`,
//     `{ state -> sending Checkout in ... }`);
//   - the `consume` and `copy` operators (`switch consume checkout { ... }`);
//   - a freestanding macro in a type body (`#Middleware(using: ...) { func me() }`):
//     the grammar closes the type at the `#`, so the members after it surface at file
//     scope and the type has no symbol;
//   - `try await` (or `try?`, `try!`) in an `if` condition
//     (`if let r = try? await p.load(d) {`): the grammar takes the body for a
//     trailing closure and the whole file becomes one error, so its types have no
//     symbols. `await` after a `try` is blanked; the call stays.
//
// A file that does not parse is parsed again with those forms blanked out. Blanking
// writes spaces over the bytes and keeps every newline, so every byte offset and line
// stays where it was: the walk reads names, spans and hashes from the original source.
// A macro's trailing closure loses its braces only, so the declarations inside it
// read as members of the type, which is what the macro expands them to. A rewrite is
// kept only when its parse has fewer errors (the same rule as the Kotlin extractor).

// recoveryRewrites are the byte-preserving rewrites tried, in order, on a file that
// does not parse.
var recoveryRewrites = []func([]byte) ([]byte, bool){blankOwnershipSpecifiers, blankTypeBodyMacros, blankAwaitAfterTry}

// parseRecovered parses src and, when the tree has errors, returns the best of it
// and the parses of src with each recovery rewrite written in, each applied on top
// of the last one kept. The trees not returned are closed. It returns nil when the
// parser returns nil.
func parseRecovered(parser *sitter.Parser, src []byte) *sitter.Tree {
	tree := parser.Parse(src, nil)
	if tree == nil || !tree.RootNode().HasError() {
		return tree
	}
	cur := src
	best := errorNodes(tree.RootNode())
	for _, rewrite := range recoveryRewrites {
		fixed, changed := rewrite(cur)
		if !changed {
			continue
		}
		again := parser.Parse(fixed, nil)
		if again == nil {
			continue
		}
		if n := errorNodes(again.RootNode()); n < best {
			tree.Close()
			tree, cur, best = again, fixed, n
			if n == 0 {
				break
			}
			continue
		}
		again.Close()
	}
	return tree
}

// errorNodes counts the error and missing nodes under n.
func errorNodes(n *sitter.Node) int {
	if !n.HasError() {
		return 0
	}
	count := 0
	if n.IsError() || n.IsMissing() {
		count++
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		count += errorNodes(n.Child(i))
	}
	return count
}

// codeMask marks the bytes of src that are code: outside comments and string
// literals. String interpolations (`\(...)`) are code.
func codeMask(src []byte) []bool {
	m := make([]bool, len(src))
	scanCode(src, m, 0, false)
	return m
}

// scanCode marks code from i on and returns where it stopped: at the `)` closing an
// interpolation when inParen, else at the end of src.
func scanCode(src []byte, m []bool, i int, inParen bool) int {
	depth := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			nest := 0
			for i < len(src) {
				if src[i] == '/' && i+1 < len(src) && src[i+1] == '*' {
					nest++
					i += 2
					continue
				}
				if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
					nest--
					i += 2
					if nest == 0 {
						break
					}
					continue
				}
				i++
			}
		case c == '"' || (c == '#' && rawStringStart(src, i)):
			i = scanString(src, m, i)
		default:
			if inParen {
				switch c {
				case '(':
					depth++
				case ')':
					if depth == 0 {
						return i
					}
					depth--
				}
			}
			m[i] = true
			i++
		}
	}
	return i
}

// rawStringStart reports whether src[i] opens a raw string literal (`#"`, `##"`).
func rawStringStart(src []byte, i int) bool {
	for i < len(src) && src[i] == '#' {
		i++
	}
	return i < len(src) && src[i] == '"'
}

// scanString skips the string literal opening at i and returns the index after it.
// Interpolated code inside it is marked as code.
func scanString(src []byte, m []bool, i int) int {
	hashes := 0
	for i < len(src) && src[i] == '#' {
		hashes++
		i++
	}
	quotes := 1
	if i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"' {
		quotes = 3
	}
	i += quotes
	for i < len(src) {
		c := src[i]
		if c == '\n' && quotes == 1 {
			return i
		}
		if c == '\\' && hasHashes(src, i+1, hashes) {
			j := i + 1 + hashes
			if j < len(src) && src[j] == '(' {
				end := scanCode(src, m, j+1, true)
				i = end + 1
				continue
			}
			i = j + 1
			continue
		}
		if c == '"' && (quotes == 1 || (i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"')) &&
			hasHashes(src, i+quotes, hashes) {
			return i + quotes + hashes
		}
		i++
	}
	return i
}

func hasHashes(src []byte, i, n int) bool {
	for k := 0; k < n; k++ {
		if i+k >= len(src) || src[i+k] != '#' {
			return false
		}
	}
	return true
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentByte(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// wordAt returns the identifier starting at i in code, or "".
func wordAt(src []byte, m []bool, i int) string {
	if i >= len(src) || !m[i] || !isIdentStart(src[i]) {
		return ""
	}
	j := i
	for j < len(src) && m[j] && isIdentByte(src[j]) {
		j++
	}
	return string(src[i:j])
}

// prevToken returns the code token that ends before i, skipping whitespace and
// non-code: an identifier, `->`, or a single punctuation byte.
func prevToken(src []byte, m []bool, i int) string {
	j := i - 1
	for j >= 0 && (!m[j] || src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
		j--
	}
	if j < 0 {
		return ""
	}
	if isIdentByte(src[j]) {
		k := j
		for k > 0 && m[k-1] && isIdentByte(src[k-1]) {
			k--
		}
		return string(src[k : j+1])
	}
	if src[j] == '>' && j > 0 && src[j-1] == '-' {
		return "->"
	}
	return string(src[j])
}

// nextWordAfterBlank returns the identifier after the horizontal whitespace that
// follows i, the byte there when it is not an identifier, and false when no
// horizontal whitespace follows i or the line ends first.
func nextWordAfterBlank(src []byte, m []bool, i int) (string, byte, bool) {
	if i >= len(src) || (src[i] != ' ' && src[i] != '\t') {
		return "", 0, false
	}
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	if i >= len(src) || !m[i] || src[i] == '\n' || src[i] == '\r' {
		return "", 0, false
	}
	return wordAt(src, m, i), src[i], true
}

func blank(out []byte, from, to int) {
	for k := from; k < to; k++ {
		if out[k] != '\n' && out[k] != '\r' {
			out[k] = ' '
		}
	}
}

// notAnOperand are the words after which a `sending`, `consume` or `copy` is a
// variable of that name, not a specifier or operator.
var notAnOperand = map[string]bool{"as": true, "is": true, "in": true, "where": true, "else": true}

// sendingAfter are the tokens that precede a type, so a `sending` after one is the
// type specifier.
var sendingAfter = map[string]bool{":": true, "->": true, "(": true, ",": true,
	"consuming": true, "borrowing": true, "inout": true, "__owned": true, "__shared": true}

// consumeAfter are the tokens that precede an expression, so a `consume` or `copy`
// after one is the operator.
var consumeAfter = map[string]bool{"switch": true, "=": true, "(": true, ",": true, "[": true,
	":": true, "{": true, "return": true, "in": true, "try": true, "await": true}

// blankOwnershipSpecifiers blanks the `sending` type specifier and the `consume` and
// `copy` operators the grammar does not know. The type or operand after each stays.
func blankOwnershipSpecifiers(src []byte) ([]byte, bool) {
	m := codeMask(src)
	out := make([]byte, len(src))
	copy(out, src)
	changed := false
	for i := 0; i < len(src); i++ {
		if !m[i] || !isIdentStart(src[i]) || (i > 0 && m[i-1] && (isIdentByte(src[i-1]) || src[i-1] == '.' || src[i-1] == '$' || src[i-1] == '@' || src[i-1] == '#' || src[i-1] == '`')) {
			continue
		}
		w := wordAt(src, m, i)
		end := i + len(w)
		switch w {
		case "sending":
			next, b, ok := nextWordAfterBlank(src, m, end)
			if ok && !notAnOperand[next] && (next != "" || b == '(' || b == '[' || b == '@') && sendingAfter[prevToken(src, m, i)] {
				blank(out, i, end)
				changed = true
			}
		case "consume", "copy":
			next, _, ok := nextWordAfterBlank(src, m, end)
			if ok && next != "" && !notAnOperand[next] && consumeAfter[prevToken(src, m, i)] {
				blank(out, i, end)
				changed = true
			}
		}
		i = end - 1
	}
	return out, changed
}

// typeKeywords open a type body.
var typeKeywords = map[string]bool{"struct": true, "class": true, "enum": true, "actor": true,
	"extension": true, "protocol": true}

// notATypeName are the words that follow `class` (or `static`-like uses) in a member
// declaration rather than a type name.
var notATypeName = map[string]bool{"func": true, "var": true, "let": true, "subscript": true,
	"init": true, "deinit": true, "override": true, "final": true, "static": true,
	"typealias": true, "required": true, "convenience": true}

// compilerDirectives are the `#` words the grammar reads in a type body.
var compilerDirectives = map[string]bool{"if": true, "elseif": true, "else": true, "endif": true,
	"sourceLocation": true}

// opensTypeBody reports whether the code between from and the `{` at to declares a
// type: a type keyword, not after `.`, followed by a type name.
func opensTypeBody(src []byte, m []bool, from, to int) bool {
	for i := from; i < to; i++ {
		if !m[i] || !isIdentStart(src[i]) || (i > 0 && m[i-1] && (isIdentByte(src[i-1]) || src[i-1] == '.' || src[i-1] == '@' || src[i-1] == '#')) {
			continue
		}
		w := wordAt(src, m, i)
		end := i + len(w)
		if typeKeywords[w] {
			j := end
			for j < to && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if name := wordAt(src, m, j); name != "" && !notATypeName[name] {
				return true
			}
			if j < to && src[j] == '`' {
				return true
			}
		}
		i = end - 1
	}
	return false
}

// skipBalanced returns the index after the bracket group opening at i (`(` or `<`),
// counting code bytes only, or i when src[i] is not open.
func skipBalanced(src []byte, m []bool, i int, open, close byte) int {
	if i >= len(src) || !m[i] || src[i] != open {
		return i
	}
	depth := 0
	for j := i; j < len(src); j++ {
		if !m[j] {
			continue
		}
		switch src[j] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return i
}

// blankTypeBodyMacros blanks each freestanding macro that stands as a member of a
// type body: its `#name`, generic arguments and arguments, and the braces of its
// trailing closure, whose declarations then read as members of the type.
func blankTypeBodyMacros(src []byte) ([]byte, bool) {
	m := codeMask(src)
	out := make([]byte, len(src))
	copy(out, src)
	changed := false

	const (
		frameOther = iota
		frameType
		frameMacro
	)
	var stack []int
	inTypeBody := func() bool {
		return len(stack) > 0 && stack[len(stack)-1] != frameOther
	}
	stmtStart := 0
	atMemberStart := func(i int) bool {
		t := prevToken(src, m, i)
		if t == "{" || t == "}" || t == ";" || t == "" {
			return true
		}
		// The first code on its line.
		for j := i - 1; j >= 0; j-- {
			if src[j] == '\n' {
				return true
			}
			if m[j] && src[j] != ' ' && src[j] != '\t' {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(src); i++ {
		if !m[i] {
			continue
		}
		switch src[i] {
		case '{':
			kind := frameOther
			if opensTypeBody(src, m, stmtStart, i) {
				kind = frameType
			}
			stack = append(stack, kind)
			stmtStart = i + 1
		case '}':
			if len(stack) > 0 {
				if stack[len(stack)-1] == frameMacro {
					blank(out, i, i+1)
				}
				stack = stack[:len(stack)-1]
			}
			stmtStart = i + 1
		case ';':
			stmtStart = i + 1
		case '#':
			name := wordAt(src, m, i+1)
			if name == "" || compilerDirectives[name] || !inTypeBody() || !atMemberStart(i) {
				continue
			}
			end := i + 1 + len(name)
			end = skipBalanced(src, m, end, '<', '>')
			end = skipBalanced(src, m, end, '(', ')')
			blank(out, i, end)
			changed = true
			j := end
			for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
				j++
			}
			if j < len(src) && m[j] && src[j] == '{' {
				blank(out, j, j+1)
				stack = append(stack, frameMacro)
				stmtStart = j + 1
				i = j
				continue
			}
			stmtStart = end
			i = end - 1
		}
	}
	return out, changed
}

// blankAwaitAfterTry blanks an `await` that follows `try`, `try?` or `try!`, except
// in `for try await`, a loop form the grammar reads.
func blankAwaitAfterTry(src []byte) ([]byte, bool) {
	m := codeMask(src)
	out := make([]byte, len(src))
	copy(out, src)
	changed := false
	for i := 0; i < len(src); i++ {
		if !m[i] || !isIdentStart(src[i]) || (i > 0 && m[i-1] && isIdentByte(src[i-1])) {
			continue
		}
		w := wordAt(src, m, i)
		end := i + len(w)
		if w == "await" {
			// The `try` before it, through a `?` or `!`.
			k := lastCodeByte(src, m, i)
			if k >= 0 && (src[k] == '?' || src[k] == '!') {
				k = lastCodeByte(src, m, k)
			}
			if k >= 2 && string(src[k-2:k+1]) == "try" && wordAt(src, m, k-2) == "try" &&
				(k < 3 || !isIdentByte(src[k-3])) && prevToken(src, m, k-2) != "for" {
				blank(out, i, end)
				changed = true
			}
		}
		i = end - 1
	}
	return out, changed
}

// lastCodeByte returns the index of the last code byte before i that is not
// whitespace, or -1.
func lastCodeByte(src []byte, m []bool, i int) int {
	for j := i - 1; j >= 0; j-- {
		if m[j] && src[j] != ' ' && src[j] != '\t' && src[j] != '\n' && src[j] != '\r' {
			return j
		}
	}
	return -1
}

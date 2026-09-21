package cppextractor

import (
	"bytes"
	"strings"
)

// This file implements a small, self-contained C macro expander used purely to
// recover function REFERENCES that the preprocessor would synthesize — most
// importantly the token-pasted callbacks of attribute macros such as
// CONFIGFS_ATTR(pfx, name) -> { .show = pfx##name##_show, ... }. It expands a
// macro invocation using #define definitions collected from the analyzed repo
// (no compiler, no include resolution, no CONFIG_* evaluation), then the caller
// scans the expanded text for function names. It is deliberately conservative:
// the funcNames filter in resolveFuncPtrRefs drops any expanded identifier that
// is not a real function, so an imperfect expansion can only miss an edge, never
// fabricate a false reference.

const (
	maxExpandDepth  = 32    // hard recursion cap (with hidesets) to guarantee termination
	maxExpandTokens = 16384 // bail out if one invocation expands beyond this (pathological macros)
)

// macroDef is one #define. params is nil for an object-like macro and non-nil
// (possibly empty) for a function-like one. variadic is true when the last
// parameter is `...` (its name is then the variadicName, default __VA_ARGS__).
type macroDef struct {
	params       []string
	variadic     bool
	variadicName string
	// body is the macro's replacement text, UNLEXED. It is lexed on demand by
	// substitute, once per expansion.
	//
	// Lexing eagerly at collect time was the single largest live allocation in a
	// kernel-sized snapshot: 671 MB, 22% of peak live. A token is a kind plus a
	// string header — 24 bytes for a few characters of text — and 4,000 kernel
	// sources alone produce 755,029 of them across 92,841 macros, held for the whole
	// AST pass because any file may invoke a macro defined in any header.
	//
	// It also lexed work nobody wanted. The table exists to recover token-pasted
	// callbacks from a handful of attribute macros; the overwhelming majority of
	// definitions in a kernel are never expanded at all, and their tokens were built
	// and retained regardless.
	body string
}

type macroTable map[string]macroDef

type tokKind int

const (
	tkIdent tokKind = iota
	tkPunct
	tkSpace
	tkString
	tkOther
)

type token struct {
	kind tokKind
	text string
}

// lex splits C source into a coarse token stream sufficient for macro expansion:
// identifiers, whitespace runs, string/char literals, the `##`/`#` operators, and
// single punctuation/other characters. It is not a full C lexer.
func lex(src string) []token {
	var out []token
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(src[j]) {
				j++
			}
			out = append(out, token{tkIdent, src[i:j]})
			i = j
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\\':
			j := i + 1
			for j < n && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r' || src[j] == '\\') {
				j++
			}
			out = append(out, token{tkSpace, " "})
			i = j
		case c == '"' || c == '\'':
			j := i + 1
			for j < n && src[j] != c {
				if src[j] == '\\' && j+1 < n {
					j++
				}
				j++
			}
			if j < n {
				j++ // closing quote
			}
			out = append(out, token{tkString, src[i:j]})
			i = j
		case c == '#':
			if i+1 < n && src[i+1] == '#' {
				out = append(out, token{tkPunct, "##"})
				i += 2
			} else {
				out = append(out, token{tkPunct, "#"})
				i++
			}
		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && (isIdentPart(src[j]) || src[j] == '.') {
				j++
			}
			out = append(out, token{tkOther, src[i:j]})
			i = j
		default:
			out = append(out, token{tkPunct, string(c)})
			i++
		}
	}
	return out
}

// allIdents returns every identifier in src. Used on fully-expanded macro text,
// where any identifier may name a referenced function (including a function passed
// as a call argument, e.g. single_open(file, name_show, ...) from
// DEFINE_SHOW_ATTRIBUTE). The funcNames filter in resolveFuncPtrRefs drops every
// non-function (types, field names, other macros), so over-collecting is safe.
func allIdents(src []byte) []string {
	var out []string
	for i, n := 0, len(src); i < n; {
		if isIdentStart(src[i]) {
			s := i
			for i < n && isIdentPart(src[i]) {
				i++
			}
			out = append(out, string(src[s:i]))
		} else {
			i++
		}
	}
	return out
}

func tokensText(toks []token) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.text)
	}
	return b.String()
}

// collectMacros scans src line-by-line for #define directives and adds them to
// table. Continuation lines (ending in `\`) are joined. Later definitions win.
//
// A logical line is materialised as a string only when it can BE a directive, which
// is decided from its first physical line alone: a continuation cannot introduce a
// leading `#`. The previous form built a []string of every logical line in the file
// before testing any of them, so a source tree paid one string allocation per line
// to find the few lines that begin with `#`.
func collectMacros(src []byte, table macroTable) {
	var joined strings.Builder
	for off := 0; off < len(src); {
		directive := startsDirective(src[off:])
		joined.Reset()
		for {
			line, next := physicalLine(src, off)
			off = next
			cont := len(line) > 0 && line[len(line)-1] == '\\'
			if cont {
				line = line[:len(line)-1]
			}
			if directive {
				joined.Write(line)
				if cont {
					joined.WriteByte(' ')
				}
			}
			if !cont || off >= len(src) {
				break
			}
		}
		if directive {
			if name, def, ok := parseDefine(joined.String()); ok {
				table[name] = def
			}
		}
	}
}

// physicalLine returns the line beginning at off with its trailing carriage returns
// removed, and the offset the next line starts at.
func physicalLine(src []byte, off int) (line []byte, next int) {
	if nl := bytes.IndexByte(src[off:], '\n'); nl >= 0 {
		line, next = src[off:off+nl], off+nl+1
	} else {
		line, next = src[off:], len(src)
	}
	return bytes.TrimRight(line, "\r"), next
}

// startsDirective reports whether a logical line beginning at the start of b opens a
// preprocessor directive: a `#` after nothing but spaces and tabs.
func startsDirective(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t':
		case '#':
			return true
		default:
			return false
		}
	}
	return false
}

// parseDefine parses a single logical line as `#define NAME[(params)] body`.
func parseDefine(line string) (string, macroDef, bool) {
	s := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(s, "#") {
		return "", macroDef{}, false
	}
	s = strings.TrimLeft(s[1:], " \t")
	if !strings.HasPrefix(s, "define") {
		return "", macroDef{}, false
	}
	s = s[len("define"):]
	if s == "" || (s[0] != ' ' && s[0] != '\t') {
		return "", macroDef{}, false
	}
	s = strings.TrimLeft(s, " \t")
	// name
	i := 0
	if i >= len(s) || !isIdentStart(s[i]) {
		return "", macroDef{}, false
	}
	for i < len(s) && isIdentPart(s[i]) {
		i++
	}
	name := s[:i]
	rest := s[i:]

	def := macroDef{}
	if strings.HasPrefix(rest, "(") { // function-like: '(' immediately follows the name
		close := strings.IndexByte(rest, ')')
		if close < 0 {
			return "", macroDef{}, false
		}
		params := strings.TrimSpace(rest[1:close])
		def.params = []string{} // function-like, possibly zero params
		if params != "" {
			for _, p := range strings.Split(params, ",") {
				p = strings.TrimSpace(p)
				switch {
				case p == "...":
					def.variadic = true
					def.variadicName = "__VA_ARGS__"
				case strings.HasSuffix(p, "..."):
					def.variadic = true
					def.variadicName = strings.TrimSpace(strings.TrimSuffix(p, "..."))
				default:
					def.params = append(def.params, p)
				}
			}
		}
		rest = rest[close+1:]
	}
	def.body = strings.TrimSpace(rest)
	return name, def, true
}

// expandCall expands the function-like macro `name(argTexts...)` and returns the
// expanded token stream. Returns nil if name is unknown or object-like. The args
// are the already-split invocation arguments (from the AST), each lexed as-is.
func expandCall(name string, argTexts []string, table macroTable) []token {
	def, ok := table[name]
	if !ok || def.params == nil {
		return nil
	}
	args := make([][]token, len(argTexts))
	for i, a := range argTexts {
		args[i] = lex(a)
	}
	sub := substitute(def, args)
	return expandTokens(sub, table, map[string]bool{name: true}, 1)
}

// expandTokens rescans toks for macro invocations and expands them recursively,
// blocking a macro from re-expanding within its own expansion (hideset) and
// capping depth.
func expandTokens(toks []token, table macroTable, hide map[string]bool, depth int) []token {
	if depth > maxExpandDepth {
		return toks
	}
	var out []token
	i := 0
	for i < len(toks) {
		if len(out) > maxExpandTokens {
			return append(out, toks[i:]...) // safety valve against pathological expansion
		}
		t := toks[i]
		if t.kind != tkIdent {
			out = append(out, t)
			i++
			continue
		}
		def, ok := table[t.text]
		if !ok || hide[t.text] {
			out = append(out, t)
			i++
			continue
		}
		newHide := cloneHide(hide, t.text)
		if def.params == nil { // object-like
			out = append(out, expandTokens(substitute(def, nil), table, newHide, depth+1)...)
			i++
			continue
		}
		// function-like: only an invocation if a '(' follows (modulo whitespace)
		j := i + 1
		for j < len(toks) && toks[j].kind == tkSpace {
			j++
		}
		if j >= len(toks) || toks[j].kind != tkPunct || toks[j].text != "(" {
			out = append(out, t)
			i++
			continue
		}
		args, end := parseArgs(toks, j)
		out = append(out, expandTokens(substitute(def, args), table, newHide, depth+1)...)
		i = end + 1
	}
	return out
}

func cloneHide(hide map[string]bool, add string) map[string]bool {
	n := make(map[string]bool, len(hide)+1)
	for k := range hide {
		n[k] = true
	}
	n[add] = true
	return n
}

// parseArgs parses a balanced-parenthesis argument list starting at the '(' at
// index open, splitting on top-level commas. Returns the args and the index of
// the matching ')'.
func parseArgs(toks []token, open int) ([][]token, int) {
	var args [][]token
	var cur []token
	depth := 0
	i := open
	for ; i < len(toks); i++ {
		t := toks[i]
		if t.kind == tkPunct {
			switch t.text {
			case "(":
				depth++
				if depth == 1 {
					continue // skip the opening paren itself
				}
			case ")":
				depth--
				if depth == 0 {
					args = append(args, trimSpaceTokens(cur))
					return args, i
				}
			case ",":
				if depth == 1 {
					args = append(args, trimSpaceTokens(cur))
					cur = nil
					continue
				}
			}
		}
		cur = append(cur, t)
	}
	args = append(args, trimSpaceTokens(cur))
	return args, i
}

func trimSpaceTokens(toks []token) []token {
	a, b := 0, len(toks)
	for a < b && toks[a].kind == tkSpace {
		a++
	}
	for b > a && toks[b-1].kind == tkSpace {
		b--
	}
	return toks[a:b]
}

// substitute replaces parameters in def.bodyTokens with the call args, applies #
// (stringize) and ## (token paste), and returns the resulting tokens. Arguments
// are substituted raw; further macro expansion happens on the rescan.
func substitute(def macroDef, args [][]token) []token {
	argOf := make(map[string][]token, len(def.params)+1)
	for i, p := range def.params {
		if i < len(args) {
			argOf[p] = args[i]
		} else {
			argOf[p] = nil
		}
	}
	if def.variadic {
		var va []token
		for i := len(def.params); i < len(args); i++ {
			if i > len(def.params) {
				va = append(va, token{tkPunct, ","})
			}
			va = append(va, args[i]...)
		}
		argOf[def.variadicName] = va
	}

	var out []token
	bt := lex(def.body)
	for j := 0; j < len(bt); j++ {
		t := bt[j]
		if t.kind == tkPunct && t.text == "#" { // stringize next param
			k := j + 1
			for k < len(bt) && bt[k].kind == tkSpace {
				k++
			}
			if k < len(bt) && bt[k].kind == tkIdent {
				if a, ok := argOf[bt[k].text]; ok {
					out = append(out, token{tkString, "\"" + tokensText(a) + "\""})
					j = k
					continue
				}
			}
			out = append(out, t)
			continue
		}
		if t.kind == tkIdent {
			if a, ok := argOf[t.text]; ok {
				if len(a) == 0 {
					out = append(out, token{tkOther, ""}) // placeholder so adjacent ## still pastes
				} else {
					out = append(out, a...)
				}
				continue
			}
		}
		out = append(out, t)
	}
	return pasteTokens(out)
}

// pasteTokens applies the ## operator: each ## merges the preceding and following
// (non-space) tokens into one.
func pasteTokens(toks []token) []token {
	var out []token
	i := 0
	for i < len(toks) {
		t := toks[i]
		if t.kind == tkPunct && t.text == "##" {
			p := len(out) - 1
			for p >= 0 && out[p].kind == tkSpace {
				p--
			}
			k := i + 1
			for k < len(toks) && toks[k].kind == tkSpace {
				k++
			}
			if p >= 0 && k < len(toks) {
				merged := token{tkIdent, out[p].text + toks[k].text}
				out = append(out[:p], merged)
				i = k + 1
				continue
			}
			i++ // dangling ##
			continue
		}
		out = append(out, t)
		i++
	}
	return out
}

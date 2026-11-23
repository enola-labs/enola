package phpextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// A Blade view is HTML with PHP in its echoes (`{{ }}`, `{!! !!}`), its directive
// arguments (`@if(...)`, `@foreach(...)`) and its `@php` blocks. To tree-sitter all of
// that is inline text, so a method or class used only from a view had no reference.
// bladeToPHP keeps only the code, as PHP statements on their original lines, and
// walkTemplate walks it like a function body onto the view's file reference fact.

// isBladeFile reports whether a path is a Blade view.
func isBladeFile(relFile string) bool {
	return strings.HasSuffix(strings.ToLower(relFile), ".blade.php")
}

// bladeArgDirectives are the Blade directives whose parenthesised argument is PHP.
// An `@name(` that is not one of them is text: `@media (`, an e-mail address, a
// front-end framework's `@click(`.
var bladeArgDirectives = map[string]bool{
	"if": true, "elseif": true, "unless": true, "isset": true, "empty": true,
	"switch": true, "case": true, "while": true, "foreach": true, "forelse": true, "for": true,
	"break": true, "continue": true,
	"include": true, "includeIf": true, "includeWhen": true, "includeUnless": true, "includeFirst": true,
	"each": true, "component": true, "slot": true, "livewire": true,
	"can": true, "cannot": true, "canany": true, "elsecan": true, "elsecannot": true, "elsecanany": true,
	"auth": true, "guest": true, "env": true, "production": true, "error": true, "session": true, "context": true,
	"section": true, "yield": true, "hasSection": true, "sectionMissing": true,
	"push": true, "pushIf": true, "pushOnce": true, "prepend": true, "prependOnce": true, "stack": true, "once": true,
	"json": true, "js": true, "lang": true, "choice": true, "method": true,
	"class": true, "style": true, "checked": true, "selected": true, "disabled": true, "readonly": true, "required": true,
	"props": true, "aware": true, "fragment": true, "dump": true, "dd": true,
	"php": true, "inject": true, "use": true,
}

// bladeToPHP rewrites a Blade view into the PHP it runs: each echo, directive
// argument, `@php` block and literal `<?php ?>` block becomes a statement, and the
// text between them is dropped but for its newlines, so every statement keeps its
// line. Comments, `@verbatim` blocks and escaped `@{{` are text.
func bladeToPHP(src []byte) []byte {
	s := string(src)
	var b strings.Builder
	b.Grow(len(s) / 4)
	b.WriteString("<?php ")
	// skip drops s[from:to] but for its newlines.
	skip := func(from, to int) {
		b.WriteString(strings.Repeat("\n", strings.Count(s[from:to], "\n")))
	}
	// region is the code between open and the first close after it, emitted as one
	// statement; an unclosed region runs to the end of the view.
	region := func(i int, open, close string) int {
		start := i + len(open)
		end := strings.Index(s[start:], close)
		if end < 0 {
			end = len(s) - start
		}
		b.WriteString(s[start : start+end])
		b.WriteString("; ")
		return min(start+end+len(close), len(s))
	}
	for i := 0; i < len(s); {
		rest := s[i:]
		switch {
		case strings.HasPrefix(rest, "{{--"):
			end := strings.Index(rest, "--}}")
			if end < 0 {
				end = len(rest) - 4
			}
			skip(i, i+end+4)
			i += end + 4
		case strings.HasPrefix(rest, "@{{"):
			i += 3
		case strings.HasPrefix(rest, "@@"):
			i += 2
		case strings.HasPrefix(rest, "{!!"):
			i = region(i, "{!!", "!!}")
		case strings.HasPrefix(rest, "{{"):
			i = region(i, "{{", "}}")
		case strings.HasPrefix(rest, "<?php"):
			i = region(i, "<?php", "?>")
		case strings.HasPrefix(rest, "<?="):
			i = region(i, "<?=", "?>")
		case s[i] == '@' && (i == 0 || !isWordByte(s[i-1])):
			i = bladeDirective(s, i, &b, skip)
		default:
			if s[i] == '\n' {
				b.WriteByte('\n')
			}
			i++
		}
	}
	return []byte(b.String())
}

// bladeDirective emits the PHP of the directive at s[at] ('@') and returns the index
// after it. A directive that takes no PHP is text, and only its '@' is consumed.
func bladeDirective(s string, at int, b *strings.Builder, skip func(from, to int)) int {
	end := at + 1
	for end < len(s) && isWordByte(s[end]) {
		end++
	}
	name := s[at+1 : end]
	switch name {
	case "verbatim":
		stop := strings.Index(s[end:], "@endverbatim")
		if stop < 0 {
			stop = len(s) - end
		}
		skip(at, end+stop)
		return end + stop
	case "php":
		if open := bladeArgOpen(s, end); open < 0 {
			stop := strings.Index(s[end:], "@endphp")
			if stop < 0 {
				stop = len(s) - end
			}
			b.WriteString(s[end : end+stop])
			b.WriteString("; ")
			return end + stop
		}
	}
	if !bladeArgDirectives[name] {
		return at + 1
	}
	open := bladeArgOpen(s, end)
	if open < 0 {
		return at + 1
	}
	close := matchingParen(s, open)
	if close < 0 {
		return at + 1
	}
	args := s[open+1 : close]
	switch name {
	case "foreach", "forelse":
		b.WriteString("foreach(" + args + "){} ")
	case "for":
		b.WriteString("for(" + args + "){} ")
	case "php":
		b.WriteString(args + "; ")
	case "use":
		// @use('App\Models\Flight') and @use('App\Models\Flight', 'Alias')
		parts := splitTopLevel(args)
		class := unquoteBlade(parts[0])
		if class == "" {
			break
		}
		b.WriteString("use " + strings.TrimLeft(class, `\`))
		if len(parts) > 1 {
			if alias := unquoteBlade(parts[1]); alias != "" {
				b.WriteString(" as " + alias)
			}
		}
		b.WriteString("; ")
		b.WriteString(strings.Repeat("\n", strings.Count(args, "\n")))
	case "inject":
		// @inject('var', 'App\Service') resolves the service from the container: it
		// names the class, whether written as a string or as Service::class.
		parts := splitTopLevel(args)
		if len(parts) == 2 {
			if class := unquoteBlade(parts[1]); class != "" {
				b.WriteString(`\` + strings.TrimLeft(class, `\`) + "::class; ")
				b.WriteString(strings.Repeat("\n", strings.Count(args, "\n")))
				break
			}
		}
		b.WriteString("[" + args + "]; ")
	default:
		b.WriteString("[" + args + "]; ")
	}
	return close + 1
}

// bladeArgOpen returns the index of the '(' opening a directive's argument at or
// after i, past spaces and tabs, or -1.
func bladeArgOpen(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && s[i] == '(' {
		return i
	}
	return -1
}

// matchingParen returns the index of the ')' closing the '(' at open, stepping over
// quoted strings, or -1.
func matchingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '\'', '"':
			i = stringEnd(s, i)
			if i < 0 {
				return -1
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// stringEnd returns the index of the quote closing the string opened at s[i].
func stringEnd(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j
		}
	}
	return -1
}

// splitTopLevel splits an argument list at its top-level commas.
func splitTopLevel(args string) []string {
	var out []string
	depth, from := 0, 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '\'', '"':
			if end := stringEnd(args, i); end > 0 {
				i = end
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, args[from:i])
				from = i + 1
			}
		}
	}
	return append(out, args[from:])
}

// unquoteBlade returns a quoted class name argument without its quotes, "" when the
// argument is anything else.
func unquoteBlade(arg string) string {
	arg = strings.TrimSpace(arg)
	if len(arg) < 2 || (arg[0] != '\'' && arg[0] != '"') || arg[len(arg)-1] != arg[0] {
		return ""
	}
	name := strings.ReplaceAll(arg[1:len(arg)-1], `\\`, `\`)
	for i := 0; i < len(name); i++ {
		if !isWordByte(name[i]) && name[i] != '\\' {
			return ""
		}
	}
	return name
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// walkTemplate records what a view's code calls, constructs and names on a file
// reference fact for the view: no symbol owns that code, and only the dead-code
// detector and the graph read the fact.
func (w *phpWalker) walkTemplate(root *sitter.Node) {
	w.out = append(w.out, facts.Fact{
		Kind:  facts.KindFileRef,
		Name:  w.relFile,
		File:  w.relFile,
		Props: map[string]any{"language": "php"},
	})
	idx := len(w.out) - 1
	w.walkForCalls(root, idx, map[string]bool{})
	if len(w.out[idx].Relations) == 0 {
		w.out = w.out[:idx]
	}
}

// bladeDirectiveCode walks the PHP that a `Blade::directive` callback returns as a
// string literal (`return "<?php echo app('captcha')->scripts(); ?>";`). Blade
// pastes that code into every view using the directive, so what it calls is called
// by the method registering it.
func (w *phpWalker) bladeDirectiveCode(call *sitter.Node, ownerIdx int, seen map[string]bool) {
	if ownerIdx < 0 {
		return
	}
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "string", "encapsed_string":
			if code := directiveStringCode(phpText(n, w.src)); code != "" {
				w.walkCode(code, ownerIdx, seen)
			}
			return
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			visit(n.NamedChild(i))
		}
	}
	visit(call.ChildByFieldName("arguments"))
}

// directiveStringCode returns the PHP inside the `<?php ... ?>` of a string literal's
// source text, "" when it holds none.
func directiveStringCode(lit string) string {
	if len(lit) < 2 {
		return ""
	}
	body := lit[1 : len(lit)-1]
	open := strings.Index(body, "<?php")
	if open < 0 {
		return ""
	}
	body = body[open+len("<?php"):]
	if end := strings.Index(body, "?>"); end >= 0 {
		body = body[:end]
	}
	if lit[0] == '"' {
		body = strings.NewReplacer(`\$`, `$`, `\"`, `"`, `\\`, `\`).Replace(body)
	}
	return body
}

// walkCode parses a fragment of PHP and walks it onto the owner fact, in the scope
// of the enclosing file (its namespace, imports and class).
func (w *phpWalker) walkCode(code string, ownerIdx int, seen map[string]bool) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return
	}
	src := []byte("<?php " + code + ";")
	tree := parser.Parse(src, nil)
	defer tree.Close()
	outer, metrics := w.src, w.metrics
	w.src, w.metrics = src, nil
	w.walkForCalls(tree.RootNode(), ownerIdx, seen)
	w.src, w.metrics = outer, metrics
}

// isBladeDirectiveCall reports whether a static call registers a custom Blade
// directive, through the facade or its global alias.
func isBladeDirectiveCall(target string) bool {
	return target == `Illuminate\Support\Facades\Blade::directive` || target == `Blade::directive`
}

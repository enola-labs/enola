package rubyextractor

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// rubyConstPathCall matches a client call whose path is a CONSTANT rather than
// a literal, capturing receiver, verb, the scope written before the constant
// and the constant's name:
//
//	Billing.client.post(PATH, payload)               -> scope ""
//	Billing.client.post(self.class::PATH, payload)   -> scope "self.class::"
//	Billing.client.post(Paths::BALANCE, payload)     -> scope "Paths::"
//
// The constant must be SCREAMING_CASE: a CamelCase argument is a class
// (`Payload.new(...)`), never a path.
var rubyConstPathCall = regexp.MustCompile(`(?:^|[^.\w@$])` + rubyReceiver + `\.(get|post|put|patch|delete|head)\b\s*\(?\s*(self\.class::|(?:[A-Z]\w*(?:::[A-Z]\w*)*)::)?([A-Z][A-Z0-9_]*)\s*[,)]`)

// rubyConstStringAssign matches a constant assigned one string literal,
// optionally frozen: `PATH = '/api/v1/products/details'`.
var rubyConstStringAssign = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*)\s*=\s*(?:"([^"]*)"|'([^']*)')(?:\.freeze)?\s*(?:#.*)?$`)

const (
	derivedConstant          = "constant"
	derivedInheritedConstant = "inherited-constant"
	constantUnresolved       = "constant-unresolved"
	constantAmbiguous        = "constant-ambiguous"
	selfClassScope           = "self.class::"
)

// rubyConstAssign is one `NAME = 'literal'` line.
type rubyConstAssign struct {
	file  string
	line  int
	name  string
	value string
}

// rubyConstSink is one client call whose path is a constant, kept until the
// whole repository's classes are known: `self.class::PATH` in a base class is
// answered by the subclasses, which live in other files.
type rubyConstSink struct {
	file  string
	line  int
	scope string
	name  string
	verb  string
	recv  string
	api   string
	hint  string
}

// scanRubyClientConstants collects a file's string-constant assignments and its
// client calls that pass a constant as the path. It derives nothing on its own;
// resolveRubyClientConstants does, once every file has been scanned.
func scanRubyClientConstants(src []byte, relFile string) ([]rubyConstAssign, []rubyConstSink) {
	api := rubyAPIHint(relFile)
	envHint := envVarHint(string(src))
	var assigns []rubyConstAssign
	var sinks []rubyConstSink
	for i, line := range strings.Split(string(src), "\n") {
		if m := rubyConstStringAssign.FindStringSubmatch(line); m != nil {
			value := m[2]
			if value == "" {
				value = m[3]
			}
			assigns = append(assigns, rubyConstAssign{file: relFile, line: i + 1, name: m[1], value: value})
			continue
		}
		m := rubyConstPathCall.FindStringSubmatch(line)
		if m == nil || !isHTTPClientReceiver(m[1]) {
			continue
		}
		hint := hintFromReceiver(m[1])
		if hint == "" {
			hint = envHint
		}
		sinks = append(sinks, rubyConstSink{
			file: relFile, line: i + 1, scope: m[3], name: m[4],
			verb: m[2], recv: m[1], api: api, hint: hint,
		})
	}
	return assigns, sinks
}

// rubyClassSpan is a class symbol's name, span and superclass as written.
type rubyClassSpan struct {
	name      string
	file      string
	line, end int
	super     string
}

// resolveRubyClientConstants turns constant-path client calls into client
// routes, using the class symbols of the whole repository:
//
//   - `PATH` resolves the way Ruby does: the enclosing class, then its
//     ancestors, then a single same-file assignment for code outside a class.
//   - `Foo::PATH` resolves Foo from the enclosing namespace, then reads PATH on
//     Foo or its ancestors.
//   - `self.class::PATH` is answered by every class the call can run as: the
//     enclosing class and each descendant that assigns PATH itself. That is the
//     template-method shape — one HTTP call in a base class, one path per
//     subclass — and a call the base class makes for N subclasses is N routes.
//
// Each derived route is located at the assignment that holds its literal,
// which is the file that owns that endpoint. A constant no class assigns, or
// one assigned two different values where only one may apply, derives nothing
// and is counted.
func resolveRubyClientConstants(all []facts.Fact, assigns []rubyConstAssign, sinks []rubyConstSink) ([]facts.Fact, int, map[string]int) {
	misses := map[string]int{}
	if len(sinks) == 0 {
		return nil, 0, misses
	}

	var classes []rubyClassSpan
	byName := map[string]bool{}
	byFile := map[string][]int{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.PropAny("symbol_kind") != facts.SymbolClass {
			continue
		}
		super, _ := f.PropAny("superclass").(string)
		classes = append(classes, rubyClassSpan{
			name: f.Name, file: f.File, line: f.Line, end: f.EndLine, super: normalizeConstant(super),
		})
		byName[f.Name] = true
		byFile[f.File] = append(byFile[f.File], len(classes)-1)
	}

	// classAt is the innermost class whose span holds the line.
	classAt := func(file string, line int) string {
		best := -1
		for _, i := range byFile[file] {
			c := classes[i]
			if c.line <= line && (c.end == 0 || line <= c.end) {
				if best < 0 || c.line >= classes[best].line {
					best = i
				}
			}
		}
		if best < 0 {
			return ""
		}
		return classes[best].name
	}

	// own maps "Class#NAME" to the values that class assigns; fileLevel maps
	// "file#NAME" to the values assigned outside any class.
	type located struct {
		value, file string
		line        int
	}
	own := map[string][]located{}
	fileLevel := map[string][]located{}
	for _, a := range assigns {
		loc := located{value: a.value, file: a.file, line: a.line}
		if cls := classAt(a.file, a.line); cls != "" {
			own[cls+"#"+a.name] = append(own[cls+"#"+a.name], loc)
		} else {
			fileLevel[a.file+"#"+a.name] = append(fileLevel[a.file+"#"+a.name], loc)
		}
	}

	// resolveRef resolves a constant reference written inside class `from` the
	// way Ruby's lexical lookup does — innermost namespace outward — and falls
	// back to a unique last-segment match.
	lastSegment := map[string][]string{}
	for name := range byName {
		seg := name
		if i := strings.LastIndex(seg, "::"); i >= 0 {
			seg = seg[i+2:]
		}
		lastSegment[seg] = append(lastSegment[seg], name)
	}
	resolveRef := func(ref, from string) string {
		ref = normalizeConstant(ref)
		if ref == "" {
			return ""
		}
		for ns := from; ns != ""; {
			i := strings.LastIndex(ns, "::")
			if i < 0 {
				ns = ""
			} else {
				ns = ns[:i]
			}
			if ns == "" {
				break
			}
			if cand := ns + "::" + ref; byName[cand] {
				return cand
			}
		}
		if byName[ref] {
			return ref
		}
		seg := ref
		if i := strings.LastIndex(seg, "::"); i >= 0 {
			seg = seg[i+2:]
		}
		if cands := lastSegment[seg]; len(cands) == 1 {
			return cands[0]
		}
		return ""
	}

	// parent is each class's resolved superclass; children its inverse.
	parent := map[string]string{}
	children := map[string][]string{}
	for _, c := range classes {
		if c.super == "" {
			continue
		}
		if p := resolveRef(c.super, c.name); p != "" && p != c.name {
			if _, seen := parent[c.name]; !seen {
				parent[c.name] = p
				children[p] = append(children[p], c.name)
			}
		}
	}
	for p := range children {
		sort.Strings(children[p])
	}

	// lookup reads NAME on cls or its nearest ancestor that assigns it.
	lookup := func(cls, name string) ([]located, bool) {
		seen := map[string]bool{}
		for c := cls; c != "" && !seen[c]; c = parent[c] {
			seen[c] = true
			if locs := own[c+"#"+name]; len(locs) > 0 {
				return locs, true
			}
		}
		return nil, false
	}
	single := func(locs []located) (located, bool) {
		for _, l := range locs[1:] {
			if l.value != locs[0].value {
				return located{}, false
			}
		}
		return locs[0], true
	}

	var out []facts.Fact
	derived := 0
	emitted := map[string]bool{}
	emit := func(s rubyConstSink, loc located, kind, viaClass string) {
		path, ok := cleanRubyPath(loc.value)
		if !ok {
			misses[constantUnresolved]++
			return
		}
		key := loc.file + "\x00" + path + "\x00" + s.verb + "\x00" + strconv.Itoa(loc.line)
		if emitted[key] {
			return
		}
		emitted[key] = true
		derived++
		props := map[string]any{
			facts.PropRole:   facts.RoleClient,
			"method":         strings.ToUpper(s.verb),
			"framework":      rubyFramework(s.recv),
			"language":       "ruby",
			facts.PropSource: facts.RouteSourceRubyHTTPClient,
			"api":            s.api,
			"target_hint":    s.hint,
			"derived":        kind,
		}
		if viaClass != "" {
			props["via_class"] = viaClass
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      path,
			File:      loc.file,
			Line:      loc.line,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: factpath.Dir(loc.file)}},
		})
	}

	for _, s := range sinks {
		cls := classAt(s.file, s.line)
		switch {
		case s.scope == selfClassScope:
			if cls == "" {
				misses[constantUnresolved]++
				continue
			}
			found := false
			queue := []string{cls}
			visited := map[string]bool{}
			for len(queue) > 0 {
				c := queue[0]
				queue = queue[1:]
				if visited[c] {
					continue
				}
				visited[c] = true
				queue = append(queue, children[c]...)
				locs := own[c+"#"+s.name]
				if len(locs) == 0 {
					continue
				}
				loc, ok := single(locs)
				if !ok {
					misses[constantAmbiguous]++
					continue
				}
				found = true
				emit(s, loc, derivedInheritedConstant, cls)
			}
			if !found {
				misses[constantUnresolved]++
			}
		case s.scope != "":
			target := resolveRef(strings.TrimSuffix(s.scope, "::"), cls)
			locs, ok := lookup(target, s.name)
			if target == "" || !ok {
				misses[constantUnresolved]++
				continue
			}
			if loc, ok := single(locs); ok {
				emit(s, loc, derivedConstant, "")
			} else {
				misses[constantAmbiguous]++
			}
		default:
			locs, ok := lookup(cls, s.name)
			if !ok {
				locs = fileLevel[s.file+"#"+s.name]
			}
			if len(locs) == 0 {
				misses[constantUnresolved]++
				continue
			}
			if loc, ok := single(locs); ok {
				emit(s, loc, derivedConstant, "")
			} else {
				misses[constantAmbiguous]++
			}
		}
	}
	return out, derived, misses
}

// sortConstInputs orders the scan's output by file and line, so resolution
// does not depend on the order the parallel per-file pass finished in.
func sortConstInputs(assigns []rubyConstAssign, sinks []rubyConstSink) {
	sort.Slice(assigns, func(i, j int) bool {
		if assigns[i].file != assigns[j].file {
			return assigns[i].file < assigns[j].file
		}
		return assigns[i].line < assigns[j].line
	})
	sort.Slice(sinks, func(i, j int) bool {
		if sinks[i].file != sinks[j].file {
			return sinks[i].file < sinks[j].file
		}
		return sinks[i].line < sinks[j].line
	})
}

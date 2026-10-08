package rubyextractor

import (
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// rubyParameterSink matches a client call whose path is a bare identifier,
// optionally wrapped once: `connection.get(build_url(path), ...)` or
// `client.post(path)`. It captures receiver, verb and the identifier.
var rubyParameterSink = regexp.MustCompile(`(?:^|[^.\w@$])` + rubyReceiver + `\.(get|post|put|patch|delete|head)\b\s*\(?\s*(?:[a-z_][\w]*[!?]?\s*\(\s*)?([a-z_][\w]*)\s*[,)]`)

// rubyDefAnyLine captures a method definition's name and its parameter list,
// parenthesised or bare.
var rubyDefAnyLine = regexp.MustCompile(`^\s*def\s+([a-z_][\w!?]*)\s*(?:\(([^)]*)\)|\s+([^#\n]*))?`)

const (
	derivedParameter         = "parameter"
	parameterNonLiteral      = "parameter-non-literal"
	parameterAmbiguousMethod = "parameter-ambiguous"
)

// extractParameterClientFacts derives one client fact per literal a caller
// passes into a method whose positional parameter reaches a client call as
// the path. The lookup is one hop, inside the file and the class that
// defines the method; a caller passing anything but a string literal at that
// position is counted, and a method name defined more than once in the file
// derives nothing and is counted once.
func extractParameterClientFacts(lines []string, relFile, api, envHint string) ([]facts.Fact, int, map[string]int) {
	misses := map[string]int{}
	defCount := map[string]int{}
	for _, line := range lines {
		if m := rubyDefAnyLine.FindStringSubmatch(line); m != nil {
			defCount[m[1]]++
		}
	}

	type sink struct {
		method string
		index  int
		verb   string
		recv   string
		line   int
	}
	var sinks []sink
	seen := map[string]bool{}
	for i, line := range lines {
		m := rubyParameterSink.FindStringSubmatch(line)
		if m == nil || !isHTTPClientReceiver(m[1]) {
			continue
		}
		ident := m[3]
		method, params := enclosingDef(lines, i)
		if method == "" {
			continue
		}
		index := positionalIndex(params, ident)
		if index < 0 {
			continue
		}
		if defCount[method] > 1 {
			if !seen[method] {
				misses[parameterAmbiguousMethod]++
				seen[method] = true
			}
			continue
		}
		sinks = append(sinks, sink{method: method, index: index, verb: m[2], recv: m[1], line: i})
	}
	if len(sinks) == 0 {
		return nil, 0, misses
	}

	var out []facts.Fact
	resolved := 0
	for _, s := range sinks {
		callOpen := regexp.MustCompile(`(?:^|[^.\w])` + regexp.QuoteMeta(s.method) + `\s*[\( ]\s*(.*)$`)
		for i, line := range lines {
			if i == s.line || rubyDefAnyLine.MatchString(line) {
				continue
			}
			m := callOpen.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			arg := positionalArg(m[1], s.index)
			if arg == "" {
				continue
			}
			if len(arg) < 2 || (arg[0] != '"' && arg[0] != '\'') || arg[len(arg)-1] != arg[0] {
				misses[parameterNonLiteral]++
				continue
			}
			path, ok := cleanRubyPath(arg[1 : len(arg)-1])
			if !ok {
				misses[parameterNonLiteral]++
				continue
			}
			hint := hintFromReceiver(s.recv)
			if hint == "" {
				hint = envHint
			}
			resolved++
			out = append(out, facts.Fact{
				Kind: facts.KindRoute,
				Name: path,
				File: relFile,
				Line: i + 1,
				Props: map[string]any{
					facts.PropRole:   facts.RoleClient,
					"method":         strings.ToUpper(s.verb),
					"framework":      rubyFramework(s.recv),
					"language":       "ruby",
					facts.PropSource: facts.RouteSourceRubyHTTPClient,
					"api":            api,
					"target_hint":    hint,
					"derived":        derivedParameter,
					"via_method":     s.method,
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: factpath.Dir(relFile)}},
			})
		}
	}
	return out, resolved, misses
}

func enclosingDef(lines []string, from int) (string, string) {
	for j := from; j >= 0; j-- {
		if m := rubyDefAnyLine.FindStringSubmatch(lines[j]); m != nil {
			params := m[2]
			if params == "" {
				params = m[3]
			}
			return m[1], params
		}
	}
	return "", ""
}

// positionalIndex returns the position of ident among the positional
// parameters of a def, or -1 when ident is absent or a keyword, splat or
// block parameter.
func positionalIndex(params, ident string) int {
	index := 0
	for _, raw := range strings.Split(params, ",") {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "*") || strings.HasPrefix(p, "&") || strings.HasSuffix(p, ":") || strings.Contains(p, ": ") {
			continue
		}
		name := p
		if eq := strings.Index(name, "="); eq >= 0 {
			name = strings.TrimSpace(name[:eq])
		}
		if name == ident {
			return index
		}
		index++
	}
	return -1
}

// positionalArg returns the index-th top-level argument of a call's argument
// text, stopping at the closing parenthesis of the call; keyword arguments
// are skipped, so `name("/x", sources:)` has one positional argument.
func positionalArg(rest string, index int) string {
	depth := 0
	inQuote := byte(0)
	start := 0
	var args []string
	flush := func(end int) {
		arg := strings.TrimSpace(rest[start:end])
		if arg != "" {
			args = append(args, arg)
		}
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth == 0 {
				flush(i)
				return pick(args, index)
			}
			depth--
		case c == ',' && depth == 0:
			flush(i)
			start = i + 1
		}
	}
	flush(len(rest))
	return pick(args, index)
}

func pick(args []string, index int) string {
	positional := 0
	for _, a := range args {
		if strings.HasSuffix(a, ":") || regexp.MustCompile(`^[a-z_]\w*:\s`).MatchString(a) {
			continue
		}
		if positional == index {
			return a
		}
		positional++
	}
	return ""
}

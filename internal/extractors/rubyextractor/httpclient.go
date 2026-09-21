package rubyextractor

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/litfold"
)

// rubyClientCall matches an outbound HTTP-client call whose first argument is a
// string literal, capturing the receiver, the HTTP verb, and the path literal in
// one of two groups (double- or single-quote — RE2 has no backreferences). It
// matches both parenthesized and bare-argument forms, e.g.
//
//	SvcCheckoutClient.post('purchase/build')
//	conn.get "users/123"
//
// The leading (?:^|[^.\w@$]) ensures the receiver is not itself the tail of a
// longer method chain (e.g. record.posts.get), which cuts ActiveRecord noise.
var rubyClientCall = regexp.MustCompile(`(?:^|[^.\w@$])([A-Za-z_][A-Za-z0-9_]*(?:::[A-Za-z_][A-Za-z0-9_]*)*)\.(get|post|put|patch|delete|head)\b\s*\(?\s*(?:"([^"]*)"|'([^']*)')`)

// rubyWrapperCall matches a client call whose first argument is a WRAPPER CALL
// carrying one string literal — connection.post(build_url("/pageview"), attrs)
// — the wrapper-literal derivation form. The wrapper expression (identifier
// plus its single quoted argument) is captured whole and handed to litfold,
// which owns the rule: the wrapped literal must be "/"-rooted or it derives
// nothing.
var rubyWrapperCall = regexp.MustCompile(`(?:^|[^.\w@$])([A-Za-z_][A-Za-z0-9_]*(?:::[A-Za-z_][A-Za-z0-9_]*)*)\.(get|post|put|patch|delete|head)\b\s*\(?\s*([a-z_][\w.]*[!?]?\(\s*(?:"[^"]*"|'[^']*')\s*\))`)

// rubyInterpolation matches a Ruby string interpolation, e.g. #{id}.
var rubyInterpolation = regexp.MustCompile(`#\{[^}]*\}`)

// rubyEnvVar matches an environment-variable read, capturing the variable name,
// e.g. ENV['CORE_HTTP_CLIENT_BASE_URL'] or ENV.fetch("XENDO_URL").
var rubyEnvVar = regexp.MustCompile(`ENV(?:\.fetch)?\s*[\(\[]\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)

// httpClientReceivers are the lowercased base receiver names (after stripping any
// "::"-scope) that denote a hand-written HTTP client, in addition to any constant
// whose name ends in "Client" (e.g. SvcCheckoutClient).
var httpClientReceivers = map[string]bool{
	"conn": true, "connection": true, "http": true,
	"client": true, "faraday": true, "req": true, "request": true,
	"typhoeus": true,
}

// typhoeusRequestNew matches the start of a Typhoeus::Request.new call. The URL
// and the method: kwarg usually sit on following lines, so detection is a small
// forward window rather than a single-line match.
var typhoeusRequestNew = regexp.MustCompile(`Typhoeus::Request\.new\s*\(`)

// rubyMethodKwarg matches a literal HTTP-verb symbol kwarg, e.g. `method: :get`.
var rubyMethodKwarg = regexp.MustCompile(`method:\s*:(get|post|put|patch|delete|head)\b`)

// rubyTemplateTailParam matches a request-URL template whose tail is a bare
// identifier — "#{base_url}#{path}" — capturing the base and tail identifiers.
// Only the two-part base+tail shape qualifies; anything longer derives nothing.
var rubyTemplateTailParam = regexp.MustCompile(`"#\{@?([a-z_][\w]*)\}#\{([a-z_][\w]*)\}"`)

// rubyDefLine captures a method definition's name and parameter list.
var rubyDefLine = regexp.MustCompile(`^\s*def\s+([a-z_][\w!?]*)\s*\(([^)]*)\)`)

// rubyPathKwarg matches a rooted string literal passed as a named kwarg,
// capturing kwarg name and literal.
var rubyPathKwarg = regexp.MustCompile(`([a-z_][\w]*):\s*"(/[^"]*)"`)

// extractRubyHTTPClientFacts emits a client-route fact for every hand-written
// HTTP-client call (Faraday / Net::HTTP / wrapper clients) in a Ruby source
// file. These represent outbound calls the app makes, so the cross-repo linker
// can match them (by method + path suffix) to the service route that serves
// them. Paths are emitted as written; the linker's suffix matching reconciles
// base-path/prefix differences.
func extractRubyHTTPClientFacts(src []byte, relFile string) []facts.Fact {
	out, _, _ := extractRubyHTTPClientFactsCounted(src, relFile)
	return out
}

// extractRubyHTTPClientFactsCounted also reports how many client paths were
// derived through a parameter and the derivations that could not be made,
// so the repository's extraction fact carries them.
func extractRubyHTTPClientFactsCounted(src []byte, relFile string) ([]facts.Fact, int, map[string]int) {
	api := rubyAPIHint(relFile)
	envHint := envVarHint(string(src)) // file-level fallback

	out := extractWrapperMethodClientFacts(string(src), relFile, api, envHint)
	derivedFacts, derivedCount, misses := extractParameterClientFacts(strings.Split(string(src), "\n"), relFile, api, envHint)
	out = append(out, derivedFacts...)
	for i, line := range strings.Split(string(src), "\n") {
		derived := ""
		m := rubyClientCall.FindStringSubmatch(line)
		if m == nil {
			if wm := rubyWrapperCall.FindStringSubmatch(line); wm != nil {
				if lit, ok := litfold.WrapperLiteralPath(wm[3]); ok {
					m = []string{wm[0], wm[1], wm[2], lit, ""}
					derived = "wrapper-literal"
				}
			}
			if m == nil {
				continue
			}
		}
		recv := m[1]
		if !isHTTPClientReceiver(recv) {
			continue
		}
		raw := m[3]
		if raw == "" {
			raw = m[4]
		}
		path, ok := cleanRubyPath(raw)
		if !ok {
			continue
		}
		hint := hintFromReceiver(recv)
		if hint == "" {
			if h := envVarHint(line); h != "" {
				hint = h
			} else {
				hint = envHint
			}
		}
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: path,
			File: relFile,
			Line: i + 1,
			Props: map[string]any{
				facts.PropRole:   facts.RoleClient,
				"method":         strings.ToUpper(m[2]),
				"framework":      rubyFramework(recv),
				"language":       "ruby",
				facts.PropSource: facts.RouteSourceRubyHTTPClient,
				"api":            api,
				"target_hint":    hint,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: factpath.Dir(relFile)}},
		})
		if derived != "" {
			out[len(out)-1].SetProp("derived", derived)
		}
	}
	return out, derivedCount, misses
}

// extractWrapperMethodClientFacts derives client facts through a request-wrapper
// method: a private helper holding the file's ONLY HTTP sink whose URL template
// tails a parameter — `Typhoeus::Request.new("#{base_url}#{path}", method: :get)`
// inside `def make_request(path:, params:)` — with the real paths living as
// rooted string-literal kwargs at the same-file call sites. The single-sink rule
// mirrors litfold's single-assignment doctrine: two sinks, or a tail identifier
// that is not a parameter of the enclosing def, derive nothing. The verb must be
// a literal symbol on the sink.
func extractWrapperMethodClientFacts(src, relFile, api, envHint string) []facts.Fact {
	lines := strings.Split(src, "\n")

	sinkLine := -1
	for i, line := range lines {
		if typhoeusRequestNew.MatchString(line) {
			if sinkLine >= 0 {
				return nil // two sinks: ambiguous, derive nothing
			}
			sinkLine = i
		}
	}
	if sinkLine < 0 {
		return nil
	}

	baseIdent, tailParam, verb := "", "", ""
	for j := sinkLine; j < len(lines) && j <= sinkLine+6; j++ {
		if m := rubyTemplateTailParam.FindStringSubmatch(lines[j]); m != nil && tailParam == "" {
			baseIdent, tailParam = m[1], m[2]
		}
		if m := rubyMethodKwarg.FindStringSubmatch(lines[j]); m != nil && verb == "" {
			verb = m[1]
		}
	}
	if tailParam == "" || verb == "" {
		return nil
	}

	// The base identifier's own assignment is the precise hint source: the env
	// var behind `@base_url = ENV["ANALYTICS_BASE_URL"]` names the provider,
	// where a file-level first-env fallback can grab an unrelated variable.
	baseAssign := regexp.MustCompile(`@?` + regexp.QuoteMeta(baseIdent) + `\s*=\s*ENV`)
	for _, line := range lines {
		if baseAssign.MatchString(line) {
			if h := envVarHint(line); h != "" {
				envHint = h
			}
			break
		}
	}

	wrapper := ""
	for j := sinkLine; j >= 0; j-- {
		if m := rubyDefLine.FindStringSubmatch(lines[j]); m != nil {
			if strings.Contains(m[2], tailParam) {
				wrapper = m[1]
			}
			break
		}
	}
	if wrapper == "" {
		return nil
	}

	var out []facts.Fact
	callOpen := regexp.MustCompile(`\b` + regexp.QuoteMeta(wrapper) + `\s*\(`)
	for i, line := range lines {
		if !callOpen.MatchString(line) || rubyDefLine.MatchString(line) {
			continue
		}
		for j := i; j < len(lines) && j <= i+2; j++ {
			m := rubyPathKwarg.FindStringSubmatch(lines[j])
			if m == nil || m[1] != tailParam {
				continue
			}
			if path, ok := cleanRubyPath(m[2]); ok {
				out = append(out, facts.Fact{
					Kind: facts.KindRoute,
					Name: path,
					File: relFile,
					Line: j + 1,
					Props: map[string]any{
						facts.PropRole:   facts.RoleClient,
						"method":         strings.ToUpper(verb),
						"framework":      "typhoeus",
						"language":       "ruby",
						facts.PropSource: facts.RouteSourceRubyHTTPClient,
						"api":            api,
						"target_hint":    envHint,
						"derived":        "wrapper-method",
					},
					Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: factpath.Dir(relFile)}},
				})
			}
			break
		}
	}
	return out
}

// isHTTPClientReceiver reports whether a receiver token names an HTTP client: a
// constant ending in "Client", or a known client variable name. The base name
// after the last "::" is used so Net::HTTP and similar scoped receivers match.
func isHTTPClientReceiver(recv string) bool {
	base := recv
	if i := strings.LastIndex(base, "::"); i >= 0 {
		base = base[i+2:]
	}
	if strings.HasSuffix(base, "Client") {
		return true
	}
	return httpClientReceivers[strings.ToLower(base)]
}

// rubyFramework classifies the client framework from the receiver token.
func rubyFramework(recv string) string {
	base := recv
	if i := strings.LastIndex(base, "::"); i >= 0 {
		base = base[i+2:]
	}
	switch {
	case strings.EqualFold(base, "HTTP") || strings.Contains(recv, "Net::HTTP"):
		return "net-http"
	case strings.EqualFold(base, "faraday") || strings.EqualFold(base, "conn") || strings.EqualFold(base, "connection"):
		return "faraday"
	case strings.EqualFold(base, "typhoeus"):
		return "typhoeus"
	default:
		return "http-client"
	}
}

// cleanRubyPath turns a client call's URL literal into a matchable route path, or
// returns ok=false when it is not a backend path (external, empty, or fully
// dynamic). It skips absolute URLs, drops the query string, and collapses Ruby
// interpolations to the {} placeholder. Mirrors tsextractor.cleanTSPath.
func cleanRubyPath(raw string) (string, bool) {
	p := strings.TrimSpace(raw)
	if strings.HasPrefix(p, "http") {
		return "", false // absolute URL → third-party, not a cross-repo backend path
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = rubyInterpolation.ReplaceAllString(p, "{}")
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "", false
	}
	// An interpolation the pattern could not close — `#{ENV["HOST"]}` has a
	// bracket inside it — leaves a fragment behind. A path built from a value
	// this extractor cannot read is not a path, and emitting the fragment
	// produces a route fact named `#{ENV[`.
	if strings.Contains(p, "#{") || strings.Contains(p, "${") {
		return "", false
	}
	// Require at least one concrete (non-placeholder) segment so a fully dynamic
	// path (e.g. just "#{path}") is skipped.
	for _, seg := range strings.Split(p, "/") {
		if seg != "" && seg != "{}" {
			return p, true
		}
	}
	return "", false
}

// hintFromReceiver derives a provider hint from a wrapper-client constant by
// stripping a trailing "Client" and lowercasing, e.g. SvcCheckoutClient ->
// "svccheckout". Returns "" for plain variable receivers (conn, client, http).
func hintFromReceiver(recv string) string {
	base := recv
	if i := strings.LastIndex(base, "::"); i >= 0 {
		base = base[i+2:]
	}
	if !strings.HasSuffix(base, "Client") || base == "Client" {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(base, "Client"))
}

// envVarHint returns a provider hint derived from the first base-URL env var name
// found in s, or "" if none. See stripURLVarSuffix for the suffix rule.
func envVarHint(s string) string {
	if m := rubyEnvVar.FindStringSubmatch(s); m != nil {
		return stripURLVarSuffix(m[1])
	}
	return ""
}

// urlVarSuffixes are env-var name suffixes stripped (longest first) to recover
// the target-service token, e.g. CORE_HTTP_CLIENT_BASE_URL -> "core",
// XENDO_URL -> "xendo".
var urlVarSuffixes = []string{
	"_HTTP_CLIENT_BASE_URL", "_CLIENT_BASE_URL", "_SERVICE_URL",
	"_BASE_URL", "_API_URL", "_URL", "_HOST", "_ENDPOINT",
}

// stripURLVarSuffix removes the longest matching base-URL suffix from an env-var
// name and lowercases the remainder (dropping underscores), yielding a provider
// hint. An env name carrying NO base-URL suffix yields nothing: a rate-limit or
// token variable must never become a hint, because a garbage hint steers the
// cross-repo matcher toward a wrong edge — and a wrong edge is worse than none.
func stripURLVarSuffix(name string) string {
	for _, suf := range urlVarSuffixes {
		if strings.HasSuffix(name, suf) && len(name) > len(suf) {
			return strings.ReplaceAll(strings.ToLower(name[:len(name)-len(suf)]), "_", "")
		}
	}
	return ""
}

// rubyAPIHint returns the source file's base name without extension, used as the
// cross-repo linker's disambiguation hint.
func rubyAPIHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

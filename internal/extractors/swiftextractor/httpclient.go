package swiftextractor

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// swiftPathComponent matches a URLRequest path built from a base URL, capturing
// the path literal, e.g. baseURL.appendingPathComponent("settings/feedback").
var swiftPathComponent = regexp.MustCompile(`appendingPathComponent\(\s*"([^"]*)"`)

// swiftHTTPMethodExpr matches an HTTP-method assignment on a URLRequest in any of
// the forms Swift code uses in practice — a string literal (`request.httpMethod =
// "POST"`), an enum member (`request.httpMethod = HTTPMethod.post.rawValue`), or a
// leading-dot enum case (Alamofire `request.method = .post`). Capture group 1 is the
// string-literal verb; group 2 is the enum-case name. The caller validates whichever
// is present via mapSwiftMethod, so a non-method assignment (e.g. `= someVar`) is
// ignored rather than mistaken for a verb.
var swiftHTTPMethodExpr = regexp.MustCompile(`\.(?:httpMethod|method)\s*=\s*(?:"([A-Za-z]+)"|(?:(?:HTTPMethod|RequestMethod|HTTPMethodType)\.|\.)([A-Za-z]+)(?:\.rawValue)?)`)

// methodWindow is how many lines on either side of an appendingPathComponent call to
// scan for the associated method assignment — symmetric because the verb is often
// set on the request before the URL path is appended, not only after it.
const methodWindow = 5

// fileURLSignals are tokens that, when present on the same line as an
// appendingPathComponent call, mark the URL as a *file* URL rather than a network
// request — so the path is a local filesystem path, not an HTTP endpoint.
// appendingPathComponent is shared by file and network URLs, so this distinguishes
// the two without tracing the base variable across statements.
var fileURLSignals = []string{
	"fileURLWithPath", "FileManager", "temporaryDirectory",
	"cachesDirectory", "documentDirectory", "documentsDirectory",
}

// nonAPIExtensions are file extensions on a path's final segment that indicate a
// local media/document file rather than an HTTP endpoint. (.json/.md are
// intentionally absent: some APIs serve them, and the file-level network gate
// already excludes the build-tooling sources where they appear here.)
var nonAPIExtensions = map[string]bool{
	".mov": true, ".mp4": true, ".m4v": true, ".mp3": true, ".wav": true,
	".pdf": true, ".png": true, ".jpg": true, ".jpeg": true, ".heic": true,
	".gif": true, ".zip": true, ".plist": true, ".sqlite": true, ".bundle": true,
}

// extractURLSessionFactsWithDir is extractURLSessionFacts with an explicit module
// identity dir, so route facts declare into the file's resolved target module
// rather than its leaf directory.
func extractURLSessionFactsWithDir(src []byte, relFile, dir string) []facts.Fact {
	// Test sources build request URLs against fixtures (e.g. an "APIResponses.bundle"
	// path), which are not real endpoints — skip them so they don't emit phantom routes.
	if isSwiftTestFile(relFile) {
		return nil
	}
	// File-level gate: appendingPathComponent is also how Swift builds local file
	// URLs. Only treat it as a network call in files that actually use URLSession /
	// URLRequest, which excludes file-I/O and build-tooling sources outright.
	if !bytes.Contains(src, []byte("URLSession")) && !bytes.Contains(src, []byte("URLRequest")) {
		return nil
	}

	lines := strings.Split(string(src), "\n")
	api := swiftAPIHint(relFile)
	host := swiftExternalHost(string(src))

	var out []facts.Fact
	for i, line := range lines {
		pm := swiftPathComponent.FindStringSubmatch(line)
		if pm == nil {
			continue
		}
		// Skip file URLs that happen to live in a network file (e.g. a temp file
		// written before an upload): a file-URL signal on the line, or a media/doc
		// extension on the path, means this is filesystem I/O, not an endpoint.
		if isFileURLLine(line) {
			continue
		}
		path := cleanSwiftPath(pm[1])
		if path == "" || hasNonAPIExtension(path) {
			continue
		}
		method := methodNear(lines, i)
		props := map[string]any{
			facts.PropRole:   facts.RoleClient,
			"method":         method,
			"framework":      "urlsession",
			"language":       "swift",
			facts.PropSource: facts.RouteSourceURLSession,
			"api":            api,
		}
		applyExternal(props, host)
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      path,
			File:      relFile,
			Line:      i + 1,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// declaresFunc reports whether a line starts a function declaration.
func declaresFunc(line string) bool {
	return strings.Contains(" "+strings.TrimSpace(line), " func ")
}

// requestReach is how far below a path methodOfRequest follows the request built from
// it: past a multipart body or headers, never past the enclosing function.
const requestReach = 60

// swiftRequestDecl matches the declaration of a URLRequest variable, capturing its name.
var swiftRequestDecl = regexp.MustCompile(`\b(?:var|let)\s+(\w+)\s*(?::\s*URLRequest\s*)?=\s*URLRequest\(`)

// methodOfRequest returns the verb set on the first URLRequest declared at or below the
// path at idx, in the same function:
//
//	var components = URLComponents(url: baseURL.appendingPathComponent("x/analyze"), …)
//	… a multipart body, twenty lines …
//	var request = URLRequest(url: url)
//	request.httpMethod = "POST"
//
// or "" when there is none. methodNear's window misses it there, and the URLSession
// default it falls back to is GET.
func methodOfRequest(lines []string, idx int) string {
	var assign *regexp.Regexp
	for j := idx; j < len(lines) && j <= idx+requestReach; j++ {
		if j > idx && declaresFunc(lines[j]) {
			return ""
		}
		if assign == nil {
			if m := swiftRequestDecl.FindStringSubmatch(lines[j]); m != nil {
				assign = regexp.MustCompile(`\b` + regexp.QuoteMeta(m[1]) + `\.(?:httpMethod|method)\s*=`)
			}
			continue
		}
		if assign.MatchString(lines[j]) {
			return methodInLine(lines[j])
		}
	}
	return ""
}

// methodNear returns the HTTP method assigned within methodWindow lines of idx,
// scanning outward (nearest line wins, forward preferred on ties), else the one set
// on the request built from the path further down, or "GET" when no valid verb is
// found — the URLSession default.
func methodNear(lines []string, idx int) string {
	if v := methodInLine(lines[idx]); v != "" {
		return v
	}
	// The window stops at a function declaration either way: a verb set in another
	// function is another request's.
	down, up := true, true
	for d := 1; d <= methodWindow; d++ {
		if j := idx + d; down && j < len(lines) {
			if down = !declaresFunc(lines[j]); down {
				if v := methodInLine(lines[j]); v != "" {
					return v
				}
			}
		}
		if j := idx - d; up && j >= 0 {
			if up = !declaresFunc(lines[j]); up {
				if v := methodInLine(lines[j]); v != "" {
					return v
				}
			}
		}
	}
	if v := methodOfRequest(lines, idx); v != "" {
		return v
	}
	return "GET"
}

// methodInLine extracts and validates an HTTP verb from a single line's method
// assignment, or "" when the line has no assignment or its value is not a known verb.
func methodInLine(line string) string {
	m := swiftHTTPMethodExpr.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	tok := m[1] // string-literal form
	if tok == "" {
		tok = m[2] // enum-case form
	}
	return mapSwiftMethod(tok) // "" for anything that isn't a recognized verb
}

// cleanSwiftPath converts Swift interpolation to the {} placeholder the linker
// understands and strips any query string.
func cleanSwiftPath(p string) string {
	p = collapseSwiftInterpolation(p)
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// collapseSwiftInterpolation replaces each Swift string interpolation \(...) with
// a single {} placeholder, correctly handling nested parentheses (e.g.
// \(UUID().uuidString) collapses to {} rather than leaking ".uuidString)").
func collapseSwiftInterpolation(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '\\' && s[i+1] == '(' {
			depth, j := 1, i+2
			for j < len(s) && depth > 0 {
				switch s[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
				j++
			}
			b.WriteString("{}")
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// isFileURLLine reports whether a line building an appendingPathComponent path is
// rooted at a file URL rather than a network request.
func isFileURLLine(line string) bool {
	for _, sig := range fileURLSignals {
		if strings.Contains(line, sig) {
			return true
		}
	}
	return false
}

// hasNonAPIExtension reports whether the path's final segment ends in a local
// media/document file extension (so it is filesystem I/O, not an HTTP endpoint).
func hasNonAPIExtension(path string) bool {
	last := path
	if i := strings.LastIndexByte(last, '/'); i >= 0 {
		last = last[i+1:]
	}
	return nonAPIExtensions[strings.ToLower(filepath.Ext(last))]
}

// swiftAPIHint returns the source file's base name without extension (e.g.
// "EntitlementAPIService"), used as the cross-repo linker's disambiguation hint.
func swiftAPIHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// isSwiftTestFile reports whether a Swift source is test/fixture code (under a
// Tests directory or a *Test(s)/*Spec file), whose request-building is against
// fixtures, not real endpoints.
func isSwiftTestFile(relFile string) bool {
	slash := filepath.ToSlash(relFile)
	if strings.Contains(slash, "/Tests/") || strings.HasPrefix(slash, "Tests/") {
		return true
	}
	base := strings.TrimSuffix(filepath.Base(slash), filepath.Ext(slash))
	return strings.HasSuffix(base, "Test") || strings.HasSuffix(base, "Tests") ||
		strings.HasSuffix(base, "Spec") || strings.HasSuffix(base, "TestCase")
}

// --- endpoint-enum client detection (Moya TargetType-like idiom) ---

// swiftVarFinders locate the opening brace of an endpoint's computed properties.
// The idiom is convention-based (a path property + a `method` property, each a
// `switch self`), independent of any specific protocol or app name.
var swiftVarFinders = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, n := range []string{"urlPathComponent", "path", "urlPrefixComponent", "basePath", "method", "urlString", "baseURL", "baseURLString"} {
		m[n] = regexp.MustCompile(`var\s+` + n + `\s*:\s*[^{]*\{`)
	}
	return m
}()

// swiftBaseURLProps are the computed-property names that carry a client's base URL.
// When one of them yields a hardcoded absolute host, the calls are external.
var swiftBaseURLProps = []string{"urlString", "baseURL", "baseURLString"}

// swiftAbsoluteHost captures the host of an absolute URL literal (e.g. the
// "jira.service.com" in "https://jira.service.com/...").
var swiftAbsoluteHost = regexp.MustCompile(`"https?://([A-Za-z0-9.\-]+)`)

// swiftStoredBaseURLHost captures a hardcoded absolute host assigned to a base-URL
// identifier in stored form, e.g. `let baseURL = URL(string: "https://host/…")` or
// `let baseURL = "https://host"`.
var swiftStoredBaseURLHost = regexp.MustCompile(`(?:baseURL|urlString|baseURLString)\w*\s*(?::[^={\n]*)?=\s*(?:URL\(string:\s*)?"https?://([A-Za-z0-9.\-]+)`)

// swiftExternalHost returns the absolute host a client type targets when its base
// URL is a hardcoded https?:// literal (e.g. APIService.Jira -> "jira.service.com"),
// or "" when the base URL is injected/relative (an internal-candidate call). A
// hardcoded external host means the call leaves the cross-repo graph, so its routes
// are classified external rather than counted as unresolved internal edges.
func swiftExternalHost(text string) string {
	if body, _ := firstPropBody(text, swiftBaseURLProps); body != "" {
		if m := swiftAbsoluteHost.FindStringSubmatch(body); m != nil {
			return m[1]
		}
	}
	if m := swiftStoredBaseURLHost.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// applyExternal tags a client route's props as external when host is non-empty.
func applyExternal(props map[string]any, host string) {
	if host != "" {
		props["external"] = true
		props["host"] = host
	}
}

// swiftPathProps / swiftPrefixProps are the property names carrying an endpoint's
// path and its optional version/base prefix, most specific first.
var (
	swiftPathProps   = []string{"urlPathComponent", "path"}
	swiftPrefixProps = []string{"urlPrefixComponent", "basePath"}
)

// swiftReturnString captures the first string literal a `return` yields.
var swiftReturnString = regexp.MustCompile(`return\s+"([^"]*)"`)

// swiftReturnEnumCase captures the enum case a `return` yields, e.g. `.post`.
var swiftReturnEnumCase = regexp.MustCompile(`return\s+\.([A-Za-z]+)`)

// swiftCaseDot captures each `.label` token of a `case .a, .b(let x):` line.
var swiftCaseDot = regexp.MustCompile(`\.([A-Za-z_]\w*)`)

// extractEndpointFacts emits a client-route fact for every endpoint case in a
// Swift file that defines an endpoint-enum type: a type exposing a path computed
// property (urlPathComponent/path) and a `method` computed property, each a
// `switch self` keyed by enum case. Path, version prefix, and method are joined
// per case label. The version prefix is resolved in this order: the type's own
// per-case value, its switch `default:` branch, its single-value computed prefix,
// and finally defaultPrefix (the repo-wide protocol-extension default, e.g. "v2")
// when the type declares no prefix property at all. Returns nil for non-idiom files.
func extractEndpointFacts(src []byte, relFile, dir, defaultPrefix string) []facts.Fact {
	if isSwiftTestFile(relFile) {
		return nil
	}
	text := string(src)
	pathBody, pathIdx := firstPropBody(text, swiftPathProps)
	if pathBody == "" {
		return nil
	}
	methodBody, _ := firstPropBody(text, []string{"method"})
	if methodBody == "" {
		return nil // a path property without a method is not the endpoint idiom
	}
	pathByCase := switchReturns(pathBody, swiftReturnString)
	if len(pathByCase) == 0 {
		return nil
	}
	methodByCase := switchReturns(methodBody, swiftReturnEnumCase)
	// A single-value method property (`var method: HTTPMethod { return .post }`, no
	// switch) yields no per-case entries; read its lone returned verb and apply it to
	// every case, mirroring how prefixResolver handles a single-value prefix body.
	methodConst := ""
	if len(methodByCase) == 0 {
		if m := swiftReturnEnumCase.FindStringSubmatch(methodBody); m != nil {
			methodConst = m[1]
		}
	}
	resolvePrefix := prefixResolver(text, defaultPrefix)

	api := swiftAPIHint(relFile)
	host := swiftExternalHost(text)
	line := 1 + strings.Count(text[:pathIdx], "\n")

	var out []facts.Fact
	seen := map[string]bool{}
	for label, rawPath := range pathByCase {
		if label == swiftDefaultLabel {
			continue // a default: path branch has no concrete case to pair — skip
		}
		path := cleanSwiftPath(rawPath)
		if path == "" || hasNonAPIExtension(path) {
			continue
		}
		if prefix := resolvePrefix(label); prefix != "" {
			path = joinURLPath(prefix, path)
		}
		method := "GET"
		verb := methodByCase[label]
		if verb == "" {
			verb = methodByCase[swiftDefaultLabel]
		}
		if verb == "" {
			verb = methodConst
		}
		if v := mapSwiftMethod(verb); v != "" {
			method = v
		}
		key := method + "\x00" + path
		if seen[key] {
			continue
		}
		seen[key] = true
		props := map[string]any{
			facts.PropRole:   facts.RoleClient,
			"method":         method,
			"framework":      "apiendpoint",
			"language":       "swift",
			facts.PropSource: facts.RouteSourceSwiftEndpoint,
			"api":            api,
		}
		applyExternal(props, host)
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      path,
			File:      relFile,
			Line:      line,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// prefixResolver returns a function mapping an enum-case label to the endpoint's
// resolved version prefix (already cleaned). It reads the type's urlPrefixComponent
// property in either form — a `switch self` (per-case + optional `default:`) or a
// single-value computed body — and falls back to defaultPrefix only when the type
// declares no prefix property at all, so an explicit per-type prefix is never
// overridden.
func prefixResolver(text, defaultPrefix string) func(string) string {
	body, _ := firstPropBody(text, swiftPrefixProps)
	if body == "" {
		clean := resolvePrefixValue(defaultPrefix)
		return func(string) string { return clean }
	}
	if containsSwitch(body) {
		byCase := switchReturns(body, swiftReturnString)
		return func(label string) string {
			v, ok := byCase[label]
			if !ok {
				v = byCase[swiftDefaultLabel]
			}
			return resolvePrefixValue(v)
		}
	}
	// Single-value computed prefix (implicit return), applies to every case.
	clean := resolvePrefixValue(firstQuoted(body))
	return func(string) string { return clean }
}

// firstPropBody returns the brace-delimited body of the first computed property
// named one of `names`, plus the byte index where its declaration begins. Returns
// ("", 0) if none is present.
func firstPropBody(text string, names []string) (string, int) {
	for _, name := range names {
		re := swiftVarFinders[name]
		if re == nil {
			continue
		}
		loc := re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		if body, ok := braceBody(text, loc[1]-1); ok {
			return body, loc[0]
		}
	}
	return "", 0
}

// braceBody returns the text between the brace at open and its matching close.
// Swift string interpolation uses `\(…)` (not braces) and these property bodies
// are plain switches, so simple depth counting is sufficient.
func braceBody(text string, open int) (string, bool) {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[open+1 : i], true
			}
		}
	}
	return "", false
}

// swiftDefaultLabel is the sentinel key under which switchReturns records a
// `default:` branch's value, so callers can fall back to it for any case the
// switch does not name explicitly.
const swiftDefaultLabel = "\x00default"

// switchReturns maps each enum-case label in a `switch self` body to the value its
// branch returns, extracted by valueRe (group 1). Grouped cases ("case .a, .b:")
// share the value; a `default:` branch is recorded under swiftDefaultLabel; the
// first return wins per label.
func switchReturns(body string, valueRe *regexp.Regexp) map[string]string {
	out := map[string]string{}
	if body == "" {
		return out
	}
	var cur []string
	inLabels := false // accumulating a case-label list that spans multiple lines (before its ':')
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "case "):
			cur = caseLabels(t)
			// A label list may wrap onto following lines; keep collecting until the
			// clause's terminating colon so `case .a,\n .b:` maps both a and b.
			inLabels = !strings.Contains(t, ":")
		case strings.HasPrefix(t, "default:"), strings.HasPrefix(t, "default :"):
			cur = []string{swiftDefaultLabel}
			inLabels = false
		case inLabels:
			cur = append(cur, caseLabels(t)...)
			if strings.Contains(t, ":") {
				inLabels = false
			}
		}
		if m := valueRe.FindStringSubmatch(t); m != nil {
			for _, lbl := range cur {
				if _, ok := out[lbl]; !ok {
					out[lbl] = m[1]
				}
			}
		}
	}
	return out
}

// caseLabels extracts the enum-case labels of a `case .a, .b(let x):` line, up to
// the trailing colon (so a `.label` inside a where-clause value is not captured).
func caseLabels(line string) []string {
	if i := strings.IndexByte(line, ':'); i >= 0 {
		line = line[:i]
	}
	var out []string
	for _, m := range swiftCaseDot.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

// mapSwiftMethod maps an HTTPMethod enum case (get/post/…) to a verb, or "" if
// unrecognized (the caller defaults to GET).
func mapSwiftMethod(v string) string {
	switch strings.ToLower(v) {
	case "get", "post", "put", "patch", "delete", "head", "options":
		return strings.ToUpper(v)
	}
	return ""
}

// joinURLPath joins a prefix and path with a single slash, trimming surrounding
// slashes (e.g. "v2" + "orders.json" -> "v2/orders.json").
func joinURLPath(a, b string) string {
	a, b = strings.Trim(a, "/"), strings.Trim(b, "/")
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "/" + b
}

// swiftFirstQuoted captures the first double-quoted string literal in a body.
var swiftFirstQuoted = regexp.MustCompile(`"([^"]*)"`)

// swiftVersionInterp matches a Swift interpolation whose expression names a
// version constant, e.g. `\(APIVersion.v3)` or `\(.v2)`, capturing the digits.
// Prefix segments interpolate a version enum rather than a runtime value, so this
// resolves to the version token instead of the generic {} path-parameter placeholder.
var swiftVersionInterp = regexp.MustCompile(`\\\([^)]*?\bv(\d+)\b[^)]*?\)`)

// firstQuoted returns the first double-quoted string literal in s, or "".
func firstQuoted(s string) string {
	if m := swiftFirstQuoted.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// containsSwitch reports whether a property body is a `switch self` (vs a
// single-value computed body).
func containsSwitch(body string) bool {
	return strings.Contains(body, "switch") || strings.Contains(body, "case ")
}

// resolvePrefixValue turns a raw urlPrefixComponent literal into a clean path
// prefix: a version-constant interpolation `\(…vN…)` becomes `vN` (so
// "core/\(APIVersion.v3)" -> "core/v3"), then cleanSwiftPath collapses any other
// interpolation and strips a query string.
func resolvePrefixValue(s string) string {
	if s == "" {
		return ""
	}
	s = swiftVersionInterp.ReplaceAllString(s, "v$1")
	return cleanSwiftPath(s)
}

// swiftPrefixRequirement matches the endpoint protocol's urlPrefixComponent
// requirement (`var urlPrefixComponent: <Type> { get }`), which uniquely marks the
// file that declares the endpoint protocol — where the repo-wide default lives.
var swiftPrefixRequirement = regexp.MustCompile(`urlPrefixComponent\s*:\s*[\w.]+\s*\{\s*get\b`)

// detectDefaultURLPrefix returns the repo-wide default urlPrefixComponent value
// (e.g. "v2") declared in the endpoint protocol's extension, or "" if none. It
// looks only in the file that declares the protocol requirement, so a concrete
// type's single-value override (e.g. `{ "core/v3" }`) cannot be mistaken for the
// default. Meant to run once per repo before the per-file walk.
func detectDefaultURLPrefix(repoPath string, files []string) string {
	for _, relFile := range files {
		if !isSwiftFile(relFile) || isSwiftTestFile(relFile) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		text := string(src)
		if !swiftPrefixRequirement.MatchString(text) {
			continue
		}
		if v := defaultPrefixLiteral(text); v != "" {
			return v
		}
	}
	return ""
}

// defaultPrefixLiteral scans every urlPrefixComponent computed property in a
// protocol-declaring file and returns the first single-value string literal it
// finds (the extension default), skipping the `{ get }` requirement and any
// `switch`-based body.
func defaultPrefixLiteral(text string) string {
	re := swiftVarFinders["urlPrefixComponent"]
	for _, loc := range re.FindAllStringIndex(text, -1) {
		body, ok := braceBody(text, loc[1]-1)
		if !ok || containsSwitch(body) {
			continue
		}
		if v := firstQuoted(body); v != "" {
			return resolvePrefixValue(v)
		}
	}
	return ""
}

// --- call-site-driven endpoint idioms (cross-file resolution) ---
//
// Two endpoint idioms cannot be resolved from the type alone because part of the
// request is supplied at each instantiation site. Both are resolved by indexing the
// types by shape and reading the relevant init arguments at every call site.
//
// 1. Stored-method structs — path (and prefix) computed on the type, but `method` a
//    STORED property set by the caller:
//
//	struct SomeEndpoint: EndpointProtocol {
//	    var urlPrefixComponent: String { "core/v3" }
//	    var urlPathComponent: String { "resource/\(id)/action" }
//	    let method: HTTPMethod   // supplied at the call site, e.g. .post / .delete
//	}
//
// 2. Request wrappers — the PATH itself is a stored, required property supplied at
//    init; the prefix and a default verb live on the type:
//
//	struct SomeRequest: EndpointProtocol {
//	    var urlPrefixComponent = "core/v3"
//	    var urlPathComponent: String            // supplied at the call site
//	    var method: HTTPMethod = .get           // default; overridable per call site
//	}
//	// SomeRequest(urlPathComponent: "resource/\(id)", method: .delete)
//
// extractEndpointFacts requires BOTH a computed path and a computed method, so it
// skips both idioms; they are handled here.

// swiftTypeDecl matches a struct/class/enum/actor declaration, capturing the type
// name. Access modifiers and attributes preceding the keyword are tolerated because
// the match anchors on the keyword itself.
var swiftTypeDecl = regexp.MustCompile(`\b(?:struct|final\s+class|class|enum|actor)\s+(\w+)`)

// swiftStoredMethod matches a STORED `method` property (`let/var method: <Type>`,
// no computed `{ … }` body). Distinguishing a stored from a computed method is what
// separates this pass from extractEndpointFacts, so the caller also confirms the
// computed finder (swiftVarFinders["method"]) does NOT match the same body.
var swiftStoredMethod = regexp.MustCompile(`\b(?:let|var)\s+method\s*:\s*[A-Za-z_][\w.<>]*`)

// swiftConstructorCall matches a capitalized `TypeName(` call head, capturing the
// type name. The trailing `(` is where the argument span begins.
var swiftConstructorCall = regexp.MustCompile(`\b([A-Z]\w*)\s*\(`)

// swiftVerbArg matches a direct enum-literal method initializer argument
// (`method: .post` or `httpMethod: .put`), capturing the verb token. The leading-dot
// anchor excludes dynamically-computed verbs (`method: cond ? .put : .post`), which
// are out of scope.
var swiftVerbArg = regexp.MustCompile(`\b(?:method|httpMethod)\s*:\s*\.([A-Za-z]+)`)

// swiftPathArg matches a string-literal urlPathComponent initializer argument
// (`urlPathComponent: "posts/\(id)"`), capturing the literal. Used for request-wrapper
// endpoints where the path is supplied per call site.
var swiftPathArg = regexp.MustCompile(`\burlPathComponent\s*:\s*"([^"]*)"`)

// swiftStoredPathProp matches a STORED, required urlPathComponent property
// (`var urlPathComponent: String` with no computed `{ … }` body and no `=` default) on
// its own line — the marker of a request-wrapper type whose path is supplied at init.
var swiftStoredPathProp = regexp.MustCompile(`(?m)^\s*(?:let|var)\s+urlPathComponent\s*:\s*String\s*(?://.*)?$`)

// swiftStoredPrefix matches a stored urlPrefixComponent with a string-literal default
// (`var urlPrefixComponent = "core/v3"`), capturing the literal. prefixResolver only
// reads computed prefixes, so wrapper types (which store the prefix) need this.
var swiftStoredPrefix = regexp.MustCompile(`\b(?:let|var)\s+urlPrefixComponent\b[^={\n]*=\s*"([^"]*)"`)

// swiftStoredVerbDefault matches a stored method/httpMethod property with an
// enum-literal default (`var method: HTTPMethod = .get`, `var httpMethod: HTTPMethod = .post`),
// capturing the verb — a wrapper type's default verb when a call site omits it.
var swiftStoredVerbDefault = regexp.MustCompile(`\b(?:method|httpMethod)\s*(?::[^=\n]*)?=\s*\.([A-Za-z]+)`)

// storedEndpointDef is a stored-method endpoint type discovered in one file: its
// simple type name, the resolved (prefix-joined) URL path, and where it lives.
type storedEndpointDef struct {
	typeName string
	path     string
	file     string
	line     int
	dir      string
}

// storedMethodEndpointDefs finds every stored-method endpoint type declared in a
// single Swift source and resolves each one's URL path (prefix joined). It is pure
// and per-file; the verbs are supplied later from call sites. A type qualifies when
// its body has a single-value (non-switch) path property yielding a string literal
// and a STORED (non-computed) `method` property.
func storedMethodEndpointDefs(text, relFile, dir, defaultPrefix string) []storedEndpointDef {
	var out []storedEndpointDef
	for _, loc := range swiftTypeDecl.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		// The type body starts at the first brace after the declaration head.
		rel := strings.IndexByte(text[loc[1]:], '{')
		if rel < 0 {
			continue
		}
		body, ok := braceBody(text, loc[1]+rel)
		if !ok {
			continue
		}
		// A stored `method` (and not a computed one) is the shape gate.
		if !swiftStoredMethod.MatchString(body) {
			continue
		}
		if computed, _ := firstPropBody(body, []string{"method"}); computed != "" {
			continue // computed method -> handled by extractEndpointFacts, not here
		}
		pathBody, _ := firstPropBody(body, swiftPathProps)
		if pathBody == "" || containsSwitch(pathBody) {
			continue // no single-value path property to resolve
		}
		path := cleanSwiftPath(firstQuoted(pathBody))
		if path == "" || hasNonAPIExtension(path) {
			continue
		}
		if prefix := prefixResolver(body, defaultPrefix)(""); prefix != "" {
			path = joinURLPath(prefix, path)
		}
		out = append(out, storedEndpointDef{
			typeName: name,
			path:     path,
			file:     relFile,
			line:     1 + strings.Count(text[:loc[0]], "\n"),
			dir:      dir,
		})
	}
	return out
}

// endpointCallSite is one constructor call of a candidate endpoint type: the type
// instantiated, the top-level `urlPathComponent:` literal (for wrapper types) and/or
// `method:`/`httpMethod:` verb passed, and the call's location.
type endpointCallSite struct {
	typeName    string
	pathArg     string // urlPathComponent: literal, if a top-level string literal
	pathLiteral bool
	verb        string // method:/httpMethod: enum-literal verb, if passed
	file        string
	line        int
}

// endpointCallSites scans a single Swift source for constructor calls, recording per
// call the type name plus any top-level `urlPathComponent:` literal and
// `method:`/`httpMethod:` verb argument. Only top-level arguments are read (via
// depthAt), so an argument inside a nested call is attributed to that nested type.
func endpointCallSites(text, relFile string) []endpointCallSite {
	var out []endpointCallSite
	for _, loc := range swiftConstructorCall.FindAllStringSubmatchIndex(text, -1) {
		span, ok := parenBody(text, loc[1]-1) // loc[1]-1 is the '(' of the call head
		if !ok {
			continue
		}
		rec := endpointCallSite{
			typeName: text[loc[2]:loc[3]],
			file:     relFile,
			line:     1 + strings.Count(text[:loc[0]], "\n"),
		}
		for _, m := range swiftPathArg.FindAllStringSubmatchIndex(span, -1) {
			if depthAt(span[:m[0]]) != 0 {
				continue
			}
			rec.pathArg, rec.pathLiteral = span[m[2]:m[3]], true
			break
		}
		for _, m := range swiftVerbArg.FindAllStringSubmatchIndex(span, -1) {
			if depthAt(span[:m[0]]) != 0 {
				continue
			}
			rec.verb = span[m[2]:m[3]]
			break
		}
		out = append(out, rec)
	}
	return out
}

// wrapperDef is a request-wrapper endpoint type: the path is supplied at each call
// site (a stored `urlPathComponent: String`), while the prefix and default verb live
// on the type.
type wrapperDef struct {
	typeName    string
	prefix      string
	defaultVerb string // an uppercase HTTP verb (the call-site fallback)
	dir         string
}

// wrapperEndpointDefs finds request-wrapper endpoint types declared in one Swift
// source: a type whose body stores a required `urlPathComponent: String` (the path is
// an init argument) together with an endpoint signal (a prefix or method/httpMethod
// property). Pure and per-file; the paths and verbs come from call sites.
func wrapperEndpointDefs(text, relFile, dir, defaultPrefix string) []wrapperDef {
	var out []wrapperDef
	for _, loc := range swiftTypeDecl.FindAllStringSubmatchIndex(text, -1) {
		name := text[loc[2]:loc[3]]
		rel := strings.IndexByte(text[loc[1]:], '{')
		if rel < 0 {
			continue
		}
		body, ok := braceBody(text, loc[1]+rel)
		if !ok {
			continue
		}
		if !swiftStoredPathProp.MatchString(body) {
			continue // path is not supplied at init — not a wrapper
		}
		if !strings.Contains(body, "urlPrefixComponent") && !hasVerbSignal(text) {
			continue // no endpoint signal — avoid grabbing an arbitrary type
		}
		out = append(out, wrapperDef{
			typeName:    name,
			prefix:      wrapperPrefix(body, defaultPrefix),
			defaultVerb: wrapperDefaultVerb(text, body),
			dir:         dir,
		})
	}
	return out
}

// hasVerbSignal reports whether a source declares a method/httpMethod verb — a stored
// default or a computed `method` property — used as an endpoint signal for wrappers.
func hasVerbSignal(text string) bool {
	if swiftStoredVerbDefault.MatchString(text) {
		return true
	}
	mb, _ := firstPropBody(text, []string{"method"})
	return mb != ""
}

// wrapperPrefix resolves a wrapper type's URL prefix: a stored literal default first
// (prefixResolver reads only computed prefixes), else the computed/default resolution.
func wrapperPrefix(body, defaultPrefix string) string {
	if m := swiftStoredPrefix.FindStringSubmatch(body); m != nil {
		return resolvePrefixValue(m[1])
	}
	return prefixResolver(body, defaultPrefix)("")
}

// wrapperDefaultVerb resolves a wrapper type's default verb: a stored
// method/httpMethod default first, else a single-value computed `method` (e.g. one
// fixed in the conformance extension), else GET.
func wrapperDefaultVerb(fileText, body string) string {
	if m := swiftStoredVerbDefault.FindStringSubmatch(body); m != nil {
		if v := mapSwiftMethod(m[1]); v != "" {
			return v
		}
	}
	if mb, _ := firstPropBody(fileText, []string{"method"}); mb != "" && !containsSwitch(mb) {
		if cm := swiftCaseDot.FindStringSubmatch(mb); cm != nil {
			if v := mapSwiftMethod(cm[1]); v != "" {
				return v
			}
		}
	}
	return "GET"
}

// parenBody returns the text between the '(' at open and its matching ')', ignoring
// parentheses inside double-quoted string literals (so a ')' in a Swift string does
// not close the span early). text[open] must be '('.
func parenBody(text string, open int) (string, bool) {
	depth, inStr := 0, false
	for i := open; i < len(text); i++ {
		c := text[i]
		if inStr {
			switch c {
			case '\\':
				i++ // skip the escaped character
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[open+1 : i], true
			}
		}
	}
	return "", false
}

// depthAt returns the net bracket nesting depth of s — how many (, [ or { are still
// open at its end — ignoring brackets inside double-quoted string literals. Used to
// keep call-site argument scanning at the top level of a single constructor call.
func depthAt(s string) int {
	depth, inStr := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth
}

// extractCallSiteEndpointFacts resolves the two call-site-driven endpoint idioms
// across a repository and emits client-route facts:
//   - stored-method structs: the path lives on the type, the verb is read from each
//     instantiation site's `method:` argument (route anchored at the definition).
//   - request wrappers: the path is supplied at each call site's `urlPathComponent:`
//     argument and the verb from `method:`/`httpMethod:` (or the type's default);
//     each call site is a distinct route, anchored at the call site.
//
// A type with no discoverable (resolvable) call site emits nothing. The fact shape
// matches extractEndpointFacts, so the cross-repo linker treats these identically.
// Meant to run once per repo (iOS only).
func extractCallSiteEndpointFacts(repoPath string, files []string, defaultPrefix string, moduleForFile func(string) string) []facts.Fact {
	var storedDefs []storedEndpointDef
	var wrappers []wrapperDef
	var callSites []endpointCallSite

	for _, relFile := range files {
		if !isSwiftFile(relFile) || isSwiftTestFile(relFile) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		text := string(src)
		dir := moduleForFile(relFile)
		storedDefs = append(storedDefs, storedMethodEndpointDefs(text, relFile, dir, defaultPrefix)...)
		wrappers = append(wrappers, wrapperEndpointDefs(text, relFile, dir, defaultPrefix)...)
		callSites = append(callSites, endpointCallSites(text, relFile)...)
	}

	storedByType := map[string][]storedEndpointDef{}
	for _, d := range storedDefs {
		storedByType[d.typeName] = append(storedByType[d.typeName], d)
	}
	wrapperByType := map[string]wrapperDef{}
	for _, d := range wrappers {
		wrapperByType[d.typeName] = d
	}

	var out []facts.Fact
	seen := map[string]bool{}
	emit := func(dir, method, path, file string, line int) {
		key := dir + "\x00" + method + "\x00" + path
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: path,
			File: file,
			Line: line,
			Props: map[string]any{
				facts.PropRole:   facts.RoleClient,
				"method":         method,
				"framework":      "apiendpoint",
				"language":       "swift",
				facts.PropSource: facts.RouteSourceSwiftEndpoint,
				"api":            swiftAPIHint(file),
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}

	for _, cs := range callSites {
		if defs, ok := storedByType[cs.typeName]; ok {
			// Path is on the type; the call site only supplies the verb.
			method := mapSwiftMethod(cs.verb)
			if method == "" {
				continue
			}
			for _, d := range defs {
				emit(d.dir, method, d.path, d.file, d.line)
			}
			continue
		}
		if d, ok := wrapperByType[cs.typeName]; ok {
			// Path and verb come from the call site (verb falls back to the default).
			if !cs.pathLiteral {
				continue
			}
			path := cleanSwiftPath(cs.pathArg)
			if path == "" || hasNonAPIExtension(path) {
				continue
			}
			path = joinURLPath(d.prefix, path)
			method := mapSwiftMethod(cs.verb)
			if method == "" {
				method = d.defaultVerb
			}
			emit(moduleForFile(cs.file), method, path, cs.file, cs.line)
		}
	}
	return out
}

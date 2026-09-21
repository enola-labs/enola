package scalaextractor

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Storage and outbound HTTP for Scala.
//
// What is DELIBERATELY not here is SQL string literals. Every other extractor that
// reads them (Go, PHP) works on application code, where a `CREATE TABLE` literal is
// the application declaring its own schema. Scala's corpus contains a SQL ENGINE, and
// 198 of its files hold such literals — grammar fixtures, planner tests, documentation
// examples — none of which are storage this application owns. A rule that cannot tell
// a query from a parser test would attribute a few hundred phantom tables to the one
// repository most likely to be analysed for its data model. The signal is available
// through Slick and the topic literals below without that hazard; see
// docs/extraction/scala.md for the stated limit.

// slickTableRe matches Slick's table definition, which names the physical table in a
// string literal on the class header:
//
//	class Users(tag: Tag) extends Table[User](tag, "users")
//
// The class name is the Scala type; the literal is the table. Both are recorded,
// because a schema review asks about the table and a code review asks about the type.
var slickTableRe = regexp.MustCompile(`extends\s+Table\s*\[[^\]]*\]\s*\(\s*\w+\s*,\s*"([^"]+)"`)

// topicSuffixRe recognises a val whose NAME says it holds a topic. Anchoring on the
// name rather than on the value is what keeps this from tagging every string
// constant: a topic is architecture, an arbitrary literal is not.
var topicSuffixRe = regexp.MustCompile(`(?i)(topic|stream|queue)$`)

// extractStorage emits storage facts for the two forms Scala states explicitly:
// a Slick table's physical name, and a messaging topic named by a constant.
//
// Both are literal-anchored. A table name computed at runtime and a topic read from
// configuration are invisible here, which is the honest outcome — the alternative is
// guessing at names that then fail to match anything on the other side of a
// cross-repo join.
func extractStorage(root *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	var out []facts.Fact

	// Slick tables. Matched on the source text rather than the AST because the
	// declaration is a class header whose shape (`extends Table[T](tag, "name")`)
	// is a single regular form, and the walker already has the class facts.
	for _, m := range slickTableRe.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		out = append(out, facts.Fact{
			Kind: facts.KindStorage,
			Name: name,
			File: relFile,
			Line: lineOfOffset(src, m[0]),
			Props: map[string]any{
				"language":          "scala",
				"storage_kind":      "table",
				facts.PropFramework: "slick",
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}

	out = append(out, extractTopics(root, src, relFile, dir)...)
	return out
}

// extractTopics emits a topic storage fact per `val ...Topic = "literal"`.
//
// Asynchronous coupling is the one dependency a call graph structurally cannot see:
// a service consuming another's events shares no import, no call and no HTTP route
// with it. The cross-repo linker binds these into a consumer -> producer edge by
// topic-name ownership, which is why the fact is worth emitting even though it names
// nothing inside this repository.
func extractTopics(root *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	if !importsMessaging(src) {
		return nil
	}
	w := &dslWalker{src: src, relFile: relFile, dir: dir}
	var out []facts.Fact
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(n) {
		case "val_definition", "var_definition":
			name, value := w.valNameAndLiteral(n)
			if name != "" && value != "" && topicSuffixRe.MatchString(name) && plausibleTopicName(value) {
				out = append(out, facts.Fact{
					Kind: facts.KindStorage,
					Name: value,
					File: relFile,
					Line: int(n.StartPosition().Row) + 1,
					Props: map[string]any{
						"language":          "scala",
						"storage_kind":      facts.StorageKindTopic,
						facts.PropMessaging: "kafka",
						facts.PropSource:    "scala-topic-const",
						"declared_as":       name,
					},
					Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
				})
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out
}

// messagingImports name a broker client. Their presence is what makes a val called
// `somethingTopic` a MESSAGING topic rather than a domain noun.
var messagingImports = []string{
	"org.apache.kafka", "kafka.", "fs2.kafka", "akka.kafka", "org.apache.pekko.kafka",
	"pulsar", "com.sksamuel.avro4s", "amqp", "rabbitmq", "jms",
	"software.amazon.awssdk.services.sqs", "software.amazon.awssdk.services.sns",
}

// importsMessaging gates topic extraction on the file actually talking to a broker.
//
// Like the Pekko `path(...)` gate, this is a measured correction rather than caution.
// `topic` is an ordinary domain noun: a forum has topics, a moderation log has
// `closeTopic`/`hideTopic` actions, a search index has topics. Without the gate, one
// corpus application contributed seven "Kafka topics" that were all forum moderation
// constants — and a topic fact is not inert, because the cross-repo linker turns it
// into a producer/consumer edge between services by name ownership. A phantom topic
// therefore invents asynchronous coupling between repositories that share nothing.
func importsMessaging(src []byte) bool {
	return containsAny(src, messagingImports)
}

// valNameAndLiteral returns a val's bound name and its string-literal value, or
// empty strings when either is absent (a computed value, a destructuring pattern).
func (w *dslWalker) valNameAndLiteral(n *sitter.Node) (name, value string) {
	p := n.ChildByFieldName("pattern")
	if p == nil {
		p = n.ChildByFieldName("name")
	}
	if p == nil || kindOf(p) != "identifier" {
		return "", ""
	}
	v := n.ChildByFieldName("value")
	if v == nil || kindOf(v) != "string" {
		return "", ""
	}
	lit := w.text(v)
	if strings.Contains(lit, "$") {
		return "", "" // interpolated: the name is not knowable here
	}
	return w.text(p), strings.Trim(lit, `"`)
}

// plausibleTopicName rejects values that cannot be a topic — an empty string, a
// path, a URL — so a constant that merely ends in `Stream` does not become one.
func plausibleTopicName(v string) bool {
	if v == "" || len(v) > 200 {
		return false
	}
	return !strings.ContainsAny(v, " /\\:?#") && !strings.HasPrefix(v, ".")
}

func lineOfOffset(b []byte, off int) int {
	if off > len(b) {
		off = len(b)
	}
	return bytes.Count(b[:off], []byte("\n")) + 1
}

// containsAny reports whether src holds any of the needles.
//
// It exists so an import gate never copies the whole file to run a substring test:
// the gates used to do `strings.Contains(string(src), imp)`, which allocated a copy
// of every Scala source in the repository. The []byte conversion below does not
// escape, so it costs no allocation at all.
func containsAny(src []byte, needles []string) bool {
	for _, n := range needles {
		if bytes.Contains(src, []byte(n)) {
			return true
		}
	}
	return false
}

// --- outbound HTTP clients ---

// scalaClientVerbs are the request methods the supported clients expose. A call to
// one with a literal path is a call site the cross-repo linker can match against the
// service that serves it.
var scalaClientVerbs = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
	"patch": "PATCH", "head": "HEAD", "options": "OPTIONS",
}

// clientImports gate the extraction the same way the Pekko routing import does, and
// for the same measured reason: `get`, `post` and `put` are among the most common
// method names in any codebase, and matching them unqualified would turn every
// map lookup and every builder into an outbound HTTP call.
var clientImports = []string{
	"sttp.client", "sttp.model", "play.api.libs.ws", "org.http4s.client",
	"org.apache.pekko.http.scaladsl.Http", "akka.http.scaladsl.Http",
	"scalaj.http", "requests.",
}

// extractHTTPClients emits a client-role route fact per outbound call with a literal
// path, so the cross-repo linker can join it to the endpoint that serves it.
//
// Only relative paths are kept as linkable. An absolute `http(s)://` URL names a
// third party rather than a sibling service, and recording it as an unresolved
// internal edge would show up as a coverage gap that no amount of loading more repos
// could ever close.
func extractHTTPClients(root *sitter.Node, src []byte, relFile, dir string) []facts.Fact {
	if !importsScalaHTTPClient(src) {
		return nil
	}
	w := &dslWalker{src: src, relFile: relFile, dir: dir}
	var out []facts.Fact
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if kindOf(n) == "call_expression" {
			if method, path, ok := w.clientCall(n); ok {
				props := map[string]any{
					"language":       "scala",
					facts.PropSource: facts.RouteSourceScalaHTTPClient,
					facts.PropRole:   facts.RoleClient,
					"method":         method,
					"path":           path,
				}
				if host, ext := externalHost(path); ext {
					props["external"] = true
					props["host"] = host
				}
				out = append(out, facts.Fact{
					Kind:      facts.KindRoute,
					Name:      path,
					File:      relFile,
					Line:      int(n.StartPosition().Row) + 1,
					Props:     props,
					Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
				})
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return out
}

func importsScalaHTTPClient(src []byte) bool {
	return containsAny(src, clientImports)
}

// clientCall recognises `client.get(uri"/x")`, `ws.url("/x").get()` and the sttp
// `basicRequest.get(...)` chain, returning the verb and the literal path.
func (w *dslWalker) clientCall(n *sitter.Node) (method, path string, ok bool) {
	fn := firstNamedChild(n)
	if fn == nil {
		return "", "", false
	}
	if kindOf(fn) == "generic_function" {
		fn = firstNamedChild(fn)
	}
	if fn == nil || kindOf(fn) != "field_expression" {
		return "", "", false
	}
	_, name := w.splitFieldExpressionDSL(fn)
	verb, isVerb := scalaClientVerbs[name]
	if !isVerb && name != "url" && name != "uri" {
		return "", "", false
	}
	lit := w.firstStringArgument(n)
	if lit == "" {
		return "", "", false
	}
	if !isVerb {
		// `ws.url("/x")` names the path; the verb arrives later in the chain and is
		// not resolvable from this node, so the call is recorded as method-agnostic
		// rather than attributed to a guessed verb.
		return facts.MethodAny, lit, true
	}
	return verb, lit, true
}

func (w *dslWalker) splitFieldExpressionDSL(fn *sitter.Node) (recv *sitter.Node, name string) {
	var named []*sitter.Node
	for i := uint(0); i < fn.ChildCount(); i++ {
		if c := fn.Child(i); c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) == 0 {
		return nil, ""
	}
	name = w.text(named[len(named)-1])
	if len(named) >= 2 {
		recv = named[len(named)-2]
	}
	return recv, name
}

// firstStringArgument returns the first literal string argument of a call, with any
// interpolator prefix (`uri"..."`) stripped. An interpolated literal returns "" —
// the path is not knowable without evaluating the expression.
func (w *dslWalker) firstStringArgument(n *sitter.Node) string {
	var found string
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if kindOf(c) != "arguments" {
			continue
		}
		for j := uint(0); j < c.ChildCount(); j++ {
			a := c.Child(j)
			switch kindOf(a) {
			case "string":
				found = strings.Trim(w.text(a), `"`)
			case "interpolated_string_expression":
				txt := w.text(a)
				if strings.Contains(txt, "$") {
					return "" // holes make the path unknowable
				}
				if i := strings.Index(txt, `"`); i >= 0 {
					found = strings.Trim(txt[i:], `"`)
				}
			}
			if found != "" {
				break
			}
		}
		break
	}
	if found == "" || strings.Contains(found, "$") {
		return ""
	}
	if !strings.HasPrefix(found, "/") && !strings.HasPrefix(found, "http") {
		return "" // not a path: a header name, a parameter, a body
	}
	return found
}

// externalHost reports the host of an absolute URL, so a third-party call is not
// mistaken for an unresolved internal edge.
func externalHost(path string) (string, bool) {
	for _, scheme := range []string{"http://", "https://"} {
		if rest, found := strings.CutPrefix(path, scheme); found {
			host := rest
			if i := strings.Index(host, "/"); i >= 0 {
				host = host[:i]
			}
			return host, true
		}
	}
	return "", false
}

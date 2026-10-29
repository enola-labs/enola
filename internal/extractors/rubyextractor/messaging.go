package rubyextractor

import (
	"bytes"
	"strconv"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// --- event publications and subscriptions ---
//
// A Rails application publishing to a broker does it through a client of its own or a
// gem's, so there is no import to recognize the way the Go extractor recognizes a Kafka
// client. Two shapes are read instead, both of which name the topic:
//
//   - a publication: a call to a publish/produce/deliver method with a `topic:`
//     keyword argument, `broker.publish_event(payload, topic: TOPIC, key: id)`;
//   - a subscription: a configuration block assigning a topic and the handler class
//     together, `config.subscribe(...) { |s| s.topic = "…"; s.handler = OrderPlaced }`.
//
// The topic is a string literal or a constant resolving to one through the class's
// constants (`TOPIC = SCHEMA_SUBJECT = "…"`). An interpolated segment becomes a
// parameter named for what it interpolates, as an AsyncAPI channel writes it:
// "core.#{PORTAL}.events" is "core.{portal}.events". A topic read at runtime from
// anything else is not guessed. The protocol is not stated: nothing in the source
// says which broker the client talks to.

// A publication is read from three shapes, all naming the topic under a `topic:` key:
// a call to one of rubyPublishMethods; any other call, an outbox writer's
// `CreateMessage.run(topic: …)`, when the topic is a dotted name; and a message
// envelope, a hash carrying `topic:` together with `payload:`, built to be handed to
// a batch publish. A dotted name is what tells a topic from a `topic:` that is a
// subject line or a category.

// rubyPublishMethods are the method names a publication is made through.
var rubyPublishMethods = map[string]bool{
	"publish": true, "publish_event": true, "publish_events": true, "publish_message": true,
	"produce": true, "produce_sync": true, "produce_async": true, "deliver_message": true,
}

// rubyConstantHops bounds how many constant-to-constant assignments a topic follows.
const rubyConstantHops = 4

type rubyMessagingWalker struct {
	src     []byte
	relFile string
	dir     string
	// consts holds each constant's value expression by qualified name ("A::B::TOPIC").
	consts map[string]*sitter.Node
	out    []facts.Fact
	seen   map[string]bool
	// pending are operations whose topic is a constant this file does not define,
	// resolved across the repository once every file is read.
	pending []rubyPendingTopic
}

// rubyTopicConst is a constant a file defines whose value reads as a topic name.
type rubyTopicConst struct {
	name  string // qualified, "A::B::TOPIC"
	value string
}

// rubyPendingTopic is a topic operation waiting on a constant another file defines.
type rubyPendingTopic struct {
	ref    string // as written, "TOPIC" or "A::B::TOPIC"
	dotted bool   // the topic must be a dotted name to count
	fact   facts.Fact
}

// extractRubyMessagingFacts reads a file's event publications and subscriptions as
// topic-operation facts. It also returns the file's topic-valued constants and the
// operations naming a constant the file does not define, for
// resolveRubyPendingTopics.
func extractRubyMessagingFacts(src []byte, relFile string) ([]facts.Fact, []rubyTopicConst, []rubyPendingTopic) {
	if !bytes.Contains(src, []byte("topic")) && !bytes.Contains(src, []byte("TOPIC")) {
		return nil, nil, nil
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return nil, nil, nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil, nil, nil
	}
	defer tree.Close()

	w := &rubyMessagingWalker{src: src, relFile: relFile, dir: factpath.Dir(relFile),
		consts: map[string]*sitter.Node{}, seen: map[string]bool{}}
	w.collectConstants(tree.RootNode(), nil)
	w.walk(tree.RootNode(), nil, "")

	var consts []rubyTopicConst
	for name, v := range w.consts {
		if topic := w.topic(v, constScope(name), 0); strings.Contains(topic, ".") {
			consts = append(consts, rubyTopicConst{name: name, value: topic})
		}
	}
	return w.out, consts, w.pending
}

// constScope returns the scope a qualified constant is defined in.
func constScope(name string) []string {
	parts := strings.Split(name, "::")
	return parts[:len(parts)-1]
}

// resolveRubyPendingTopics names the operations waiting on another file's constant:
// the constant of that qualified name, else the one constant of that bare name in
// the repository. A name two constants share resolves nothing.
func resolveRubyPendingTopics(pending []rubyPendingTopic, consts []rubyTopicConst) []facts.Fact {
	byName := map[string]string{}
	byBare := map[string]string{}
	bareCount := map[string]int{}
	for _, c := range consts {
		byName[c.name] = c.value
		bare := c.name
		if i := strings.LastIndex(bare, "::"); i >= 0 {
			bare = bare[i+2:]
		}
		if prev, ok := byBare[bare]; !ok || prev != c.value {
			bareCount[bare]++
		}
		byBare[bare] = c.value
	}
	var out []facts.Fact
	for _, p := range pending {
		ref := strings.TrimPrefix(p.ref, "::")
		topic, ok := byName[ref]
		if !ok && !strings.Contains(ref, "::") && bareCount[ref] == 1 {
			topic, ok = byBare[ref], true
		}
		if !ok || (p.dotted && !strings.Contains(topic, ".")) {
			continue
		}
		f := p.fact
		f.Name = topic
		out = append(out, f)
	}
	return out
}

// scopeName returns the qualified name a class or module declaration opens.
func (w *rubyMessagingWalker) scopeName(node *sitter.Node, scope []string) []string {
	name := rubyText(node.ChildByFieldName("name"), w.src)
	if name == "" {
		return scope
	}
	return append(append([]string(nil), scope...), strings.TrimPrefix(name, "::"))
}

func (w *rubyMessagingWalker) collectConstants(node *sitter.Node, scope []string) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "class", "module":
		scope = w.scopeName(node, scope)
	case "assignment":
		left := node.ChildByFieldName("left")
		if left != nil && kindOf(left) == "constant" {
			w.consts[strings.Join(append(append([]string(nil), scope...), rubyText(left, w.src)), "::")] = node.ChildByFieldName("right")
		}
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		w.collectConstants(node.NamedChild(i), scope)
	}
}

func (w *rubyMessagingWalker) walk(node *sitter.Node, scope []string, method string) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "class", "module":
		scope = w.scopeName(node, scope)
		method = ""
	case "method":
		method = w.methodName(scope, rubyText(node.ChildByFieldName("name"), w.src), false)
	case "singleton_method":
		method = w.methodName(scope, rubyText(node.ChildByFieldName("name"), w.src), true)
	case "call":
		w.publication(node, scope, method)
	case "hash":
		w.envelope(node, scope, method)
	case "do_block", "block":
		w.subscription(node, scope)
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		w.walk(node.NamedChild(i), scope, method)
	}
}

// methodName spells a method the way the walker names its symbol.
func (w *rubyMessagingWalker) methodName(scope []string, name string, singleton bool) string {
	if name == "" {
		return ""
	}
	switch {
	case len(scope) == 0:
		return w.dir + "." + name
	case singleton:
		return strings.Join(scope, "::") + "." + name
	}
	return strings.Join(scope, "::") + "#" + name
}

// publication emits a call naming its topic under `topic:`.
func (w *rubyMessagingWalker) publication(call *sitter.Node, scope []string, method string) {
	name := rubyText(call.ChildByFieldName("method"), w.src)
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	for i := uint(0); i < args.NamedChildCount(); i++ {
		pair := args.NamedChild(i)
		if kindOf(pair) != "pair" || rubyHashKey(pair.ChildByFieldName("key"), w.src) != "topic" {
			continue
		}
		value := pair.ChildByFieldName("value")
		topic := w.topic(value, scope, 0)
		if topic == "" {
			w.defer_(value, !rubyPublishMethods[name], call, facts.MessagingOperationPublish, facts.MessagingRoleProducer, method, name)
		} else if rubyPublishMethods[name] || strings.Contains(topic, ".") {
			w.emit(topic, call, facts.MessagingOperationPublish, facts.MessagingRoleProducer, method, name)
		}
	}
}

// envelope emits a message hash, `{ topic: TOPIC, payload: … }`.
func (w *rubyMessagingWalker) envelope(hash *sitter.Node, scope []string, method string) {
	var topicNode *sitter.Node
	payload := false
	for i := uint(0); i < hash.NamedChildCount(); i++ {
		pair := hash.NamedChild(i)
		if kindOf(pair) != "pair" {
			continue
		}
		switch rubyHashKey(pair.ChildByFieldName("key"), w.src) {
		case "topic":
			topicNode = pair.ChildByFieldName("value")
		case "payload":
			payload = true
		}
	}
	if topicNode == nil || !payload {
		return
	}
	topic := w.topic(topicNode, scope, 0)
	if topic == "" {
		w.defer_(topicNode, true, hash, facts.MessagingOperationPublish, facts.MessagingRoleProducer, method, "message")
	} else if strings.Contains(topic, ".") {
		w.emit(topic, hash, facts.MessagingOperationPublish, facts.MessagingRoleProducer, method, "message")
	}
}

// subscription emits a block configuring a topic together with the classes that
// handle it: each handler subscribes to the topic.
func (w *rubyMessagingWalker) subscription(block *sitter.Node, scope []string) {
	var topicNode *sitter.Node
	var handlers []string
	body := block.ChildByFieldName("body")
	if body == nil {
		return
	}
	for i := uint(0); i < body.NamedChildCount(); i++ {
		stmt := body.NamedChild(i)
		if kindOf(stmt) != "assignment" {
			continue
		}
		left, right := stmt.ChildByFieldName("left"), stmt.ChildByFieldName("right")
		if left == nil || right == nil || kindOf(left) != "call" {
			continue
		}
		switch rubyText(left.ChildByFieldName("method"), w.src) {
		case "topic":
			topicNode = right
		case "handler":
			if isRubyConstantRef(right) {
				handlers = append(handlers, strings.TrimPrefix(rubyText(right, w.src), "::"))
			}
		case "handlers":
			for j := uint(0); j < right.NamedChildCount(); j++ {
				if p := right.NamedChild(j); kindOf(p) == "pair" {
					if v := p.ChildByFieldName("value"); v != nil && isRubyConstantRef(v) {
						handlers = append(handlers, strings.TrimPrefix(rubyText(v, w.src), "::"))
					}
				}
			}
		}
	}
	if topicNode == nil || len(handlers) == 0 {
		return
	}
	topic := w.topic(topicNode, scope, 0)
	for _, h := range handlers {
		if topic == "" {
			w.defer_(topicNode, false, topicNode, facts.MessagingOperationSubscribe, facts.MessagingRoleConsumer, h, "handler")
		} else {
			w.emit(topic, topicNode, facts.MessagingOperationSubscribe, facts.MessagingRoleConsumer, h, "handler")
		}
	}
}

func (w *rubyMessagingWalker) emit(topic string, at *sitter.Node, operation, role, symbol, call string) {
	line := int(at.StartPosition().Row) + 1
	key := topic + "\x00" + operation + "\x00" + symbol + "\x00" + strconv.Itoa(line)
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	w.out = append(w.out, w.operation(topic, line, operation, role, symbol, call))
}

// defer_ holds an operation whose topic is a constant reference this file does not
// define; a value of any other shape is not guessed.
func (w *rubyMessagingWalker) defer_(value *sitter.Node, dotted bool, at *sitter.Node, operation, role, symbol, call string) {
	if value == nil || !isRubyConstantRef(value) {
		return
	}
	line := int(at.StartPosition().Row) + 1
	w.pending = append(w.pending, rubyPendingTopic{
		ref: rubyText(value, w.src), dotted: dotted,
		fact: w.operation("", line, operation, role, symbol, call),
	})
}

func (w *rubyMessagingWalker) operation(topic string, line int, operation, role, symbol, call string) facts.Fact {
	props := map[string]any{
		"storage_kind":               facts.StorageKindTopic,
		facts.PropMessagingRole:      role,
		facts.PropMessagingOperation: operation,
		facts.PropSource:             facts.MessagingSourceRubyCall,
		"language":                   "ruby",
		"call":                       call,
	}
	if symbol != "" {
		props["code_symbol"] = symbol
	}
	return facts.Fact{
		Kind: facts.KindStorage, Name: topic, File: w.relFile, Line: line, Props: props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
}

// topic reads a topic name from a string or a constant resolving to one, "" when it
// does not resolve.
func (w *rubyMessagingWalker) topic(node *sitter.Node, scope []string, hops int) string {
	if node == nil || hops > rubyConstantHops {
		return ""
	}
	switch kindOf(node) {
	case "string":
		return w.stringTopic(node)
	case "constant", "scope_resolution":
		if v := w.resolveConstant(rubyText(node, w.src), scope); v != nil {
			return w.topic(v, scope, hops+1)
		}
	case "call":
		// "…".freeze, NAME.dup: the same string.
		switch rubyText(node.ChildByFieldName("method"), w.src) {
		case "freeze", "dup", "to_s":
			return w.topic(node.ChildByFieldName("receiver"), scope, hops)
		}
	}
	return ""
}

// resolveConstant looks a constant up from the innermost scope outward, as Ruby
// does lexically; a qualified reference is also tried as written.
func (w *rubyMessagingWalker) resolveConstant(ref string, scope []string) *sitter.Node {
	ref = strings.TrimPrefix(ref, "::")
	for i := len(scope); i >= 0; i-- {
		name := ref
		if i > 0 {
			name = strings.Join(scope[:i], "::") + "::" + ref
		}
		if v, ok := w.consts[name]; ok {
			return v
		}
	}
	return nil
}

// stringTopic renders a string literal, each interpolation as a {parameter}.
func (w *rubyMessagingWalker) stringTopic(node *sitter.Node) string {
	var b strings.Builder
	for i := uint(0); i < node.NamedChildCount(); i++ {
		c := node.NamedChild(i)
		switch kindOf(c) {
		case "string_content":
			b.WriteString(rubyText(c, w.src))
		case "interpolation":
			name := lastRubyIdentifier(c, w.src)
			if name == "" {
				return ""
			}
			b.WriteString("{" + strings.ToLower(name) + "}")
		default:
			return ""
		}
	}
	topic := strings.TrimSpace(b.String())
	if topic == "" || strings.ContainsAny(topic, " /") {
		return ""
	}
	return topic
}

// lastRubyIdentifier returns the last identifier or constant an expression names:
// PORTAL in #{PORTAL}, env in #{Rails.env}.
func lastRubyIdentifier(node *sitter.Node, src []byte) string {
	name := ""
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		switch kindOf(n) {
		case "identifier", "constant":
			name = rubyText(n, src)
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			visit(n.NamedChild(i))
		}
	}
	visit(node)
	return name
}

func isRubyConstantRef(n *sitter.Node) bool {
	switch kindOf(n) {
	case "constant", "scope_resolution":
		return true
	}
	return false
}

// rubyHashKey returns a hash pair's key as a bare name: `topic:`, `:topic =>`,
// `"topic" =>`.
func rubyHashKey(key *sitter.Node, src []byte) string {
	if key == nil {
		return ""
	}
	return strings.Trim(rubyText(key, src), `:"' `)
}

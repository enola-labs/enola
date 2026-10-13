package tsextractor

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// extractTSKafkaFacts recognizes static KafkaJS and node-rdkafka call sites.
// An explicit Kafka package import gates the pass so unrelated send/subscribe
// methods (web sockets, in-process event buses, RxJS) cannot become contracts.
func extractTSKafkaFacts(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string) []facts.Fact {
	if !importsTSKafka(kinds, root, src) {
		return nil
	}
	dir := factpath.Dir(relFile)
	seen := map[string]bool{}
	var out []facts.Fact

	var walk func(*sitter.Node, string, string)
	walk = func(node *sitter.Node, symbol, className string) {
		if node == nil {
			return
		}
		kind := kindOf(kinds, node)
		switch kind {
		case "class_declaration", "abstract_class_declaration", "class":
			if name := findChildByKind(kinds, node, "type_identifier"); name != nil {
				className = nodeText(name, src)
				symbol = dir + "." + className
			}
		case "function_declaration", "generator_function_declaration":
			if name := findChildByKind(kinds, node, "identifier"); name != nil {
				symbol = dir + "." + nodeText(name, src)
			}
		case "method_definition":
			name := node.ChildByFieldName("name")
			if name == nil {
				name = findChildByKind(kinds, node, "property_identifier")
			}
			if name != nil && className != "" {
				symbol = dir + "." + className + "." + nodeText(name, src)
			}
		case "variable_declarator":
			value := node.ChildByFieldName("value")
			if value != nil && isTSFunctionKind(kindOf(kinds, value)) {
				if name := node.ChildByFieldName("name"); name != nil {
					symbol = dir + "." + nodeText(name, src)
				}
			}
		case "call_expression":
			bindings := tsKafkaStringBindingsFor(kinds, root, node, src)
			topic, operation, role, call := tsKafkaCall(kinds, node, src, bindings)
			if topic != "" {
				line := int(node.StartPosition().Row) + 1
				key := topic + "\x00" + operation + "\x00" + relFile + "\x00" + strconv.Itoa(line)
				if !seen[key] {
					seen[key] = true
					out = append(out, facts.Fact{
						Kind: facts.KindStorage, Name: topic, File: relFile, Line: line,
						Props: map[string]any{
							"storage_kind": facts.StorageKindTopic, facts.PropMessaging: "kafka",
							facts.PropMessagingRole: role, facts.PropMessagingOperation: operation,
							facts.PropSource: facts.MessagingSourceTSKafkaCall,
							"language":       "typescript", "code_symbol": symbol, "call": call,
						},
						Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
					})
				}
			}
		}
		for i := range node.ChildCount() {
			walk(node.Child(i), symbol, className)
		}
	}
	walk(root, "", "")
	return out
}

func importsTSKafka(kinds *tsutil.KindTable, root *sitter.Node, src []byte) bool {
	if !bytes.Contains(src, kafkaWord) {
		return false
	}
	found := false
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if node == nil || found {
			return
		}
		var packageNode *sitter.Node
		switch kindOf(kinds, node) {
		case "import_statement":
			packageNode = node.ChildByFieldName("source")
			if packageNode == nil {
				packageNode = findChildByKind(kinds, node, "string")
			}
		case "call_expression":
			fn := node.ChildByFieldName("function")
			if fn != nil && (nodeText(fn, src) == "require" || nodeText(fn, src) == "import") {
				packageNode = firstNamedChild(kinds, node.ChildByFieldName("arguments"))
			}
		}
		if packageNode != nil && isTSKafkaPackage(strings.Trim(nodeText(packageNode, src), "'\"")) {
			found = true
			return
		}
		for i := range node.ChildCount() {
			walk(node.Child(i))
		}
	}
	walk(root)
	return found
}

func isTSKafkaPackage(name string) bool {
	return name == "kafkajs" || name == "node-rdkafka" || name == "kafka-node"
}

func isTSFunctionKind(kind string) bool {
	switch kind {
	case "arrow_function", "function_expression", "generator_function":
		return true
	default:
		return false
	}
}

func tsKafkaCall(kinds *tsutil.KindTable, call *sitter.Node, src []byte, bindings map[string]string) (topic, operation, role, name string) {
	fn := call.ChildByFieldName("function")
	if fn == nil || kindOf(kinds, fn) != "member_expression" {
		return "", "", "", ""
	}
	property := fn.ChildByFieldName("property")
	if property == nil {
		return "", "", "", ""
	}
	name = nodeText(property, src)
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return "", "", "", ""
	}
	switch name {
	case "send":
		topic = topicProperty(kinds, args, src, bindings)
		operation, role = facts.MessagingOperationPublish, facts.MessagingRoleProducer
	case "subscribe":
		topic = topicProperty(kinds, args, src, bindings)
		operation, role = facts.MessagingOperationSubscribe, facts.MessagingRoleConsumer
	case "produce":
		if arg := firstNamedChild(kinds, args); arg != nil {
			topic = tsKafkaStringValue(kinds, arg, src, bindings)
		}
		operation, role = facts.MessagingOperationPublish, facts.MessagingRoleProducer
	}
	return topic, operation, role, name
}

func topicProperty(kinds *tsutil.KindTable, node *sitter.Node, src []byte, bindings map[string]string) string {
	object := firstNamedChild(kinds, node)
	if object == nil || kindOf(kinds, object) != "object" {
		return ""
	}
	for i := range object.NamedChildCount() {
		pair := object.NamedChild(i)
		if kindOf(kinds, pair) != "pair" {
			continue
		}
		key := pair.ChildByFieldName("key")
		if key != nil && strings.Trim(nodeText(key, src), "'\"") == "topic" {
			return tsKafkaStringValue(kinds, pair.ChildByFieldName("value"), src, bindings)
		}
	}
	return ""
}

func firstNamedChild(kinds *tsutil.KindTable, node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := range node.NamedChildCount() {
		child := node.NamedChild(i)
		if kindOf(kinds, child) != "comment" {
			return child
		}
	}
	return nil
}

// tsKafkaStringBindingsFor returns constants visible at a call site. Conflicts are
// scoped to their enclosing function, so the conventional `const topic = ...` in
// two independent functions does not make both bindings ambiguous.
func tsKafkaStringBindingsFor(kinds *tsutil.KindTable, root, call *sitter.Node, src []byte) map[string]string {
	values := map[string]string{}
	ambiguous := map[string]bool{}
	callScope := tsKafkaFunctionScope(kinds, call)
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if kindOf(kinds, node) == "variable_declarator" {
			// Only immutable bindings are safe to fold repo-locally. A let/var may
			// be reassigned through control flow this lightweight pass cannot prove.
			parent := node.Parent()
			if parent != nil && strings.HasPrefix(strings.TrimSpace(nodeText(parent, src)), "const ") {
				declScope := tsKafkaFunctionScope(kinds, node)
				if declScope != nil && !sameTSNode(declScope, callScope) {
					return
				}
				name, value := node.ChildByFieldName("name"), node.ChildByFieldName("value")
				if name != nil && value != nil && kindOf(kinds, name) == "identifier" {
					literal := tsKafkaStringValue(kinds, value, src, nil)
					id := nodeText(name, src)
					if literal != "" && !ambiguous[id] {
						if old, ok := values[id]; ok && old != literal {
							delete(values, id)
							ambiguous[id] = true
						} else {
							values[id] = literal
						}
					}
				}
			}
		}
		for i := range node.ChildCount() {
			walk(node.Child(i))
		}
	}
	walk(root)
	return values
}

func tsKafkaFunctionScope(kinds *tsutil.KindTable, node *sitter.Node) *sitter.Node {
	for current := node; current != nil; current = current.Parent() {
		switch kindOf(kinds, current) {
		case "function_declaration", "generator_function_declaration", "function_expression", "generator_function", "arrow_function", "method_definition":
			return current
		}
	}
	return nil
}

func sameTSNode(a, b *sitter.Node) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte()
}

func tsKafkaStringValue(kinds *tsutil.KindTable, node *sitter.Node, src []byte, bindings map[string]string) string {
	if node == nil {
		return ""
	}
	switch kindOf(kinds, node) {
	case "string":
		return strings.Trim(nodeText(node, src), "'\"")
	case "template_string":
		value := strings.Trim(nodeText(node, src), "`")
		if !strings.Contains(value, "${") {
			return value
		}
	case "identifier":
		return bindings[nodeText(node, src)]
	}
	return ""
}

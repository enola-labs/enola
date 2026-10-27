package tsextractor

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// In-process events and job queues: a name one piece of code emits or enqueues and
// another handles, with nothing but the name between them. Each becomes a topic
// fact (storage_kind=topic, the convention Kafka set) under its own messaging
// protocol, so neither the AsyncAPI contract binder nor the Kafka cross-repo signal
// treats it as a broker topic, and the code on each side publishes or subscribes to
// it. Only forms a framework gives meaning to are read; a DOM listener or a stream's
// 'data' event is not an architectural event.
const (
	messagingMedusaEvent   = "medusa-event"
	messagingDirectusEvent = "directus-event"
	messagingNodeEvent     = "node-event"
	messagingBullMQ        = "bullmq"

	messagingSourceTSEventCall = "typescript-event-call"

	// propEventRef holds an event name written as a constant (`OrderEvents.PLACED`)
	// until the repo-wide pass resolves it.
	propEventRef = "event_ref"

	eventUnresolvedDynamic = "dynamic-event-name"
)

// nodeStreamEvents are the events Node's own streams, sockets and processes emit.
// An emitter's 'error' or 'close' names no contract between two parts of an app.
var nodeStreamEvents = map[string]bool{
	"error": true, "close": true, "end": true, "data": true, "finish": true, "open": true,
	"message": true, "ready": true, "exit": true, "drain": true, "pipe": true, "unpipe": true,
	"readable": true, "timeout": true, "response": true, "request": true, "connect": true,
	"disconnect": true, "newListener": true, "removeListener": true, "listening": true,
}

var (
	defaultExportFnRe    = regexp.MustCompile(`export\s+default\s+(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	defaultExportIdentRe = regexp.MustCompile(`export\s+default\s+([A-Za-z_$][\w$]*)\s*;?\s*(?:\n|$)`)
)

type eventCall struct {
	name, ref, messaging, role, symbol string
	line                               int
}

// extractTSEventFacts reads one production file's event emits and handlers.
func extractTSEventFacts(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string) []facts.Fact {
	if facts.IsTestPath(relFile) {
		return nil
	}
	text := string(src)
	medusa := strings.Contains(text, "@medusajs/") || strings.Contains(relFile, "/subscribers/")
	directus := strings.Contains(text, "emitter.") || strings.Contains(text, "defineHook")
	nodeEmitter := strings.Contains(text, "EventEmitter")
	bull := strings.Contains(text, "bullmq") || strings.Contains(text, "'bull'") || strings.Contains(text, `"bull"`)
	if !medusa && !directus && !nodeEmitter && !bull {
		return nil
	}
	dir := factpath.Dir(relFile)
	emitters := map[string]bool{}
	if nodeEmitter {
		emitters = nodeEmitterBindings(kinds, root, src)
	}
	subscriber := ""
	if m := defaultExportFnRe.FindStringSubmatch(text); m != nil {
		subscriber = dir + "." + m[1]
	} else if m := defaultExportIdentRe.FindStringSubmatch(text); m != nil {
		subscriber = dir + "." + m[1]
	}

	var calls []eventCall
	unresolved := map[string]bool{}
	var add func(n *sitter.Node, messaging, role, symbol string)
	add = func(n *sitter.Node, messaging, role, symbol string) {
		line := int(n.StartPosition().Row) + 1
		switch kindOf(kinds, n) {
		case "string":
			calls = append(calls, eventCall{name: strings.Trim(nodeText(n, src), `"'`), messaging: messaging, role: role, symbol: symbol, line: line})
		case "template_string":
			if t := nodeText(n, src); !strings.Contains(t, "${") {
				calls = append(calls, eventCall{name: strings.Trim(t, "`"), messaging: messaging, role: role, symbol: symbol, line: line})
				return
			}
			unresolved[symbol] = true
		case "member_expression":
			calls = append(calls, eventCall{ref: nodeText(n, src), messaging: messaging, role: role, symbol: symbol, line: line})
		case "array":
			for i := range n.NamedChildCount() {
				el := n.NamedChild(i)
				if k := kindOf(kinds, el); k == "string" || k == "member_expression" || k == "template_string" {
					add(el, messaging, role, symbol)
				} else {
					unresolved[symbol] = true
				}
			}
		default:
			unresolved[symbol] = true
		}
	}
	objField := func(obj *sitter.Node, key string) *sitter.Node {
		for i := range obj.NamedChildCount() {
			p := obj.NamedChild(i)
			if kindOf(kinds, p) == "pair" {
				if k := p.ChildByFieldName("key"); k != nil && strings.Trim(nodeText(k, src), `"'`) == key {
					return p.ChildByFieldName("value")
				}
			}
		}
		return nil
	}

	var walk func(n *sitter.Node, inHook bool)
	walk = func(n *sitter.Node, inHook bool) {
		switch kindOf(kinds, n) {
		case "variable_declarator":
			// A Medusa subscriber: `export const config: SubscriberConfig = { event: … }`
			// beside the default-exported handler that receives it.
			if name := n.ChildByFieldName("name"); medusa && name != nil && nodeText(name, src) == "config" {
				if val := n.ChildByFieldName("value"); val != nil && kindOf(kinds, val) == "object" {
					if ev := objField(val, "event"); ev != nil {
						sym := subscriber
						if sym == "" {
							sym = dir + ".config"
						}
						add(ev, messagingMedusaEvent, facts.MessagingRoleConsumer, sym)
						unresolvedTo(unresolved, sym, dir+".config")
					}
				}
			}
		case "call_expression":
			fn := n.ChildByFieldName("function")
			args := namedArgs(kinds, n.ChildByFieldName("arguments"))
			if fn == nil || len(args) == 0 {
				break
			}
			// The enclosing symbol is a walk to the root, so it is found only for a call
			// that is an event.
			symOf := func() string { return graphqlEnclosingSymbol(kinds, n, src, relFile) }
			switch kindOf(kinds, fn) {
			case "identifier":
				switch name := nodeText(fn, src); {
				case medusa && name == "emitEventStep" && kindOf(kinds, args[0]) == "object":
					if ev := objField(args[0], "eventName"); ev != nil {
						add(ev, messagingMedusaEvent, facts.MessagingRoleProducer, symOf())
					}
				case directus && name == "defineHook":
					for i := range n.NamedChildCount() {
						walk(n.NamedChild(i), true)
					}
					return
				case directus && inHook && (name == "action" || name == "filter" || name == "init"):
					add(args[0], messagingDirectusEvent, facts.MessagingRoleConsumer, symOf())
				}
			case "member_expression":
				obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
				if obj == nil || prop == nil {
					break
				}
				recv, method := nodeText(obj, src), nodeText(prop, src)
				switch {
				case medusa && method == "emit" && strings.Contains(strings.ToLower(recv), "eventbus"):
					// eventBus.emit({ name, data }) or a list of them; eventBusService.emit(name, data)
					target := args[0]
					if kindOf(kinds, target) == "array" && target.NamedChildCount() > 0 {
						for i := range target.NamedChildCount() {
							if el := target.NamedChild(i); kindOf(kinds, el) == "object" {
								if ev := objField(el, "name"); ev != nil {
									add(ev, messagingMedusaEvent, facts.MessagingRoleProducer, symOf())
								}
							}
						}
					} else if kindOf(kinds, target) == "object" {
						if ev := objField(target, "name"); ev != nil {
							add(ev, messagingMedusaEvent, facts.MessagingRoleProducer, symOf())
						}
					} else {
						add(target, messagingMedusaEvent, facts.MessagingRoleProducer, symOf())
					}
				case directus && recv == "emitter" && (method == "emitAction" || method == "emitFilter" || method == "emitInit"):
					add(args[0], messagingDirectusEvent, facts.MessagingRoleProducer, symOf())
				case directus && recv == "emitter" && (method == "onAction" || method == "onFilter" || method == "onInit"):
					add(args[0], messagingDirectusEvent, facts.MessagingRoleConsumer, symOf())
				case nodeEmitter && emitters[recv] && (method == "emit" || method == "on" || method == "once" || method == "addListener" || method == "prependListener"):
					ev := strings.Trim(nodeText(args[0], src), `"'`)
					if kindOf(kinds, args[0]) != "string" || nodeStreamEvents[ev] {
						break
					}
					role := facts.MessagingRoleConsumer
					if method == "emit" {
						role = facts.MessagingRoleProducer
					}
					// A Node event belongs to its emitter: two emitters' 'result' events
					// are unrelated, so the topic is named for the emitter it is on.
					calls = append(calls, eventCall{name: nodeEmitterOwner(kinds, n, src, dir, recv) + ":" + ev,
						messaging: messagingNodeEvent, role: role, symbol: symOf(), line: int(args[0].StartPosition().Row) + 1})
				}
			}
		case "new_expression":
			// BullMQ: new Queue('emails') enqueues, new Worker('emails', processor) handles.
			if c := n.ChildByFieldName("constructor"); bull && c != nil {
				args := namedArgs(kinds, n.ChildByFieldName("arguments"))
				if len(args) > 0 && kindOf(kinds, args[0]) == "string" {
					sym := graphqlEnclosingSymbol(kinds, n, src, relFile)
					switch nodeText(c, src) {
					case "Queue":
						add(args[0], messagingBullMQ, facts.MessagingRoleProducer, sym)
					case "Worker":
						add(args[0], messagingBullMQ, facts.MessagingRoleConsumer, sym)
					}
				}
			}
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i), inHook)
		}
	}
	walk(root, false)

	var out []facts.Fact
	for _, c := range calls {
		props := map[string]any{
			"storage_kind": facts.StorageKindTopic, facts.PropMessaging: c.messaging,
			facts.PropMessagingRole: c.role, facts.PropSource: messagingSourceTSEventCall,
			"language": "typescript",
		}
		if c.role == facts.MessagingRoleProducer {
			props[facts.PropMessagingOperation] = facts.MessagingOperationPublish
		} else {
			props[facts.PropMessagingOperation] = facts.MessagingOperationSubscribe
		}
		if c.symbol != "" {
			props[propCodeSymbol] = c.symbol
		}
		name := c.name
		if c.ref != "" {
			props[propEventRef] = c.ref
			name = c.ref
		}
		out = append(out, facts.Fact{Kind: facts.KindStorage, Name: name, File: relFile, Line: c.line, Props: props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}})
	}
	syms := make([]string, 0, len(unresolved))
	for s := range unresolved {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	for _, s := range syms {
		// Carried on a placeholder the repo-wide pass turns into a prop on the symbol.
		out = append(out, facts.Fact{Kind: facts.KindStorage, Name: "", File: relFile, Props: map[string]any{
			"storage_kind": facts.StorageKindTopic, facts.PropSource: messagingSourceTSEventCall,
			propCodeSymbol: s, "unresolved_reason": eventUnresolvedDynamic,
		}})
	}
	return out
}

// unresolvedTo moves an unresolved mark from the handler to the config that names
// the event: the config holds the expression that could not be read.
func unresolvedTo(unresolved map[string]bool, from, to string) {
	if unresolved[from] && from != to {
		delete(unresolved, from)
		unresolved[to] = true
	}
}

// nodeEmitterBindings names what emits Node events in this file: an identifier bound
// to `new EventEmitter()` (or EventEmitter2), and `this` inside a class that extends
// EventEmitter.
func nodeEmitterBindings(kinds *tsutil.KindTable, root *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		switch kindOf(kinds, n) {
		case "variable_declarator", "public_field_definition":
			name := n.ChildByFieldName("name")
			val := n.ChildByFieldName("value")
			if name != nil && val != nil && kindOf(kinds, val) == "new_expression" {
				if c := val.ChildByFieldName("constructor"); c != nil && strings.HasPrefix(nodeText(c, src), "EventEmitter") {
					out[nodeText(name, src)] = true
					out["this."+nodeText(name, src)] = true
				}
			}
		case "class_declaration", "class":
			if h := findChildByKind(kinds, n, "class_heritage"); h != nil && strings.Contains(nodeText(h, src), "EventEmitter") {
				out["this"] = true
			}
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return out
}

var (
	eventEnumRe      = regexp.MustCompile(`(?:export\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)\s*\{([^}]*)\}`)
	eventEnumMember  = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*=\s*["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]`)
	eventObjConstRe  = regexp.MustCompile(`(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*(?::[^=\n]+)?=\s*\{`)
	eventObjMemberRe = regexp.MustCompile(`(?m)^\s*([A-Za-z_$][\w$]*|"[^"]+"|'[^']+')\s*:\s*["'` + "`" + `]([^"'` + "`" + `\n]+)["'` + "`" + `]`)
)

// eventConstFile is what one file declares toward the event-name index: "Holder.KEY"
// and the string an exported enum member or flat object constant holds, the way event
// names are usually declared (`export const OrderEvents = { PLACED: "order.placed" }`).
// blanked is the file with its comments blanked, since event tables document each
// payload in JSDoc, braces included; it is called only for a file that may hold one.
func eventConstFile(blanked []byte) [][2]string {
	src := string(blanked)
	var out [][2]string
	for _, m := range eventEnumRe.FindAllStringSubmatch(src, -1) {
		for _, mm := range eventEnumMember.FindAllStringSubmatch(m[2], -1) {
			out = append(out, [2]string{m[1] + "." + mm[1], mm[2]})
		}
	}
	for _, loc := range eventObjConstRe.FindAllStringSubmatchIndex(src, -1) {
		holder := src[loc[2]:loc[3]]
		body := src[loc[1]:]
		// Only the object's own top level: stop at its closing brace.
		depth := 1
		for i, ch := range body {
			switch ch {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				body = body[:i]
				break
			}
		}
		if strings.Count(body, "{") > 0 {
			continue // nested: not a flat name table
		}
		for _, mm := range eventObjMemberRe.FindAllStringSubmatch(body, -1) {
			out = append(out, [2]string{holder + "." + strings.Trim(mm[1], `"'`), mm[2]})
		}
	}
	return out
}

// mayDeclareEventConsts reports whether a file is read for the event-name index.
func mayDeclareEventConsts(relFile string, src []byte) bool {
	return !facts.IsTestPath(relFile) && !isVueFile(relFile) &&
		(bytes.Contains(src, []byte("enum ")) || bytes.Contains(src, []byte("const ")))
}

// eventConstIndex merges what every file declares. A name two files give different
// values is dropped rather than guessed.
func eventConstIndex(perFile []tsFileResult) map[string]string {
	vals := map[string]map[string]bool{}
	for _, res := range perFile {
		for _, kv := range res.eventConsts {
			if vals[kv[0]] == nil {
				vals[kv[0]] = map[string]bool{}
			}
			vals[kv[0]][kv[1]] = true
		}
	}
	out := map[string]string{}
	for k, set := range vals {
		if len(set) == 1 {
			for v := range set {
				out[k] = v
			}
		}
	}
	return out
}

// bindEvents resolves event names written as constants, connects the code on each
// side to the event (publishes / subscribes), and turns a name that cannot be read
// statically into unresolved_reason on the code that wrote it.
func bindEvents(all []facts.Fact, consts map[string]string) []facts.Fact {
	unresolved := map[string]bool{}
	rels := map[string][]facts.Relation{}
	kept := all[:0]
	for _, f := range all {
		if f.Kind != facts.KindStorage || f.PropString(facts.PropSource) != messagingSourceTSEventCall {
			kept = append(kept, f)
			continue
		}
		sym := f.PropString(propCodeSymbol)
		if f.Name == "" {
			unresolved[sym] = true
			continue
		}
		if ref := f.PropString(propEventRef); ref != "" {
			v, ok := consts[ref]
			if !ok {
				unresolved[sym] = true
				continue
			}
			f.Name = v
		}
		if sym == "" {
			// Module-level code (a route callback, an exported defineHook): the file
			// is what holds it.
			sym = f.File
		}
		if sym != "" {
			kind := facts.RelSubscribes
			if f.PropString(facts.PropMessagingRole) == facts.MessagingRoleProducer {
				kind = facts.RelPublishes
			}
			rels[sym] = append(rels[sym], facts.Relation{Kind: kind, Target: f.Name})
		}
		kept = append(kept, f)
	}
	for i := range kept {
		f := &kept[i]
		if f.Kind != facts.KindSymbol && f.Kind != facts.KindFileRef {
			continue
		}
		for _, r := range rels[f.Name] {
			if !f.HasRelation(r.Kind, r.Target) {
				f.Relations = append(f.Relations, r)
			}
		}
		if unresolved[f.Name] {
			f.SetProp("unresolved_reason", eventUnresolvedDynamic)
		}
	}
	return kept
}

// nodeEmitterOwner names the emitter a Node event is on: the class for `this`, the
// class and field for `this.events`, the module binding otherwise.
func nodeEmitterOwner(kinds *tsutil.KindTable, n *sitter.Node, src []byte, dir, recv string) string {
	if recv == "this" || strings.HasPrefix(recv, "this.") {
		for p := n.Parent(); p != nil; p = p.Parent() {
			if k := kindOf(kinds, p); k == "class_declaration" || k == "abstract_class_declaration" {
				if name := p.ChildByFieldName("name"); name != nil {
					return dir + "." + nodeText(name, src) + strings.TrimPrefix(recv, "this")
				}
			}
		}
	}
	return dir + "." + recv
}

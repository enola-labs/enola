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
	// owner is the emitter a Node event written as a constant is on; the topic is
	// named owner:value once the constant resolves.
	owner string
	// pending marks a Node event on an emitter imported from another file: it is kept
	// only if that file exports an emitter of the name (bindEvents).
	pending bool
	line    int
}

// Props a Node event carries until bindEvents settles it.
const (
	propEventOwner     = "event_owner"
	propEmitterPending = "emitter_pending"
)

// extractTSEventFacts reads one production file's event emits and handlers.
func extractTSEventFacts(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string) []facts.Fact {
	if facts.IsTestPath(relFile) {
		return nil
	}
	text := string(src)
	medusa := strings.Contains(text, "@medusajs/") || strings.Contains(relFile, "/subscribers/")
	directus := strings.Contains(text, "emitter.") || strings.Contains(text, "defineHook")
	nodeEmitter := strings.Contains(text, "EventEmitter") && nodeEventEmitterImport(text)
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
				case nodeEmitter && emitters[recv] && nodeEventMethods[method]:
					// A Node event belongs to its emitter: two emitters' 'result' events
					// are unrelated, so the topic is named for the emitter it is on.
					calls = nodeEventCalls(kinds, src, args[0], nodeEmitterOwner(kinds, n, src, dir, recv), method, symOf, false, calls, unresolved)
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

	out := eventCallFacts(calls, relFile, dir)
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

var nodeEventMethods = map[string]bool{"emit": true, "on": true, "once": true, "addListener": true, "prependListener": true}

// nodeEventCalls reads the event a Node emitter call names: a string, a constant
// (`DistributedTransactionEvent.TIMEOUT`, resolved repo-wide), or either branch of a
// conditional. Any other expression is a name that cannot be read statically.
func nodeEventCalls(kinds *tsutil.KindTable, src []byte, arg *sitter.Node, owner, method string, symOf func() string, pending bool, calls []eventCall, unresolved map[string]bool) []eventCall {
	role := facts.MessagingRoleConsumer
	if method == "emit" {
		role = facts.MessagingRoleProducer
	}
	line := int(arg.StartPosition().Row) + 1
	switch kindOf(kinds, arg) {
	case "string":
		if ev := strings.Trim(nodeText(arg, src), `"'`); !nodeStreamEvents[ev] {
			calls = append(calls, eventCall{name: owner + ":" + ev, messaging: messagingNodeEvent, role: role, symbol: symOf(), pending: pending, line: line})
		}
	case "member_expression":
		calls = append(calls, eventCall{ref: nodeText(arg, src), owner: owner, messaging: messagingNodeEvent, role: role, symbol: symOf(), pending: pending, line: line})
	case "ternary_expression":
		for _, f := range []string{"consequence", "alternative"} {
			if b := arg.ChildByFieldName(f); b != nil {
				calls = nodeEventCalls(kinds, src, b, owner, method, symOf, pending, calls, unresolved)
			}
		}
	case "parenthesized_expression":
		if arg.NamedChildCount() > 0 {
			calls = nodeEventCalls(kinds, src, arg.NamedChild(0), owner, method, symOf, pending, calls, unresolved)
		}
	default:
		if !pending {
			unresolved[symOf()] = true
		}
	}
	return calls
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
func bindEvents(all []facts.Fact, consts map[string]string, emitters map[string]bool) []facts.Fact {
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
		if owner := f.PropString(propEmitterPending); owner != "" {
			// An emitter imported from another file: an event only if that file
			// exports an emitter of the name. A socket, a Vue app or a stream someone
			// imported is not one.
			if !emitters[owner] {
				continue
			}
			f.DelProp(propEmitterPending)
		}
		if ref := f.PropString(propEventRef); ref != "" {
			v, ok := consts[ref]
			if !ok {
				unresolved[sym] = true
				continue
			}
			f.Name = v
			if owner := f.PropString(propEventOwner); owner != "" {
				// Not filtered as a stream's own event: a name an event table declares
				// (`TIMEOUT = "timeout"`) is the application's contract.
				f.Name = owner + ":" + v
				f.DelProp(propEventOwner)
			}
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

// mayCallNodeEvents reports whether a file's text holds an emitter call at all.
func mayCallNodeEvents(src []byte) bool {
	for _, m := range []string{".on(", ".once(", ".emit(", ".addListener("} {
		if bytes.Contains(src, []byte(m)) {
			return true
		}
	}
	return false
}

// nodeEventEmitterLibs are the modules whose EventEmitter is Node's (or a drop-in
// for it). Angular's EventEmitter (@angular/core) is a component output: its emit
// takes a payload, not an event name.
var nodeEventEmitterLibs = map[string]bool{
	"events": true, "node:events": true, "eventemitter2": true, "eventemitter3": true, "stream": true, "node:stream": true,
}

var eventEmitterImportRe = regexp.MustCompile(`import\s*\{[^}]*\bEventEmitter\d?\b[^}]*\}\s*from\s*["']([^"']+)["']`)

// nodeEventEmitterImport reports whether a file's EventEmitter is Node's: imported
// from an event library, or not imported by name at all (required, or a global).
func nodeEventEmitterImport(text string) bool {
	m := eventEmitterImportRe.FindStringSubmatch(text)
	return m == nil || nodeEventEmitterLibs[m[1]]
}

// exportedEmitters names the Node event emitters a file exports, as another file
// imports them: `export class Migrations extends EventEmitter` is "<dir>.Migrations",
// `export const bus = new EventEmitter()` is "<dir>.bus". The names match what
// nodeEmitterOwner calls the same emitter inside the file.
func exportedEmitters(kinds *tsutil.KindTable, root *sitter.Node, src []byte, dir string) []string {
	var out []string
	for i := range root.NamedChildCount() {
		stmt := root.NamedChild(i)
		if kindOf(kinds, stmt) != "export_statement" {
			continue
		}
		decl := stmt.ChildByFieldName("declaration")
		if decl == nil {
			continue
		}
		switch kindOf(kinds, decl) {
		case "class_declaration", "abstract_class_declaration":
			if h := findChildByKind(kinds, decl, "class_heritage"); h != nil && strings.Contains(nodeText(h, src), "EventEmitter") {
				if name := decl.ChildByFieldName("name"); name != nil {
					out = append(out, dir+"."+nodeText(name, src))
				}
			}
		case "lexical_declaration", "variable_declaration":
			for j := range decl.NamedChildCount() {
				d := decl.NamedChild(j)
				name, val := d.ChildByFieldName("name"), d.ChildByFieldName("value")
				if name == nil || val == nil || kindOf(kinds, val) != "new_expression" {
					continue
				}
				if c := val.ChildByFieldName("constructor"); c != nil && strings.HasPrefix(nodeText(c, src), "EventEmitter") {
					out = append(out, dir+"."+nodeText(name, src))
				}
			}
		}
	}
	return out
}

// importedEmitterEventFacts reads the Node events a file emits or handles on an
// emitter another file of the repository exports: an imported instance
// (`import { bus } from './bus'; bus.on('ready')`), or one built here from an
// imported emitter class (`const migrations = new Migrations(orm);
// migrations.on('migrated')`). Each is a candidate until bindEvents confirms the
// other file exports an emitter of the name.
func importedEmitterEventFacts(kinds *tsutil.KindTable, root *sitter.Node, ctx *extractCtx) []facts.Fact {
	src := ctx.src
	if len(ctx.values) == 0 || !mayCallNodeEvents(src) {
		return nil
	}
	// The receivers that may be an imported emitter: an import from this repository,
	// or a local built with `new` from one.
	owners := map[string]string{}
	for local, target := range ctx.values {
		if _, imported := ctx.importMap[local]; imported {
			owners[local] = target
		}
	}
	var bind func(n *sitter.Node)
	bind = func(n *sitter.Node) {
		if kindOf(kinds, n) == "variable_declarator" {
			name, val := n.ChildByFieldName("name"), n.ChildByFieldName("value")
			if name != nil && val != nil && kindOf(kinds, name) == "identifier" && kindOf(kinds, val) == "new_expression" {
				if c := val.ChildByFieldName("constructor"); c != nil {
					if target, ok := owners[nodeText(c, src)]; ok {
						owners[nodeText(name, src)] = target
					}
				}
			}
		}
		for i := range n.NamedChildCount() {
			bind(n.NamedChild(i))
		}
	}
	bind(root)
	if len(owners) == 0 {
		return nil
	}
	var calls []eventCall
	unresolved := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if kindOf(kinds, n) == "call_expression" {
			fn := n.ChildByFieldName("function")
			args := namedArgs(kinds, n.ChildByFieldName("arguments"))
			if fn != nil && len(args) > 0 && kindOf(kinds, fn) == "member_expression" {
				obj, prop := fn.ChildByFieldName("object"), fn.ChildByFieldName("property")
				if obj != nil && prop != nil && kindOf(kinds, obj) == "identifier" && nodeEventMethods[nodeText(prop, src)] {
					if owner, ok := owners[nodeText(obj, src)]; ok {
						symOf := func() string { return graphqlEnclosingSymbol(kinds, n, src, ctx.relFile) }
						calls = nodeEventCalls(kinds, src, args[0], owner, nodeText(prop, src), symOf, true, calls, unresolved)
					}
				}
			}
		}
		for i := range n.NamedChildCount() {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return eventCallFacts(calls, ctx.relFile, ctx.dir)
}

// eventCallFacts turns the event calls a file makes into topic facts, carrying what
// bindEvents still has to settle: a constant to resolve (event_ref, with the Node
// emitter it is on), and an imported emitter to confirm (emitter_pending).
func eventCallFacts(calls []eventCall, relFile, dir string) []facts.Fact {
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
		name, owner := c.name, c.owner
		if c.ref != "" {
			props[propEventRef] = c.ref
			name = c.ref
			if c.owner != "" {
				props[propEventOwner] = c.owner
			}
		} else if c.pending {
			owner, _, _ = strings.Cut(c.name, ":")
		}
		if c.pending {
			props[propEmitterPending] = owner
		}
		out = append(out, facts.Fact{Kind: facts.KindStorage, Name: name, File: relFile, Line: c.line, Props: props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}}})
	}
	return out
}

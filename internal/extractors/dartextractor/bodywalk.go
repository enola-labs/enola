package dartextractor

import (
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/facts"
)

// bodyWalk is one function body's derived facts: the edges it draws and the metrics the
// complexity-outliers explainer and the performance analyzer read.
type bodyWalk struct {
	relations []facts.Relation

	cyclomatic int
	loopCount  int
	maxLoop    int
	maxScaling int
	// callsInLoop / callsInScalingLoop are metric strings, NOT graph edges: they name
	// what was invoked per iteration so the performance analyzer can spot an N+1, and
	// they deliberately include receiver-qualified targets that no fact resolves to.
	callsInLoop        map[string]bool
	callsInScalingLoop map[string]bool
	recursiveSelf      bool
	ioDirect           bool

	seenRel map[string]bool
}

// applyTo writes the metrics onto a symbol fact's props.
func (b *bodyWalk) applyTo(props map[string]any) {
	props["cyclomatic"] = b.cyclomatic + 1
	if b.loopCount > 0 {
		props["loop_count"] = b.loopCount
		props["loop_depth"] = b.maxLoop
	}
	if b.maxScaling > 0 {
		props["scaling_loop_depth"] = b.maxScaling
	}
	if len(b.callsInLoop) > 0 {
		props["calls_in_loop"] = sortedKeys(b.callsInLoop)
	}
	if len(b.callsInScalingLoop) > 0 {
		props["calls_in_scaling_loop"] = sortedKeys(b.callsInScalingLoop)
	}
	if b.recursiveSelf {
		props["recursive_self"] = true
	}
	if b.ioDirect {
		props["io_direct"] = true
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// walkBody walks a function body for call edges and complexity metrics.
//
// selfName is the enclosing symbol's canonical name, used for recursion detection.
func (w *walker) walkBody(body *sitter.Node, selfName string) *bodyWalk {
	b := &bodyWalk{
		callsInLoop:        map[string]bool{},
		callsInScalingLoop: map[string]bool{},
		seenRel:            map[string]bool{},
	}
	selfShort := selfName
	if i := strings.LastIndexByte(selfShort, '.'); i >= 0 {
		selfShort = selfShort[i+1:]
	}
	w.walkNode(body, b, selfShort, 0, 0)
	return b
}

// walkNode is the recursive body walk.
//
// loopDepth counts every syntactic iteration; scalingDepth counts only the iterations
// whose trip count grows with the input. The split is the same discount Go, C# and
// Scala apply: a literal-bounded `for (var i = 0; i < 3; i++)` or an iteration over a
// collection literal runs a fixed number of times, so counting it would turn an honest
// O(n) into a fabricated O(n²).
func (w *walker) walkNode(n *sitter.Node, b *bodyWalk, selfShort string, loopDepth, scalingDepth int) {
	if n == nil {
		return
	}
	kind := kindOf(n)

	// A nested function/closure DEFINITION resets loop depth: its body is deferred, not
	// executed per iteration of the enclosing loop. An iterator callback is handled at
	// the call site below, where it is entered WITH the incremented depth.
	if kind == "function_expression" || kind == "function_expression_body" {
		// The call scan has to run on this node too, not only on its descendants: a
		// closure body's invocation is a direct child SEQUENCE of it
		// (`() => getTiles(ctx)` is [identifier getTiles][selector (ctx)]), so
		// returning after walkChildren alone loses it entirely. Arrow closures are
		// pervasive in Flutter — `onPressed: () => save()`, `builder: (c) => Widget()`,
		// `.map((e) => f(e))` — and every call inside one was missing, which inflates
		// the dead-code report with functions that are plainly used.
		w.scanCallChain(n, b, selfShort, 0, 0)
		w.walkChildren(n, b, selfShort, 0, 0)
		return
	}

	switch kind {
	case "if_statement", "conditional_expression", "catch_clause",
		"switch_statement_case", "switch_expression_case":
		b.cyclomatic++
	// Dart spells its logical operators as their own node kinds rather than as a
	// generic binary expression, and the OPERATOR is a node too. Counting the operator
	// nodes is exact: `a && b && c` holds two logical_and_operator nodes and adds two,
	// where counting occurrences in the enclosing expression's TEXT would count the
	// outer node's operators again at every nested level.
	case "logical_and_operator", "logical_or_operator":
		b.cyclomatic++
	// `??` short-circuits, so it is a branch for the same reason `||` is.
	case "if_null_expression":
		b.cyclomatic++
	case "for_statement", "while_statement", "do_statement":
		b.cyclomatic++
		b.loopCount++
		nextLoop := loopDepth + 1
		nextScaling := scalingDepth
		if !w.isConstantTripLoop(n) {
			nextScaling++
		}
		b.observeDepth(nextLoop, nextScaling)
		w.walkChildren(n, b, selfShort, nextLoop, nextScaling)
		return
	}

	// Calls are found by scanning a node's children as a SEQUENCE, because the grammar
	// models `api.fetch(id)` as three siblings (identifier, `.fetch` selector, `(id)`
	// selector) rather than as a nested call expression.
	w.scanCallChain(n, b, selfShort, loopDepth, scalingDepth)
	w.walkChildren(n, b, selfShort, loopDepth, scalingDepth)
}

func (w *walker) walkChildren(n *sitter.Node, b *bodyWalk, selfShort string, loopDepth, scalingDepth int) {
	for _, c := range namedChildren(n) {
		w.walkNode(c, b, selfShort, loopDepth, scalingDepth)
	}
}

func (b *bodyWalk) observeDepth(loop, scaling int) {
	if loop > b.maxLoop {
		b.maxLoop = loop
	}
	if scaling > b.maxScaling {
		b.maxScaling = scaling
	}
}

// scanCallChain finds invocations among a node's direct children.
//
// The grammar flattens a selector chain into siblings, so `users.map(f).toList()` is
// [identifier users][.map][(f)][.toList][()]. An invocation is a selector holding an
// `argument_part`; the name being invoked is the nearest preceding `.name` selector, or
// the chain's base identifier when the arguments follow it directly.
func (w *walker) scanCallChain(n *sitter.Node, b *bodyWalk, selfShort string, loopDepth, scalingDepth int) {
	kids := namedChildren(n)
	for i, c := range kids {
		// `const Foo(...)` / `new Foo(...)` are their own nodes rather than selector
		// chains, so they are matched directly.
		if kindOf(c) == "const_object_expression" || kindOf(c) == "new_expression" {
			if t := childOfKind(c, "type_identifier"); t != nil {
				b.addRelation(facts.RelInstantiates, t.Utf8Text(w.src))
			}
			continue
		}
		if kindOf(c) != "selector" || childOfKind(c, "argument_part") == nil {
			continue
		}

		name, receiver, baseIsType := w.calleeOf(kids, i)
		if name == "" {
			continue
		}

		switch {
		case baseIsType && receiver == name:
			// `Foo(...)` — a bare capitalised identifier invoked with arguments is a
			// constructor call in Dart, where `new` has been optional since Dart 2.
			b.addRelation(facts.RelInstantiates, name)
		case baseIsType:
			// `Foo.named(...)` — a named constructor or a static method. The two are
			// syntactically identical, so the edge is drawn to the qualified member
			// and left for resolution to bind; guessing which it is would manufacture
			// an instantiates edge half the time.
			b.addRelation(facts.RelCalls, receiver+"."+name)
		default:
			// Any other receiver: the edge is the bare method name. The receiver's
			// static type is not tracked, so a canonical target cannot be verified —
			// and per the governing rule a bare name that dead-code matching can still
			// see beats a fabricated qualified target.
			b.addRelation(facts.RelCalls, name)
		}

		// Recursion requires the call to reach THIS symbol, not merely a method that
		// shares its name.
		//
		// Matching on the short name alone is wrong in Dart to the point of dominating
		// the report: `dispose()` calling `controller.dispose()`, `fromJson` calling a
		// nested `fromJson`, `stop()` calling `player.stop()` are all ordinary
		// delegation to a DIFFERENT object. On one mid-size app that produced 64 false
		// recursion findings — 63 of the 75 performance findings the analyzer emitted.
		//
		// So an explicit receiver disqualifies it, with the two spellings that do mean
		// self kept: a bare call (`foo()`, where calleeOf reports the base as its own
		// name) and `this.foo()`. `super.foo()` is deliberately excluded — it dispatches
		// to the ancestor's implementation, which is the opposite of recursing.
		if name == selfShort && (receiver == "" || receiver == name || receiver == "this") {
			b.recursiveSelf = true
		}
		if w.isIOCall(name, receiver) {
			b.ioDirect = true
		}

		metric := name
		if receiver != "" && receiver != name {
			metric = receiver + "." + name
		}
		if loopDepth > 0 {
			b.callsInLoop[metric] = true
		}
		if scalingDepth > 0 {
			b.callsInScalingLoop[metric] = true
		}

		// An iterator method taking a closure IS a loop, and its closure body runs per
		// element — so it is walked at the incremented depth rather than being left to
		// the function_expression reset above.
		if dartLoopMethods[name] && hasClosureArgument(c) {
			b.cyclomatic++
			b.loopCount++
			nextLoop := loopDepth + 1
			nextScaling := scalingDepth
			if !w.isConstantTripIterator(kids, i) {
				nextScaling++
			}
			b.observeDepth(nextLoop, nextScaling)
			for _, arg := range namedChildren(c) {
				w.walkChildren(arg, b, selfShort, nextLoop, nextScaling)
			}
		}
	}
}

// calleeOf resolves the invoked name at kids[i] (a selector holding arguments).
//
// Returns the method name, the chain's base text, and whether that base looks like a
// type (capitalised), which is what distinguishes a constructor call from a method call.
func (w *walker) calleeOf(kids []*sitter.Node, i int) (name, receiver string, baseIsType bool) {
	if i == 0 {
		return "", "", false
	}
	prev := kids[i-1]

	// Case 1: the arguments follow a `.name` selector — a method or named constructor.
	if kindOf(prev) == "selector" {
		sel := childOfKind(prev, "unconditional_assignable_selector", "conditional_assignable_selector")
		if sel == nil {
			return "", "", false
		}
		name = identifierChild(sel, w.src)
		if name == "" {
			return "", "", false
		}
		// Walk back past the selector run to the chain base.
		base := ""
		for j := i - 2; j >= 0; j-- {
			if kindOf(kids[j]) == "selector" {
				continue
			}
			base = strings.TrimSpace(kids[j].Utf8Text(w.src))
			break
		}
		// Only a base that is a plain identifier is meaningful as a receiver; an
		// expression base ("(a + b).foo()") is not something to key on.
		if base != "" && isPlainIdentifier(base) {
			return name, base, namesAType(base)
		}
		return name, "", false
	}

	// Case 2: the arguments follow the base directly — `runApp(...)` or `Foo(...)`.
	base := strings.TrimSpace(prev.Utf8Text(w.src))
	if base == "" || !isPlainIdentifier(base) {
		return "", "", false
	}
	return base, base, namesAType(base)
}

// isPlainIdentifier reports whether text is a bare Dart identifier.
func isPlainIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func (b *bodyWalk) addRelation(kind, target string) {
	if target == "" {
		return
	}
	key := kind + "\x00" + target
	if b.seenRel[key] {
		return
	}
	b.seenRel[key] = true
	b.relations = append(b.relations, facts.Relation{Kind: kind, Target: target})
}

// hasClosureArgument reports whether an argument list contains a function expression,
// which is what makes an iterator method an actual iteration rather than a plain call.
func hasClosureArgument(sel *sitter.Node) bool {
	return firstOfKind(sel, "function_expression") != nil
}

// dartLoopMethods are the collection methods whose callback runs once per element.
//
// The exclusions matter more than the inclusions. `then`, `catchError`,
// `whenComplete`, `listen`, `setState`, `addPostFrameCallback`, `compute` and the Timer
// family all take a closure and none of them iterates — counting them would put a
// per-iteration finding on essentially every asynchronous method in Flutter, which is
// most of them.
var dartLoopMethods = map[string]bool{
	"map": true, "forEach": true, "where": true, "expand": true, "fold": true,
	"reduce": true, "any": true, "every": true, "takeWhile": true, "skipWhile": true,
	"firstWhere": true, "lastWhere": true, "singleWhere": true, "indexWhere": true,
	"whereType": true, "sort": true, "removeWhere": true, "retainWhere": true,
	"followedBy": true, "asMap": true, "generate": true,
}

// isConstantTripLoop reports a `for` whose trip count is fixed.
//
// `for (final x in items)` scales with items and must keep its factor of n; `for (var i
// = 0; i < 10; i++)` runs a fixed number of times and must not, or an honest O(n)
// becomes a fabricated O(n²). The two are the same node kind and are told apart by
// their `for_loop_parts`: the C-style form holds a `relational_expression` condition,
// the for-in form holds none.
//
// The bound must be a LITERAL. A named constant could be anything, and `i < xs.length`
// is the scaling case wearing the C-style form's clothes.
func (w *walker) isConstantTripLoop(n *sitter.Node) bool {
	if kindOf(n) != "for_statement" {
		return false
	}
	parts := childOfKind(n, "for_loop_parts")
	if parts == nil {
		return false
	}
	cond := childOfKind(parts, "relational_expression")
	if cond == nil {
		// No condition clause: this is `for (x in xs)`, which scales.
		return false
	}
	// The literal has to be in the CONDITION, not merely somewhere in the loop — a
	// body full of integer literals says nothing about the trip count.
	return childOfKind(cond, "decimal_integer_literal") != nil ||
		childOfKind(cond, "hex_integer_literal") != nil
}

// isConstantTripIterator reports an iterator applied to a collection literal.
func (w *walker) isConstantTripIterator(kids []*sitter.Node, i int) bool {
	for j := i - 1; j >= 0; j-- {
		if kindOf(kids[j]) == "selector" {
			continue
		}
		return kindOf(kids[j]) == "list_literal" || kindOf(kids[j]) == "set_or_map_literal"
	}
	return false
}

// ---------------------------------------------------------------------------
// I/O detection
// ---------------------------------------------------------------------------

// ioPackages are the import URIs that make a file capable of doing I/O at all.
//
// This is the gate the whole heuristic rests on. Dart's I/O verbs are `get`, `post`,
// `query`, `insert`, `update` and `read` — the most generic method names in the
// language, and matching them unguarded would flag every map lookup and every state
// update. Because a Dart file cannot reach an I/O API it has not imported, requiring
// the import first turns a guess into a precondition.
var ioPackages = []string{
	"dart:io", "dart:html",
	"package:http/", "package:dio/", "package:chopper/", "package:retrofit/",
	"package:sqflite/", "package:drift/", "package:isar/", "package:hive/",
	"package:cloud_firestore/", "package:firebase_", "package:supabase",
	"package:shared_preferences/", "package:path_provider/",
	"package:web_socket_channel/", "package:graphql_flutter/", "package:grpc/",
	"package:flutter/services.dart", // rootBundle
}

// importsIO reports whether this file imports anything that can perform I/O.
func (w *walker) importsIO() bool {
	for _, uri := range w.importURIs {
		for _, p := range ioPackages {
			if strings.HasPrefix(uri, p) {
				return true
			}
		}
	}
	return false
}

// distinctiveIOCalls are unambiguous regardless of receiver: nothing in Dart is named
// `readAsString` or `rawQuery` except the I/O operation.
var distinctiveIOCalls = map[string]bool{
	"readAsString": true, "readAsBytes": true, "readAsLines": true,
	"writeAsString": true, "writeAsBytes": true, "openRead": true, "openWrite": true,
	"rawQuery": true, "rawInsert": true, "rawUpdate": true, "rawDelete": true,
	"openDatabase": true, "getInstance": true, "loadString": true,
	"getApplicationDocumentsDirectory": true, "getTemporaryDirectory": true,
	"getDownloadURL": true, "putFile": true, "putData": true,
}

// ioVerbs are I/O only when the receiver looks like a client.
var ioVerbs = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true,
	"head": true, "read": true, "send": true, "fetch": true, "query": true,
	"insert": true, "update": true, "select": true, "download": true, "upload": true,
}

// ioReceiverTokens are the receiver-name fragments that mark an I/O client.
var ioReceiverTokens = []string{
	"http", "dio", "client", "api", "db", "database", "firestore", "storage",
	"prefs", "bundle", "supabase", "socket", "repository", "service", "gateway",
	"box", "isar", "channel",
}

// isIOCall reports whether an invocation performs network, file or database I/O.
//
// Two tiers behind one gate. The gate is the file's imports; above it, a distinctive
// name needs no receiver evidence, while a generic verb needs a receiver that looks
// like a client. This mirrors the shape the TypeScript, Swift and JVM analyzers
// converged on, and for the same reason: the verb list alone is too generic to be
// trusted, and dropping the verbs entirely would miss most real HTTP calls.
func (w *walker) isIOCall(name, receiver string) bool {
	if !w.importsIO() {
		return false
	}
	if distinctiveIOCalls[name] {
		return true
	}
	if !ioVerbs[name] {
		return false
	}
	if receiver == "" {
		return false
	}
	lower := strings.ToLower(receiver)
	for _, tok := range ioReceiverTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

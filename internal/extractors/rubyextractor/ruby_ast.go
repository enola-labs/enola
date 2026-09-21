package rubyextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// sortedKeys returns the keys of a set in deterministic (sorted) order — used to
// emit stable prop values into facts.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// extractFileAST parses a Ruby file with tree-sitter and emits architectural
// facts. It replaces the former line-based regex scanner: every symbol, import,
// mixin, constant, attr, ActiveRecord storage/association, and RelCalls edge the
// regex produced is preserved here, with higher fidelity (heredocs, multi-line
// expressions, endless methods, and nested scopes are handled by the grammar).
func extractFileAST(src []byte, relFile string, isRails, exportedByPackwerk bool) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	w := &rubyWalker{
		src:                src,
		relFile:            relFile,
		dir:                factpath.Dir(relFile),
		isRails:            isRails,
		exportedByPackwerk: exportedByPackwerk,
		fileRefIdx:         -1,
	}
	root := tree.RootNode()
	w.walkBody(root)
	// Capture executable calls made at file scope (top-level assignment RHS,
	// conditionals, fixture `Badge.foo(...)`, plugin `after_initialize` blocks) on
	// the file-scope ref fact. walkForCalls returns at nested defs/classes, which
	// get their own pass.
	if owner := w.bodyCallOwner(); owner >= 0 {
		w.walkScopeForCalls(root, owner, map[string]bool{}, nil)
	}
	// Attach any dynamic-dispatch prefixes discovered anywhere in the file to the
	// file-scope fact so the collector can mark same-prefix methods as used.
	if len(w.dynamicPrefixes) > 0 {
		idx := w.ensureFileRefFact()
		w.out[idx].SetProp("dynamic_send_prefixes", sortedKeys(w.dynamicPrefixes))
	}
	// Drop the file-scope reference fact if it carries neither call edges nor
	// dynamic-dispatch prefixes, so empty facts never reach the store.
	if w.fileRefIdx >= 0 && len(w.out[w.fileRefIdx].Relations) == 0 &&
		w.out[w.fileRefIdx].PropAny("dynamic_send_prefixes") == nil {
		w.out = append(w.out[:w.fileRefIdx], w.out[w.fileRefIdx+1:]...)
	}
	return w.out
}

// ensureFileRefFact returns the index of this file's lazily-created file-scope
// reference fact (facts.KindFileRef), creating it on first use.
// setModelTable corrects the enclosing model's table claim with the one the
// class declared, and records that it was declared rather than derived — the
// same distinction the association work carries, for the same reason: a derived
// name is a convention holding and a declared one is a statement in the source.
func (w *rubyWalker) setModelTable(table string) {
	for i := len(w.out) - 1; i >= 0; i-- {
		fact := w.out[i]
		if fact.Kind != facts.KindStorage || fact.Props == nil {
			continue
		}
		if kind, _ := fact.PropAny("storage_kind").(string); kind != "model" {
			continue
		}
		fact.SetProp("table", table)
		fact.SetProp("table_source", "declared")
		return
	}
}

// setModuleTableNamePrefix records on the enclosing module's symbol fact the
// literal its `def self.table_name_prefix` returns. Only a module carries one —
// Rails reads it off the namespace a model is nested in — and only a body that
// is a single plain string is read: a computed or interpolated prefix is a value
// this pass cannot know, and it states nothing rather than a guess.
func (w *rubyWalker) setModuleTableNamePrefix(node *sitter.Node) {
	s := w.cur()
	if s == nil || s.kind != "module" || s.symFactIdx < 0 {
		return
	}
	if prefix := plainStringBody(node.ChildByFieldName("body"), w.src); prefix != "" {
		w.out[s.symFactIdx].SetProp("table_name_prefix", prefix)
	}
}

// plainStringBody returns the literal a method body consists of when that body is
// exactly one plain string — one statement, no interpolation. Every other shape
// returns "", so a prefix assembled at runtime never becomes a fact.
func plainStringBody(body *sitter.Node, src []byte) string {
	if body == nil {
		return ""
	}
	node := body
	if node.Kind() == "body_statement" {
		var only *sitter.Node
		for i := uint(0); i < node.NamedChildCount(); i++ {
			child := node.NamedChild(i)
			if child.Kind() == "comment" {
				continue
			}
			if only != nil {
				return ""
			}
			only = child
		}
		node = only
	}
	if node == nil || node.Kind() != "string" {
		return ""
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		if node.NamedChild(i).Kind() != "string_content" {
			return ""
		}
	}
	return stringLiteralContent(node, src)
}

func (w *rubyWalker) ensureFileRefFact() int {
	if w.fileRefIdx < 0 {
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindFileRef,
			Name:  w.relFile,
			File:  w.relFile,
			Props: map[string]any{"language": "ruby"},
		})
		w.fileRefIdx = len(w.out) - 1
	}
	return w.fileRefIdx
}

// bodyCallOwner returns the index of the fact that class/module-body and
// top-level call edges should attach to: the enclosing class/module symbol fact
// when inside a type scope, otherwise (top level, or an eigenclass body whose
// symFactIdx is -1) the lazily-created file-scope reference fact.
func (w *rubyWalker) bodyCallOwner() int {
	if s := w.cur(); s != nil && s.symFactIdx >= 0 {
		return s.symFactIdx
	}
	return w.ensureFileRefFact()
}

// rubyScope tracks a class/module/eigenclass nesting level.
type rubyScope struct {
	name         string // simple (last) name; "" for an eigenclass (class << self)
	kind         string // "class", "module", or "eigenclass"
	visibility   string // "public" | "private" | "protected"
	moduleFunc   bool   // module_function active: subsequent defs are class methods
	isModel      bool   // ActiveRecord model: associations/scopes/table_name apply
	isSerializer bool   // ActiveModel::Serializer: attributes/associations back methods
	// hasInstanceMethod records that this scope directly defined a `def foo`
	// (instance, not `def self.x`) method. For a module this signals a mixin
	// (meant to be included), which makes the module abstract for package metrics.
	hasInstanceMethod bool
	symFactIdx        int // index into w.out of this scope's class/module symbol fact, or -1
}

type rubyWalker struct {
	src                []byte
	relFile            string
	dir                string
	isRails            bool
	exportedByPackwerk bool

	out        []facts.Fact
	scopeStack []rubyScope

	// fileRefIdx is the index into out of the lazily-created file-scope reference
	// fact (facts.KindFileRef) that holds top-level call edges; -1 until first used.
	fileRefIdx int

	// dynamicPrefixes accumulates the static prefixes of interpolated symbols
	// (`:"report_#{type}"` -> "report_") seen anywhere in the file. They mark
	// dynamic dispatch (public_send/send by computed name), letting the dead-code
	// detector treat same-prefix methods as used. File-global; nil until first hit.
	dynamicPrefixes map[string]bool

	// pendingStrPrefixes / sawDispatcher gate interpolated-STRING prefixes
	// (`"present_#{idx}"`) per scope: unlike symbols, snake_case strings are commonly
	// cache/Redis keys, so a string prefix is committed to dynamicPrefixes only when
	// the same scope also invokes a dispatcher (send/public_send/__send__/try). Reset
	// at each scope-entry walk (walkScopeForCalls / handleMethod) and committed after.
	pendingStrPrefixes map[string]bool
	sawDispatcher      bool

	// Per-method complexity state, set up by handleMethod around walkForCalls.
	// metrics is nil outside a method body walk. loopDepth is the current loop
	// nesting depth; selfName/selfShort are the enclosing method's full and short
	// names (for direct-recursion detection — Ruby self-calls are usually bare).
	metrics   *rubyBodyMetrics
	loopDepth int
	selfName  string
	selfShort string
}

// rubyBodyMetrics accumulates per-method complexity signals during the single
// walkForCalls body traversal — mirrors the Go/Python extractors.
type rubyBodyMetrics struct {
	loopDepth   int             // max loop nesting depth
	loopCount   int             // number of loop constructs (syntactic + iterator blocks)
	decisions   int             // decision points (cyclomatic = 1 + decisions)
	callsInLoop []string        // distinct call targets invoked at loop depth >= 1
	inLoopSeen  map[string]bool // dedup set for callsInLoop
	recursive   bool            // body directly calls the enclosing method

	// fieldsRead/fieldsWritten record which instance variables the body
	// touches. They answer "which methods actually use @client", which is the
	// question behind every extract-class refactor and which the graph could
	// not be asked before.
	//
	// Instance variables only. Class variables are a separate namespace and
	// merging them would overstate cohesion, per the fact-model research; a
	// controller's @ivars are view parameters rather than encapsulated state,
	// which is why the population that matters here is services (0.68 ivar
	// references per method) rather than models (0.13, and 146 of 503 of those
	// are memoization).
	fieldsRead    []string
	fieldsWritten []string
	fieldSeen     map[string]bool

	// blockBindings records `param=collection` for each enumerable block in the
	// body. It is the one piece of type information Ruby gives away for free,
	// and it is exactly the N+1 shape: `form_questions.each { |q| q.form_answers }`
	// issues a query per iteration only because `q` is a FormQuestion. Without
	// the binding, `q.form_answers` and `client.post` are the same string to
	// this graph, which is why the association-read detector measured to zero
	// true findings on the monolith.
	//
	// The binding is recorded, not resolved. Turning `form_questions` into
	// `FormQuestion` needs the model index, which lives a layer up — the
	// extractor states what it saw and the consumer joins it.
	blockBindings []string
	blockSeen     map[string]bool
	// localChains remembers, per local, the receiver chain it was last assigned
	// from (`users = company.users.active` -> users: [company users active]), so a
	// binding on that local names the relation it was built from rather than the
	// local's name. A local assigned from a chain that starts with itself
	// (`x = x.includes(:y)`) extends its own chain.
	localChains map[string][]string
	// batchLoader marks a body that hands per-element reads to BatchLoader.for:
	// the loop inside its batch block is batched by construction.
	batchLoader bool
	// params are the method's parameter names, emitted only alongside block
	// bindings, so the consumer of those can tell a collection that arrives as
	// an argument (typed by nothing but its name) from one the body built.
	params []string

	// localTypes records `name=Class` for a variable assigned from a constant's
	// factory or finder. It is the receiver information this graph has never
	// had: 1,062 such assignments on the monolith, typing 1,238 association
	// reads that currently resolve to nothing because `@meeting.candidates` and
	// `client.post` are the same shape without it.
	//
	// Only a constant receiver types anything. `x = helper.build` says nothing
	// about x, and guessing there is the name-coincidence failure that produced
	// seven candidates and zero true findings.
	localTypes []string
	localSeen  map[string]bool

	// unpersistedLocals records the names in localTypes whose typing call was
	// `new`: the receiver is an instance the graph knows the type of, and one
	// that has never been saved, so reading an association on it builds in
	// memory and issues no query. `Company.new(...)` followed by
	// `company.pages.build` in a loop is a mock, not an N+1. Kept apart from
	// localTypes so consumers that only need the type read nothing new.
	unpersistedLocals []string
	unpersistedSeen   map[string]bool

	// preloads records every association name handed to `includes`,
	// `preload` or `eager_load` in the body, symbols and hash keys and values
	// alike, so a consumer can tell an association read that hits a preloaded
	// cache from one that queries per element. Recorded, not resolved: the
	// extractor cannot see whether the relation that was preloaded is the one
	// being iterated, so this is a per-method statement of intent that a
	// consumer weighs, and it errs toward silence.
	preloads    []string
	preloadSeen map[string]bool
}

// typingMethods are the constant-receiver calls whose result is an instance of
// that constant. A class method that returns something else — `Company.count`,
// `Company.table_name` — types nothing.
var typingMethods = map[string]bool{
	"new": true, "find": true, "find_by": true, "find_by!": true,
	"create": true, "create!": true, "first": true, "last": true,
	"find_or_create_by": true, "find_or_initialize_by": true,
}

func (m *rubyBodyMetrics) recordLocalType(name, class string) {
	if m == nil || name == "" || class == "" {
		return
	}
	entry := name + "=" + class
	if m.localSeen == nil {
		m.localSeen = map[string]bool{}
	}
	if m.localSeen[entry] {
		return
	}
	m.localSeen[entry] = true
	m.localTypes = append(m.localTypes, entry)
}

func (m *rubyBodyMetrics) recordUnpersistedLocal(name string) {
	if m == nil || name == "" {
		return
	}
	if m.unpersistedSeen == nil {
		m.unpersistedSeen = map[string]bool{}
	}
	if m.unpersistedSeen[name] {
		return
	}
	m.unpersistedSeen[name] = true
	m.unpersistedLocals = append(m.unpersistedLocals, name)
}

func (m *rubyBodyMetrics) recordPreload(association string) {
	if m == nil || association == "" {
		return
	}
	if m.preloadSeen == nil {
		m.preloadSeen = map[string]bool{}
	}
	if m.preloadSeen[association] {
		return
	}
	m.preloadSeen[association] = true
	m.preloads = append(m.preloads, association)
}

func (m *rubyBodyMetrics) recordBlockBinding(param, collection string) {
	if m == nil || param == "" || collection == "" {
		return
	}
	entry := param + "=" + collection
	if m.blockSeen == nil {
		m.blockSeen = map[string]bool{}
	}
	if m.blockSeen[entry] {
		return
	}
	m.blockSeen[entry] = true
	m.blockBindings = append(m.blockBindings, entry)
}

func (m *rubyBodyMetrics) recordLocalChain(name string, chain []string) {
	if m == nil || name == "" || len(chain) == 0 {
		return
	}
	if m.localChains == nil {
		m.localChains = map[string][]string{}
	}
	m.localChains[name] = m.spliceChain(chain)
}

// spliceChain replaces a chain's first segment with the chain that local was
// assigned from, when there is one, so every chain bottoms out in what the body
// actually started from.
func (m *rubyBodyMetrics) spliceChain(chain []string) []string {
	if m == nil || len(chain) == 0 {
		return chain
	}
	base, ok := m.localChains[chain[0]]
	if !ok {
		return chain
	}
	out := make([]string, 0, len(base)+len(chain)-1)
	out = append(out, base...)
	out = append(out, chain[1:]...)
	return out
}

// chainWrappers are receiverless calls whose result is their first argument's
// relation, narrowed: `policy_scope(candidate.actions)` is still candidate.actions.
var chainWrappers = map[string]bool{"policy_scope": true, "Array": true}

// receiverChain flattens a receiver expression into the names along it, arguments
// dropped: `Current.company.users.allowed_to_login.preload(:x)` becomes
// [Current company users allowed_to_login preload]. Nil when the expression is
// not a chain of names (an element reference, a literal, a ternary).
func receiverChain(node *sitter.Node, src []byte) []string {
	if node == nil {
		return nil
	}
	switch kindOf(node) {
	case "identifier", "instance_variable", "class_variable", "global_variable", "constant", "scope_resolution", "self":
		return []string{rubyText(node, src)}
	case "parenthesized_statements":
		if node.NamedChildCount() == 1 {
			return receiverChain(node.NamedChild(0), src)
		}
		return nil
	case "call":
		meth := node.ChildByFieldName("method")
		if meth == nil {
			return nil
		}
		name := rubyText(meth, src)
		recv := node.ChildByFieldName("receiver")
		if recv == nil {
			if chainWrappers[name] {
				if args := node.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
					return receiverChain(args.NamedChild(0), src)
				}
			}
			return []string{name}
		}
		base := receiverChain(recv, src)
		if base == nil {
			return nil
		}
		return append(base, name)
	}
	return nil
}

// recordFieldAccess notes one instance-variable read or write on the method
// being walked. Deduped per method and per mode: the fact is which members a
// method touches, not how many times.
func (m *rubyBodyMetrics) recordFieldAccess(name string, write bool) {
	if m == nil || name == "" || !strings.HasPrefix(name, "@") || strings.HasPrefix(name, "@@") {
		return
	}
	if m.fieldSeen == nil {
		m.fieldSeen = map[string]bool{}
	}
	mode := "r"
	if write {
		mode = "w"
	}
	if m.fieldSeen[mode+name] {
		return
	}
	m.fieldSeen[mode+name] = true
	if write {
		m.fieldsWritten = append(m.fieldsWritten, name)
		return
	}
	m.fieldsRead = append(m.fieldsRead, name)
}

// rubyIterators are methods whose block runs once per element — i.e. a loop.
// Block-taking methods NOT in this set (transaction, tap, synchronize,
// File.open, …) run their block once and are deliberately not treated as loops.
// Aggregate-or-iterate methods (count/sum/find/all?…) are safe to include
// because a block is required before any of these counts as a loop.
//
// find_in_batches / in_batches are deliberately excluded: their block runs once
// per *batch* (an array), so the real per-element loop is the inner .each/.map
// over that batch. Counting the batch block as a loop as well double-counts and
// mislabels a single O(n) pass as O(n²). (find_each yields individual elements
// and is a genuine per-element loop, so it stays.) Both names remain in the
// analyzer's expensiveMethods gate, so a batch scan nested inside another loop is
// still flagged by name — only the spurious extra depth is dropped.
var rubyIterators = map[string]bool{
	"each": true, "each_with_index": true, "each_with_object": true,
	"each_pair": true, "each_key": true, "each_value": true,
	"each_slice": true, "each_cons": true, "each_line": true,
	"each_char": true, "each_entry": true,
	"map": true, "map!": true, "collect": true, "collect!": true,
	"flat_map": true, "select": true, "select!": true, "filter": true,
	"filter_map": true, "reject": true, "reject!": true,
	"detect": true, "find": true, "find_all": true, "find_index": true,
	"find_each": true,
	"reduce":    true, "inject": true, "min_by": true, "max_by": true,
	"sort_by": true, "group_by": true, "partition": true, "chunk_while": true,
	"zip": true, "cycle": true, "times": true, "upto": true, "downto": true,
	"step": true, "loop": true, "all?": true, "any?": true, "none?": true,
	"one?": true, "count": true, "sum": true, "tally_by": true,
}

// recordCallMetrics notes a resolved call target against the current method's
// complexity metrics: flags direct recursion and records calls made inside loops.
func (w *rubyWalker) recordCallMetrics(target string) {
	if w.metrics == nil || target == "" {
		return
	}
	if target == w.selfShort || target == w.selfName || target == "self."+w.selfShort {
		w.metrics.recursive = true
	}
	w.recordInLoopCall(target)
}

// recordSelfAwareMetrics records a call target's metrics but only counts it as
// recursion when the call dispatches to the SAME object as the enclosing method —
// a receiverless call or a plain `self.foo`. An explicit receiver (`x.foo`,
// `self.class.foo`, `obj.try(:foo)`) targets a different object or a sibling
// class/instance method, so it feeds the in-loop N+1 signal but never the recursion
// flag. (A `Const.foo` that resolves to this exact method is handled by the caller
// via a selfName match before reaching here.)
func (w *rubyWalker) recordSelfAwareMetrics(target string, recv *sitter.Node) {
	if recv == nil || kindOf(recv) == "self" {
		w.recordCallMetrics(target)
		return
	}
	w.recordInLoopCall(target)
}

// recordInLoopCall adds a target to calls_in_loop (deduped) when inside a loop,
// without the recursion check — used for raw instance-method names (e.g. an
// association read `u.posts`) whose name must not be mistaken for self-recursion.
func (w *rubyWalker) recordInLoopCall(target string) {
	if w.metrics == nil || target == "" || w.loopDepth == 0 {
		return
	}
	if w.metrics.inLoopSeen == nil {
		w.metrics.inLoopSeen = make(map[string]bool)
	}
	if !w.metrics.inLoopSeen[target] {
		w.metrics.inLoopSeen[target] = true
		w.metrics.callsInLoop = append(w.metrics.callsInLoop, target)
	}
}

// rubyCheapMethods are obviously-cheap attribute/Enumerable/Kernel methods that
// are not DB I/O. No-arg instance calls to these inside loops are not recorded in
// calls_in_loop, to keep it focused (the analyzer's association/keyword gate is
// the real precision filter, so this list need not be exhaustive).
var rubyCheapMethods = map[string]bool{
	"id": true, "name": true, "to_s": true, "to_str": true, "to_i": true,
	"to_a": true, "to_h": true, "to_sym": true, "to_param": true, "inspect": true,
	"hash": true, "class": true, "object_id": true, "freeze": true, "frozen?": true,
	"dup": true, "clone": true, "present?": true, "blank?": true, "nil?": true,
	"empty?": true, "any?": true, "size": true, "length": true, "first": true,
	"last": true, "keys": true, "values": true, "key?": true, "include?": true,
	"is_a?": true, "kind_of?": true, "instance_of?": true, "respond_to?": true,
	"tap": true, "then": true, "itself": true, "send": true, "public_send": true,
}

// rubyPreloaders are the ActiveRecord calls that load associations ahead of
// the reads that follow, so an association read on the elements of that
// relation is a cache hit rather than a query per element.
var rubyPreloaders = map[string]bool{"includes": true, "preload": true, "eager_load": true}

// symbolNamesIn collects every symbol in an argument list, descending into
// arrays and hashes so `includes(:questions, answers: :author)` yields
// questions, answers and author. Interpolated symbols name nothing.
// parameterNames lists a method's parameter names in declaration order, the
// bare identifier of each optional, keyword, splat or block parameter included.
func parameterNames(params *sitter.Node, src []byte) []string {
	if params == nil {
		return nil
	}
	var out []string
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		if kindOf(p) == "identifier" {
			out = append(out, rubyText(p, src))
			continue
		}
		if name := p.ChildByFieldName("name"); name != nil {
			out = append(out, rubyText(name, src))
			continue
		}
		for j := uint(0); j < p.NamedChildCount(); j++ {
			if c := p.NamedChild(j); c != nil && kindOf(c) == "identifier" {
				out = append(out, rubyText(c, src))
				break
			}
		}
	}
	return out
}

// preloadsIn collects every association named by an includes / preload /
// eager_load call anywhere under node: the body of a scope lambda, or a
// relation-returning expression.
func preloadsIn(node *sitter.Node, src []byte) []string {
	if node == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(n) == "call" {
			if meth := n.ChildByFieldName("method"); meth != nil && rubyPreloaders[rubyText(meth, src)] {
				for _, a := range symbolNamesIn(n.ChildByFieldName("arguments"), src) {
					if !seen[a] {
						seen[a] = true
						out = append(out, a)
					}
				}
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(node)
	sort.Strings(out)
	return out
}

func symbolNamesIn(args *sitter.Node, src []byte) []string {
	if args == nil {
		return nil
	}
	var out []string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "simple_symbol":
			out = append(out, strings.TrimPrefix(rubyText(n, src), ":"))
			return
		case "hash_key_symbol":
			out = append(out, rubyText(n, src))
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(args)
	return out
}

// --- scope helpers ---

func (w *rubyWalker) push(s rubyScope) { w.scopeStack = append(w.scopeStack, s) }
func (w *rubyWalker) pop()             { w.scopeStack = w.scopeStack[:len(w.scopeStack)-1] }

func (w *rubyWalker) cur() *rubyScope {
	if len(w.scopeStack) == 0 {
		return nil
	}
	return &w.scopeStack[len(w.scopeStack)-1]
}

// scopeQual joins the enclosing class/module names into a Ruby-qualified name.
// Eigenclass and anonymous entries do not contribute.
func (w *rubyWalker) scopeQual() string {
	var parts []string
	for _, s := range w.scopeStack {
		if s.kind == "eigenclass" || s.name == "" {
			continue
		}
		parts = append(parts, s.name)
	}
	return strings.Join(parts, "::")
}

// curVisibility returns the visibility of the innermost type scope.
func (w *rubyWalker) curVisibility() string {
	if s := w.cur(); s != nil && s.visibility != "" {
		return s.visibility
	}
	return "public"
}

// inEigenclass reports whether the innermost scope is an eigenclass.
func (w *rubyWalker) inEigenclass() bool {
	s := w.cur()
	return s != nil && s.kind == "eigenclass"
}

func (w *rubyWalker) exported() bool {
	return w.curVisibility() == "public" && w.exportedByPackwerk
}

// --- body walking ---

// walkBody iterates the statements of a program or body_statement, dispatching
// each. It is the single entry point for both top-level and nested scopes.
func (w *rubyWalker) walkBody(node *sitter.Node) {
	if node == nil {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkStatement(node.Child(i))
	}
}

func (w *rubyWalker) walkStatement(node *sitter.Node) {
	if node == nil || !node.IsNamed() {
		return
	}
	switch kindOf(node) {
	case "module":
		w.handleModule(node)
	case "class":
		w.handleClass(node)
	case "singleton_class":
		w.handleSingletonClass(node)
	case "method":
		w.handleMethod(node, w.classMethodContext())
	case "singleton_method":
		w.handleMethod(node, true)
	case "assignment":
		w.handleAssignment(node)
	case "call":
		w.handleBodyCall(node)
		// Executable call EDGES in this statement (macro args, qualified
		// `Const.method` calls, calls inside blocks) are captured by the per-scope
		// walkForCalls pass run in handleClass/handleModule/extractFileAST — not here
		// — so assignments and every other statement kind are covered uniformly.
		// This case still descends into a trailing do/brace block to capture nested
		// DECLARATIONS (def/class/const inside included/class_methods/concerning blocks).
		if body := blockBody(node); body != nil {
			w.walkBody(body)
		}
	case "identifier":
		// Bare statements: visibility markers and module_function.
		switch rubyText(node, w.src) {
		case "private", "protected", "public":
			if s := w.cur(); s != nil {
				s.visibility = rubyText(node, w.src)
			}
		case "module_function":
			if s := w.cur(); s != nil {
				s.moduleFunc = true
			}
		}
	case "comment":
		// ignore
	default:
		// Control-flow / grouping containers (if, unless, begin, case, while,
		// modifiers, ...): descend so nested require/include/def/const
		// declarations are captured, as the line-based scanner did.
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkStatement(node.Child(i))
		}
	}
}

// classMethodContext reports whether a plain `def` in the current scope should be
// treated as a class method (eigenclass body or after module_function).
func (w *rubyWalker) classMethodContext() bool {
	if w.inEigenclass() {
		return true
	}
	if s := w.cur(); s != nil && s.moduleFunc {
		return true
	}
	return false
}

// --- modules / classes ---

func (w *rubyWalker) handleModule(node *sitter.Node) {
	name := w.constName(node.ChildByFieldName("name"))
	if name == "" {
		return
	}
	qual := w.qualify(name)
	body := node.ChildByFieldName("body")

	props := map[string]any{
		"symbol_kind": facts.SymbolInterface,
		"exported":    w.exportedByPackwerk,
		"language":    "ruby",
		// Package-metrics abstractness: a Ruby module is only "abstract" when it is
		// a mixin (defines instance methods or is an ActiveSupport::Concern). Most
		// Rails modules are pure namespaces (`module Api; class Foo`), so default to
		// concrete and promote to abstract below once the body is known.
		"abstract": false,
	}
	if bodyHasConcern(body, w.src) {
		props["concern"] = true
		props["abstract"] = true // Concern = behavior mixed into includers
	}
	if w.isRails {
		props["framework"] = "rails"
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      qual,
		File:      w.relFile,
		Line:      line(node),
		EndLine:   endLine(node),
		Column:    column(node),
		EndColumn: endColumn(node),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})

	modIdx := len(w.out) - 1
	w.push(rubyScope{name: name, kind: "module", visibility: "public", symFactIdx: modIdx})
	w.walkBody(body)
	// Capture executable calls made directly in the module body (see handleClass).
	w.walkScopeForCalls(body, modIdx, map[string]bool{}, nil)
	// A module that defined instance methods during the walk is a mixin → abstract.
	// props is shared by reference with the fact appended above, so this updates it
	// in place (same mechanism handleMethod uses for cyclomatic).
	if s := w.cur(); s != nil && s.hasInstanceMethod {
		props["abstract"] = true
	}
	w.pop()
}

func (w *rubyWalker) handleClass(node *sitter.Node) {
	name := w.constName(node.ChildByFieldName("name"))
	if name == "" {
		return
	}
	qual := w.qualify(name)
	superclass := w.superclassName(node.ChildByFieldName("superclass"))
	superclassBase := superclass
	if i := strings.IndexByte(superclassBase, '('); i >= 0 {
		superclassBase = strings.TrimSpace(superclassBase[:i])
	}

	props := map[string]any{
		"symbol_kind": facts.SymbolClass,
		"exported":    w.exported(),
		"language":    "ruby",
	}
	if w.isRails {
		props["framework"] = "rails"
	}
	if superclassBase != "" {
		props["superclass"] = superclassBase
	}
	// What KIND of Rails thing this is — job, mailer, channel, policy, controller,
	// component. Derived from the superclass first and the path only as a fallback,
	// because `< ApplicationJob` is what Rails dispatches on while `app/services` is a
	// convention with no framework meaning.
	if w.isRails {
		if c := railsComponent(qual, superclassBase, nil, w.relFile); c != "" {
			props["rails_component"] = c
		}
	}
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}}
	if superclassBase != "" {
		rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: superclassBase})
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      qual,
		File:      w.relFile,
		Line:      line(node),
		EndLine:   endLine(node),
		Column:    column(node),
		EndColumn: endColumn(node),
		Props:     props,
		Relations: rels,
	})
	clsIdx := len(w.out) - 1

	// ActiveRecord model: emit a storage fact and flag the scope so the body
	// scan picks up associations, scopes, and explicit table names. Sequel
	// models get the same companion shape: `class X < Sequel::Model` and the
	// dataset form `Sequel::Model(:table)`, whose literal argument is the
	// physical table (inferTableName is the fallback when the argument is
	// absent or dynamic).
	// `self.abstract_class = true` says this class backs no table — it exists to
	// be inherited from. ApplicationRecord is the canonical one, and emitting a
	// storage fact for it invents a table called application_records.
	isModel := isARBaseClass(superclassBase) && !declaresAbstractClass(node, w.src)
	sequelTable, isSequel := sequelModelBase(superclass)
	if isModel || isSequel {
		table := inferTableName(qual)
		framework := "rails"
		if isSequel {
			framework = "sequel"
			if sequelTable != "" {
				table = sequelTable
			}
		}
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindStorage,
			Name:      qual,
			File:      w.relFile,
			Line:      line(node),
			EndLine:   endLine(node),
			Column:    column(node),
			EndColumn: endColumn(node),
			Props: map[string]any{
				"storage_kind": "model",
				"table":        table,
				"table_source": "derived",
				"language":     "ruby",
				"framework":    framework,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
		})
		isModel = true
	}

	w.push(rubyScope{name: name, kind: "class", visibility: "public", isModel: isModel,
		isSerializer: isSerializerBase(superclass), symFactIdx: clsIdx})
	body := node.ChildByFieldName("body")
	w.walkBody(body)
	// Capture executable calls made directly in the class body (assignment RHS,
	// conditionals, hash/Proc literals, macro args) as uses of this class.
	// walkForCalls returns at nested defs/classes, which get their own pass.
	w.walkScopeForCalls(body, clsIdx, map[string]bool{}, nil)
	w.pop()
}

func (w *rubyWalker) handleSingletonClass(node *sitter.Node) {
	// class << self — methods inside become class (singleton) methods. The
	// eigenclass entry carries no name and does not affect qualification.
	w.push(rubyScope{name: "", kind: "eigenclass", visibility: "public", symFactIdx: -1})
	body := node.ChildByFieldName("body")
	w.walkBody(body)
	// The eigenclass has no symbol fact (symFactIdx -1); attribute any executable
	// calls in its body to the file-scope ref fact via bodyCallOwner.
	if owner := w.bodyCallOwner(); owner >= 0 {
		w.walkScopeForCalls(body, owner, map[string]bool{}, nil)
	}
	w.pop()
}

// --- methods ---

func (w *rubyWalker) handleMethod(node *sitter.Node, isClassMethod bool) {
	name := rubyText(node.ChildByFieldName("name"), w.src)
	if name == "" {
		return
	}

	// `def self.table_name_prefix` states what Rails puts in front of every table
	// name derived under this namespace. The models it governs live in other
	// files, so the literal belongs on the module's own fact, where the whole-repo
	// pass that corrects them can read it.
	if isClassMethod && name == "table_name_prefix" {
		w.setModuleTableNamePrefix(node)
	}

	// An instance method (`def foo`, not `def self.x`) directly in a module body
	// marks that module as a mixin — behavior meant to be included into another
	// type — which makes it abstract for package metrics. Fetch the scope fresh:
	// push() can reallocate scopeStack, so a cached pointer would be stale.
	if !isClassMethod {
		if s := w.cur(); s != nil && s.kind == "module" {
			s.hasInstanceMethod = true
		}
	}

	scope := w.scopeQual()
	var fullName string
	switch {
	case scope == "":
		fullName = w.dir + "." + name
	case isClassMethod:
		fullName = scope + "." + name
	default:
		fullName = scope + "#" + name
	}

	symbolKind := facts.SymbolMethod
	if isClassMethod {
		symbolKind = facts.SymbolFunc
	}
	props := map[string]any{
		"symbol_kind": symbolKind,
		"exported":    w.exported(),
		"language":    "ruby",
	}
	if w.isRails {
		props["framework"] = "rails"
	}

	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      fullName,
		File:      w.relFile,
		Line:      line(node),
		EndLine:   endLine(node),
		Column:    column(node),
		EndColumn: endColumn(node),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
	ownerIdx := len(w.out) - 1

	// Accumulate RelCalls from the body onto this method (deduplicated), while
	// computing complexity metrics in the same walk. The props map is shared by
	// reference with the fact just appended, so writing to it after the walk
	// updates the emitted fact.
	seen := make(map[string]bool)
	locals := collectLocals(node, w.src)
	// The method scope (parameters + body) gates interpolated-string prefixes: reset
	// the tentative state before the walks and commit after (see commitPendingStrPrefixes).
	w.pendingStrPrefixes = nil
	w.sawDispatcher = false
	// Default parameter values (`def f(x = self.class.foo)`) contain real call
	// references. Walk them with metrics off (params are not the body, so they must
	// not affect the complexity score); seen is shared so body calls still dedup.
	w.walkForCalls(node.ChildByFieldName("parameters"), ownerIdx, seen, locals)
	w.metrics = &rubyBodyMetrics{params: parameterNames(node.ChildByFieldName("parameters"), w.src)}
	w.loopDepth = 0
	w.selfName = fullName
	w.selfShort = name
	w.walkForCalls(node.ChildByFieldName("body"), ownerIdx, seen, locals)
	props["cyclomatic"] = 1 + w.metrics.decisions
	if w.metrics.loopDepth > 0 {
		props["loop_depth"] = w.metrics.loopDepth
	}
	if w.metrics.loopCount > 0 {
		props["loop_count"] = w.metrics.loopCount
	}
	if len(w.metrics.callsInLoop) > 0 {
		props["calls_in_loop"] = w.metrics.callsInLoop
	}
	if len(w.metrics.fieldsRead) > 0 {
		sort.Strings(w.metrics.fieldsRead)
		props["fields_read"] = w.metrics.fieldsRead
	}
	if len(w.metrics.fieldsWritten) > 0 {
		sort.Strings(w.metrics.fieldsWritten)
		props["fields_written"] = w.metrics.fieldsWritten
	}
	if len(w.metrics.blockBindings) > 0 {
		sort.Strings(w.metrics.blockBindings)
		props["block_bindings"] = w.metrics.blockBindings
	}
	if len(w.metrics.unpersistedLocals) > 0 {
		sort.Strings(w.metrics.unpersistedLocals)
		props["unpersisted_locals"] = w.metrics.unpersistedLocals
	}
	if len(w.metrics.preloads) > 0 {
		sort.Strings(w.metrics.preloads)
		props["preloads"] = w.metrics.preloads
	}
	if w.metrics.batchLoader {
		props["batch_loader"] = true
	}
	if len(w.metrics.params) > 0 && len(w.metrics.blockBindings) > 0 {
		props["params"] = w.metrics.params
	}
	if len(w.metrics.localTypes) > 0 {
		sort.Strings(w.metrics.localTypes)
		props["local_types"] = w.metrics.localTypes
	}
	if w.metrics.recursive {
		props["recursive_self"] = true
	}
	w.metrics = nil
	w.commitPendingStrPrefixes()
}

// isClassHoldingReceiver reports whether a call receiver is a variable named by the
// Ruby idiom for a Class object (`klass`/`clazz`/`klazz`, plain or instance var). A
// method call on such a receiver is a class-method dispatch (`klass.inline`), not an
// attribute read, so it is recorded regardless of the method name.
func isClassHoldingReceiver(recv *sitter.Node, src []byte) bool {
	if recv == nil {
		return false
	}
	switch kindOf(recv) {
	case "identifier", "instance_variable":
		switch rubyText(recv, src) {
		case "klass", "clazz", "klazz", "@klass", "@clazz", "@klazz":
			return true
		}
	}
	return false
}

// isVarReceiver reports whether a call receiver node kind is a simple variable
// reference — a local/method identifier or an instance/class/global variable.
// A no-arg, underscored call on such a receiver (`items.preload_relations`,
// `@klass.bo_search_fields`) is a scope/class-method invocation, not an attribute
// read, so it is recorded as a reference.
func isVarReceiver(kind string) bool {
	switch kind {
	case "identifier", "instance_variable", "class_variable", "global_variable":
		return true
	}
	return false
}

// constantBoundReceiver reports whether an iterator's receiver is provably bounded by
// a compile-time constant, so the loop runs a fixed number of times regardless of the
// method's input (O(1) in n): an integer literal (`6.times`), a collection literal
// (`[…].each`, `{…}.each`, `%w[…]`, `%i[…]`), or an ALL-CAPS data constant
// (`STOP_CHARS.any?`). Mixed-case constants (classes like `User`) are excluded — a
// `.each` on a class/relation is not a bounded literal.
func constantBoundReceiver(recv *sitter.Node, src []byte) bool {
	if recv == nil {
		return false
	}
	switch kindOf(recv) {
	case "integer", "array", "hash", "string_array", "symbol_array":
		return true
	case "constant":
		return isScreamingSnake(rubyText(recv, src))
	case "call":
		// A trailing size-preserving/reducing chain method keeps a bounded base
		// bounded: `[a,b].compact.all?`, `%w[x y].map { … }.each`. Unwrap it and
		// re-check the inner receiver. Size-expanding ops (product/cycle/flat_map)
		// are excluded, so this never turns an unbounded source into "bounded".
		if m := recv.ChildByFieldName("method"); m != nil && chainPreservesBound[rubyText(m, src)] {
			return constantBoundReceiver(recv.ChildByFieldName("receiver"), src)
		}
	}
	return false
}

// chainPreservesBound are Enumerable methods that never grow a collection beyond its
// input size, so a bounded literal/constant piped through them stays bounded.
var chainPreservesBound = map[string]bool{
	"compact": true, "uniq": true, "flatten": true, "sort": true, "sort_by": true,
	"reverse": true, "to_a": true, "dup": true, "freeze": true,
	"map": true, "collect": true, "select": true, "filter": true, "reject": true,
	"first": true, "take": true,
}

// isScreamingSnake reports whether s is a SCREAMING_SNAKE_CASE data constant — only
// uppercase letters, digits, and underscores, with at least one letter.
func isScreamingSnake(s string) bool {
	hasLetter := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

// walkScopeForCalls runs walkForCalls over one scope (a class/module body or the
// top-level program) with the per-scope interpolated-string-prefix gate: it resets
// the tentative state, walks, then commits any pending string prefixes iff the scope
// invoked a dispatcher. (Method bodies are gated inline in handleMethod, which spans
// the parameter + body walks.)
func (w *rubyWalker) walkScopeForCalls(node *sitter.Node, ownerIdx int, seen, locals map[string]bool) {
	w.pendingStrPrefixes = nil
	w.sawDispatcher = false
	w.walkForCalls(node, ownerIdx, seen, locals)
	w.commitPendingStrPrefixes()
}

// commitPendingStrPrefixes promotes the tentative interpolated-string dispatch
// prefixes gathered in the current scope into the committed set — but only if the
// scope also invoked a dispatcher (send/public_send/…). Then it clears the per-scope
// state. This keeps `"present_#{idx}"` (in a method that calls send) while dropping
// cache/Redis key strings like `"fetch_#{id}"` in non-dispatching methods.
func (w *rubyWalker) commitPendingStrPrefixes() {
	if w.sawDispatcher {
		for p := range w.pendingStrPrefixes {
			if w.dynamicPrefixes == nil {
				w.dynamicPrefixes = map[string]bool{}
			}
			w.dynamicPrefixes[p] = true
		}
	}
	w.pendingStrPrefixes = nil
	w.sawDispatcher = false
}

// walkForCalls recursively scans a method body for call expressions and appends
// RelCalls edges to the owner fact. It does not descend into nested
// method/class/module definitions — those receive their own owner.
//
// Four reference shapes are captured: (1) qualified calls via callTarget
// ("Const.method", "var.method"); (2) bare calls with a method name but no
// receiver ("render :x", "helper(arg)") → the bare method name; (3) lone
// identifiers in expression position ("current_user") that are not known locals
// → the bare name; (4) bare constant references ("MyJob", "Chat::Message") used
// as values → the constant name. (2) and (3) are why Ruby methods invoked without
// a receiver (the common Rails case) are recorded as referenced; (4) is why a
// class/module used only as a value (registered, passed as an argument, matched in
// case/when) is. Bare targets — from (2), (3) and (4) — carry no ".", so
// constFromCall ignores them and the package-metrics coupling graph (which keys
// off "Recv.method" constant receivers) is unaffected.
func (w *rubyWalker) walkForCalls(node *sitter.Node, ownerIdx int, seen, locals map[string]bool) {
	if node == nil {
		return
	}
	// Skip anonymous tokens (keywords/operators/punctuation). They are childless
	// leaves, and critically the keyword token for a statement shares its Kind
	// (e.g. the `while`/`if` keyword reports Kind "while"/"if"), which would
	// otherwise double-count loops and decisions below.
	if !node.IsNamed() {
		return
	}

	// Complexity metrics: count decision points so the single body walk doubles
	// as the cyclomatic pass. `case` itself is not counted (each `when` branch is);
	// loop constructs are counted in their own handling below.
	if w.metrics != nil {
		switch kindOf(node) {
		case "if", "elsif", "unless", "if_modifier", "unless_modifier",
			"when", "rescue", "conditional":
			w.metrics.decisions++
		}
	}

	switch kindOf(node) {
	case "method", "singleton_method", "class", "module", "singleton_class":
		return
	case "super":
		// `super` invokes the same-named method in an ancestor (superclass or mixin),
		// so it references that base method. Only meaningful inside a method body
		// (metrics != nil), where selfShort is the enclosing method's bare name.
		// Record the call edge (dead-code marks the ancestor method used) but NOT the
		// complexity metrics: `super` climbs the inheritance chain and terminates —
		// it is not self-recursion, and treating it as such was the dominant recursion
		// false positive (every override with a `super` call). Recurse afterwards to
		// capture any calls in `super(args)`.
		if w.metrics != nil && w.selfShort != "" {
			w.addCall(ownerIdx, seen, w.selfShort)
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i), ownerIdx, seen, locals)
		}
		return
	case "while", "until", "for", "while_modifier", "until_modifier":
		// Syntactic loops: everything in the body runs per iteration.
		if w.metrics != nil {
			w.metrics.loopCount++
			w.metrics.decisions++
			if w.loopDepth+1 > w.metrics.loopDepth {
				w.metrics.loopDepth = w.loopDepth + 1
			}
		}
		w.loopDepth++
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i), ownerIdx, seen, locals)
		}
		w.loopDepth--
		return
	case "call":
		method := node.ChildByFieldName("method")
		recv := node.ChildByFieldName("receiver")
		if method != nil && rubyPreloaders[rubyText(method, w.src)] && w.metrics != nil {
			for _, association := range symbolNamesIn(node.ChildByFieldName("arguments"), w.src) {
				w.metrics.recordPreload(association)
			}
		}
		if method != nil && recv != nil && w.metrics != nil && rubyText(method, w.src) == "for" && rubyText(recv, w.src) == "BatchLoader" {
			w.metrics.batchLoader = true
		}
		// Dynamic dispatch by LITERAL name — `obj.try(:foo)`, `send(:bar)`,
		// `respond_to?(:baz)` — names the target method exactly, so record it as a
		// call. Safe-nav `&.try` still exposes the `method` child. Distinct from an
		// interpolated symbol (`:"report_#{x}"`), which is only a prefix hint.
		if method != nil && rubyDispatchers[rubyText(method, w.src)] {
			w.sawDispatcher = true // gates tentative interpolated-string prefixes
			if nm := dispatchSymbolArg(node.ChildByFieldName("arguments"), w.src); nm != "" {
				w.addCall(ownerIdx, seen, nm)
				// `obj.try(:foo)` dispatches to a DIFFERENT object; only a
				// receiverless/self dispatch (`send(:foo)`, `self.try(:foo)`) can recurse.
				w.recordSelfAwareMetrics(nm, recv)
			}
		}
		if method != nil && recv != nil && rubyText(method, w.src) == "new" {
			switch kindOf(recv) {
			case "constant", "scope_resolution":
				w.addInstantiation(ownerIdx, stripLeadingColons(rubyText(recv, w.src)), oneShotCallOn(node, w.src))
			}
		}
		if target := w.callTarget(node); target != "" {
			w.addCall(ownerIdx, seen, target)
			// Recursion only for a same-object self dispatch. callTarget returns a bare
			// method name for both plain `self.foo` (recv kind "self" — genuine
			// recursion) and `self.class.foo` (recv kind "call" — the instance method
			// calling its sibling CLASS method, NOT recursion), so the target string
			// alone can't tell them apart; gate on the receiver. An explicit
			// `Const.foo` that resolves to this exact method (selfName) is real
			// class-method self-recursion and is preserved.
			if target == w.selfName {
				w.recordCallMetrics(target)
			} else {
				w.recordSelfAwareMetrics(target, recv)
			}
		} else if recv == nil && method != nil && kindOf(method) == "identifier" {
			if name := rubyText(method, w.src); !rubyNonCalls[name] {
				w.addCall(ownerIdx, seen, name)
				w.recordCallMetrics(name)
			}
		} else if recv != nil && method != nil && kindOf(method) == "identifier" {
			// A no-arg call on a receiver that callTarget suppressed. Bare target
			// (no ".") -> no coupling impact. Skip keywords and common
			// attribute/enumerable reads so a dead method sharing a name with
			// `.name`/`.count`/`.first` isn't hidden.
			name := rubyText(method, w.src)
			switch {
			case rubyNonCalls[name] || rubyCheapMethods[name]:
				// keyword / cheap attribute-or-enumerable read — ignore
			case kindOf(recv) == "call" || strings.HasSuffix(name, "?") || strings.HasSuffix(name, "!") ||
				(isVarReceiver(kindOf(recv)) && strings.Contains(name, "_")) ||
				isClassHoldingReceiver(recv, w.src):
				// Chained receiver (ActiveRecord scope / class-method chains
				// `Model.scope1.scope2.final`, `assoc.class_method`, `x.class.method`),
				// a predicate/bang call on ANY receiver (`viewer.rich?`, `x.save!`), OR a
				// call on a variable receiver — a local, a bare method, or an
				// instance/class/global variable (`@klass.bo_search_fields`) — whose name
				// is scope/class-method-like (has `_`) — e.g.
				// `items.preload_relations`, `some_relation.pluck_job_id`. All are
				// unambiguously method calls (an attribute read never ends in `?`/`!`,
				// and a snake_case multi-word name is a scope/class-method, not a plain
				// attribute). Single-word reads (`user.email`, `x.name`) stay out.
				//
				// Record the call edge and (if in a loop) the in-loop N+1 signal, but
				// NOT recursion: this branch always has an explicit, non-self receiver
				// (self-receiver calls resolve via callTarget above), so a call whose
				// name matches the enclosing method is a same-named call on a DIFFERENT
				// object — the SimpleDelegator/decorator pattern (`@delegate.render`,
				// `new.call`), not self-recursion.
				w.addCall(ownerIdx, seen, name)
				w.recordInLoopCall(name)
			case w.loopDepth > 0:
				// A no-arg single-level read inside a loop (the association read
				// `u.posts` or `record.reload`). It is not a graph edge, but its method
				// name feeds the perf metric so the performance analyzer can flag
				// lazy-loaded association / per-iteration I/O (N+1).
				//
				// The RECEIVER is kept when it is a plain variable, and that is the
				// whole difference between a name and an N+1. `form_questions.each { |q|
				// q.form_answers }` is a query per iteration only because `q` is a
				// FormQuestion; recording it as bare `form_answers` throws away the one
				// thing that makes it decidable, and a consumer joining block bindings
				// to association facts found exactly zero because of it. With the
				// receiver, `q.form_answers` joins to `q=form_questions` and resolves.
				if isVarReceiver(recv.Kind()) {
					w.addNames(ownerIdx, seen, name)
					w.recordInLoopCall(rubyText(recv, w.src) + "." + name)
					break
				}
				w.recordInLoopCall(name)
			case isVarReceiver(kindOf(recv)):
				// A single-word read on a variable receiver (`parser.usernames`,
				// `user.email`) is kept out of the call graph on purpose: most are
				// attribute reads and the coupling metrics would drown in them. It
				// is still a reference to whatever method carries that name, so it
				// is recorded by name, which only the dead-code reading consumes.
				w.addNames(ownerIdx, seen, name)
			}
		}
		// An iterator method with a block (users.each { … }, n.times { … }) is a
		// loop: its block body runs per element, but the receiver and arguments
		// are evaluated once — so only the block child walks at +1 depth (mirrors
		// the Python comprehension handling).
		block := node.ChildByFieldName("block")
		isIter := block != nil && method != nil && rubyIterators[rubyText(method, w.src)]
		if isIter && recv != nil && w.metrics != nil {
			// Only a named receiver is worth binding: `[1,2].each` says nothing
			// about the element's type, while `form_questions.each` names the
			// collection whose target the consumer can resolve, and
			// `Company.find_each` names the class the consumer can type from.
			if isVarReceiver(recv.Kind()) || recv.Kind() == "call" || recv.Kind() == "constant" || recv.Kind() == "scope_resolution" {
				if param := blockParamName(block, w.src); param != "" {
					collection := lastSegment(rubyText(recv, w.src))
					if chain := receiverChain(recv, w.src); len(chain) > 0 {
						collection = strings.Join(w.metrics.spliceChain(chain), ".")
					}
					w.metrics.recordBlockBinding(param, collection)
				}
			}
		}
		// A constant-bounded iterator (`6.times`, `[…].each`, `STOP_CHARS.any?`) runs a
		// fixed number of times regardless of the method's input, so it still counts as
		// a loop (cyclomatic) but must not add scaling loop DEPTH — otherwise a literal
		// or constant inner/outer loop inflates a genuine O(n) into a false O(n²)/O(n³).
		bounded := isIter && constantBoundReceiver(recv, w.src)
		if isIter && w.metrics != nil {
			w.metrics.loopCount++
			w.metrics.decisions++
			if !bounded && w.loopDepth+1 > w.metrics.loopDepth {
				w.metrics.loopDepth = w.loopDepth + 1
			}
		}
		// Recurse into every child EXCEPT the callee `method` child, which has
		// already been consumed above (otherwise it would be re-counted by the
		// bare-identifier case below).
		for i := uint(0); i < node.ChildCount(); i++ {
			c := node.Child(i)
			if method != nil && c.StartByte() == method.StartByte() && c.EndByte() == method.EndByte() {
				continue
			}
			if isIter && c.StartByte() == block.StartByte() && c.EndByte() == block.EndByte() {
				if bounded {
					// Fixed iteration count: walk the block at the SAME depth so any
					// inner scaling loop or per-iteration I/O is measured against the
					// real input, not multiplied by a constant.
					w.walkForCalls(c, ownerIdx, seen, locals)
					continue
				}
				w.loopDepth++
				w.walkForCalls(c, ownerIdx, seen, locals)
				w.loopDepth--
				continue
			}
			w.walkForCalls(c, ownerIdx, seen, locals)
		}
		return
	case "assignment", "operator_assignment":
		// `@client = …` is a write; `@count += 1` is both. Handled here rather
		// than in the declaration pass because that pass runs with metrics off,
		// and handled before the generic descent because descending would walk
		// the target as if it were a read — which is how the first version
		// reported every write as a read.
		// `x = Meeting.find(id)` types x as a Meeting for the rest of the body.
		if left := node.ChildByFieldName("left"); left != nil &&
			(left.Kind() == "identifier" || left.Kind() == "instance_variable") {
			if right := node.ChildByFieldName("right"); right != nil && right.Kind() == "call" {
				recv := right.ChildByFieldName("receiver")
				meth := right.ChildByFieldName("method")
				if recv != nil && meth != nil && recv.Kind() == "constant" &&
					typingMethods[rubyText(meth, w.src)] {
					w.metrics.recordLocalType(rubyText(left, w.src), rubyText(recv, w.src))
					if rubyText(meth, w.src) == "new" {
						w.metrics.recordUnpersistedLocal(rubyText(left, w.src))
					}
				}
			}
			if right := node.ChildByFieldName("right"); right != nil && w.metrics != nil {
				w.metrics.recordLocalChain(rubyText(left, w.src), receiverChain(right, w.src))
			}
		}
		if left := node.ChildByFieldName("left"); left != nil && left.Kind() == "instance_variable" {
			name := rubyText(left, w.src)
			w.metrics.recordFieldAccess(name, true)
			if node.Kind() == "operator_assignment" {
				// `||=` and `+=` read the current value before storing.
				w.metrics.recordFieldAccess(name, false)
			}
			if right := node.ChildByFieldName("right"); right != nil {
				w.walkForCalls(right, ownerIdx, seen, locals)
			}
			return
		}
	case "instance_variable":
		// A read of `@client`.
		w.metrics.recordFieldAccess(rubyText(node, w.src), false)
		return
	case "identifier":
		// A bare identifier outside callee position: either an arg-less method
		// call or a local variable read. Emit unless it is a known local or a
		// keyword/builtin; matching is conservative so over-emitting is safe.
		if name := rubyText(node, w.src); name != "" && !locals[name] && !rubyNonCalls[name] {
			w.addCall(ownerIdx, seen, name)
			w.recordCallMetrics(name)
		}
		return
	case "constant", "scope_resolution":
		// A bare constant reference in expression position — an argument
		// (register(MyJob)), array/hash element, case/when or rescue clause,
		// assignment RHS, or a lone `Foo` value. It is NOT a `Const.method` call
		// (that is captured as the receiver via callTarget above) and NOT a
		// definition name (handleClass/handleModule consume those), so without this
		// a class/module used only as a value looks unreferenced and is mis-reported
		// as dead. Record it as a use of that constant. The target carries no ".",
		// so constFromCall ignores it and the package-metrics coupling graph is
		// unaffected; it is not a method invocation, so perf metrics are untouched.
		// scope_resolution is recorded whole (e.g. "Chat::Message") and not
		// descended into, so the qualified path is matched rather than its segments.
		if name := stripLeadingColons(rubyText(node, w.src)); name != "" && !rubyBuiltinConsts[name] {
			w.addCall(ownerIdx, seen, name)
		}
		return
	case "argument_list":
		// A symbol literal handed to a call as a direct argument
		// (`perform_async(id, :requisitions_done)`, `retry_with(:fallback)`)
		// usually names a method for something else to dispatch. Recorded as a
		// reference by name, not a call; hash pairs (`order(created_at: :desc)`)
		// are not direct children and are not recorded.
		for i := uint(0); i < node.NamedChildCount(); i++ {
			c := node.NamedChild(i)
			if c == nil {
				continue
			}
			switch kindOf(c) {
			case "simple_symbol":
				w.addNames(ownerIdx, seen, strings.TrimPrefix(rubyText(c, w.src), ":"))
			case "pair":
				// `mailer_method: :notify_new_connect`, `with: :render_not_found`:
				// a symbol handed in as a keyword value names a method as often as a
				// positional one does. The key is not a name.
				if v := c.ChildByFieldName("value"); v != nil && kindOf(v) == "simple_symbol" {
					w.addNames(ownerIdx, seen, strings.TrimPrefix(rubyText(v, w.src), ":"))
				}
			case "string":
				// A string shaped like `Class#method` or `Class.method` names a
				// method the same way (`batch.on(:success, "Destroy#users_done")`,
				// Sidekiq batch callbacks, `method_object("Thing#run")`).
				if name := methodNamedByString(rubyText(c, w.src)); name != "" {
					w.addNames(ownerIdx, seen, name)
				}
			}
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i), ownerIdx, seen, locals)
		}
		return
	case "symbol_array":
		// `%i[name email]`: each bare symbol is a name, exactly like the bracketed form below.
		for i := uint(0); i < node.NamedChildCount(); i++ {
			if c := node.NamedChild(i); c != nil && kindOf(c) == "bare_symbol" {
				w.addNames(ownerIdx, seen, rubyText(c, w.src))
			}
		}
		return
	case "array":
		// A symbol array (`ATTRS = %i[name email]`, `[:a, :b].each { |m| send(m) }`)
		// is the usual source of names fed to send later. Each symbol is a
		// reference by name, never a call.
		for i := uint(0); i < node.NamedChildCount(); i++ {
			if c := node.NamedChild(i); c != nil && kindOf(c) == "simple_symbol" {
				w.addNames(ownerIdx, seen, strings.TrimPrefix(rubyText(c, w.src), ":"))
			}
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i), ownerIdx, seen, locals)
		}
		return
	case "block_argument":
		// `each(&:destroy_with_publication!)` sends the symbol's method to every
		// element. It is a call by any reading, and without this edge a method
		// used only through symbol-to-proc looked unreferenced.
		for i := uint(0); i < node.NamedChildCount(); i++ {
			if c := node.NamedChild(i); c != nil && kindOf(c) == "simple_symbol" {
				w.addCall(ownerIdx, seen, strings.TrimPrefix(rubyText(c, w.src), ":"))
			}
		}
		return
	case "delimited_symbol":
		// An interpolated symbol `:"report_#{type}"` — a method name computed for
		// dynamic dispatch (public_send/send). Record its static prefix so the
		// dead-code detector treats same-prefix methods as used. Symbols are captured
		// unconditionally (they are almost always method names). Fall through to
		// recurse: the interpolation may itself contain real calls.
		if p := dynamicSymbolPrefix(node, w.src); p != "" {
			if w.dynamicPrefixes == nil {
				w.dynamicPrefixes = map[string]bool{}
			}
			w.dynamicPrefixes[p] = true
		}
	case "string":
		// An interpolated string `"present_#{idx}"` may be a computed dispatch name
		// too, but snake_case strings are commonly cache/Redis keys — so record its
		// prefix only TENTATIVELY, committed after the scope walk iff the scope also
		// invokes a dispatcher (see commitPendingStrPrefixes). Recurse for nested calls.
		if p := dynamicSymbolPrefix(node, w.src); p != "" {
			if w.pendingStrPrefixes == nil {
				w.pendingStrPrefixes = map[string]bool{}
			}
			w.pendingStrPrefixes[p] = true
		}
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkForCalls(node.Child(i), ownerIdx, seen, locals)
	}
}

// dynamicSymbolPrefix returns the static literal prefix of an interpolated symbol
// node (`:"report_#{type}"` -> "report_"), or "" when the node is not an
// interpolated symbol or the prefix is not specific enough to be a useful dispatch
// hint. The prefix is the string_content preceding the FIRST interpolation; it
// qualifies only when at least one interpolation is present and the prefix is >= 4
// chars ending in "_" (a word boundary), so generic 1-2 char stems don't over-match.
func dynamicSymbolPrefix(node *sitter.Node, src []byte) string {
	var prefix strings.Builder
	sawInterp := false
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "interpolation":
			sawInterp = true
			i = node.ChildCount() // stop at the first interpolation
		case "string_content":
			prefix.WriteString(rubyText(c, src))
		}
	}
	if !sawInterp {
		return ""
	}
	p := prefix.String()
	if len(p) >= 4 && strings.HasSuffix(p, "_") {
		return p
	}
	return ""
}

// rubyDispatchers are methods that invoke (or reference) another method named by
// their first argument: `obj.try(:foo)`, `send(:bar)`, `respond_to?(:baz)`,
// `method(:qux)`. When that argument is a LITERAL symbol/string the target method
// is statically known, so it is recorded as a call.
var rubyDispatchers = map[string]bool{
	"send": true, "public_send": true, "__send__": true,
	"try": true, "try!": true, "respond_to?": true,
	"method": true, "public_method": true,
}

// dispatchSymbolArg returns the method name named by the first argument of a
// dispatcher call (`:foo` -> "foo", "foo" -> "foo"), or "" when the first argument
// is not a literal symbol / static string (e.g. a variable or interpolated value).
func dispatchSymbolArg(args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		c := args.Child(i)
		if !c.IsNamed() {
			continue
		}
		switch kindOf(c) {
		case "simple_symbol":
			return strings.TrimPrefix(rubyText(c, src), ":")
		case "string":
			// Static string only (no interpolation): the literal is the method name.
			for j := uint(0); j < c.ChildCount(); j++ {
				if kindOf(c.Child(j)) == "interpolation" {
					return ""
				}
			}
			return stringLiteralContent(c, src)
		}
		return "" // first positional arg is something else — not a literal name
	}
	return ""
}

// stringLiteralContent returns the concatenated string_content of a string node.
func stringLiteralContent(node *sitter.Node, src []byte) string {
	var b strings.Builder
	for i := uint(0); i < node.ChildCount(); i++ {
		if kindOf(node.Child(i)) == "string_content" {
			b.WriteString(rubyText(node.Child(i), src))
		}
	}
	return b.String()
}

// extractTestRefsAST parses a test/spec file and returns a single
// facts.KindTestRef fact whose RelCalls relations name the production symbols the
// test references. It reuses the production call-target conventions (callTarget +
// bare receiver-less calls + bare constants) but descends through every scope —
// a test file has no meaningful symbol surface of its own, and its references
// live inside describe/context/it blocks and example methods. Local-variable
// tracking is deliberately omitted (unlike walkForCalls): over-emitting a
// reference can only ever keep a production symbol alive, never falsely flag one
// dead, matching the orphan detector's conservative bias. Returns nil when the
// file references nothing.
func extractTestRefsAST(src []byte, relFile string) []facts.Fact {
	return refsFromRuby(src, relFile, facts.KindTestRef)
}

// walkTestRefs recurses through ALL named nodes of a test file, emitting a
// RelCalls target for each qualified call, bare receiver-less call, and bare
// constant — the same conventions as walkForCalls, minus the loop/complexity
// metrics and the class/module/method early-returns (a spec's references live
// inside those bodies).
func (w *rubyWalker) walkTestRefs(node *sitter.Node, add func(string)) {
	if node == nil || !node.IsNamed() {
		return
	}
	switch kindOf(node) {
	case "call":
		method := node.ChildByFieldName("method")
		recv := node.ChildByFieldName("receiver")
		if target := w.callTarget(node); target != "" {
			add(target)
		} else if recv == nil && method != nil && kindOf(method) == "identifier" {
			if name := rubyText(method, w.src); !rubyNonCalls[name] {
				add(name)
			}
		}
	case "identifier":
		// A bare identifier: an arg-less method call or a local read. Emit unless a
		// keyword/builtin; matching is conservative so over-emitting is safe.
		if name := rubyText(node, w.src); name != "" && !rubyNonCalls[name] {
			add(name)
		}
	case "constant", "scope_resolution":
		if name := stripLeadingColons(rubyText(node, w.src)); name != "" && !rubyBuiltinConsts[name] {
			add(name)
		}
		return // recorded whole; do not descend into scope_resolution segments
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkTestRefs(node.Child(i), add)
	}
}

// addInstantiation records that the owner constructs the named class. `new` on
// a literal constant is the one call whose result has a knowable type, and the
// relation is what a question about who builds a class reads. When the
// construction is immediately the receiver of another call the ceremony
// `Foo.new(...).bar` is named under OneShotCallProp, which is what a rule
// about one-shot objects selects on; the provider records the same two things
// with better receiver resolution.
func (w *rubyWalker) addInstantiation(ownerIdx int, class, oneShot string) {
	if class == "" || rubyBuiltinConsts[class] {
		return
	}
	for _, r := range w.out[ownerIdx].Relations {
		if r.Kind == facts.RelInstantiates && r.Target == class {
			return
		}
	}
	w.out[ownerIdx].Relations = append(w.out[ownerIdx].Relations,
		facts.Relation{Kind: facts.RelInstantiates, Target: class})
	if oneShot == "" {
		return
	}
	if w.out[ownerIdx].Props == nil {
		w.out[ownerIdx].Props = map[string]any{}
	}
	existing, _ := w.out[ownerIdx].PropAny(OneShotCallProp).(string)
	entry := class + "." + oneShot
	for _, seen := range strings.Fields(existing) {
		if seen == entry {
			return
		}
	}
	if existing == "" {
		w.out[ownerIdx].SetProp(OneShotCallProp, entry)
	} else {
		w.out[ownerIdx].SetProp(OneShotCallProp, existing+" "+entry)
	}
}

// OneShotCallProp lists, space separated, each `Class.method` a member built
// and called in one expression.
const OneShotCallProp = "one_shot_call"

// oneShotCallOn returns the method a `new` call's result is immediately sent,
// or "" when the constructed object is bound to something first.
func oneShotCallOn(newCall *sitter.Node, src []byte) string {
	parent := newCall.Parent()
	if parent == nil || kindOf(parent) != "call" {
		return ""
	}
	receiver := parent.ChildByFieldName("receiver")
	if receiver == nil || receiver.StartByte() != newCall.StartByte() || receiver.EndByte() != newCall.EndByte() {
		return ""
	}
	method := parent.ChildByFieldName("method")
	if method == nil {
		return ""
	}
	return rubyText(method, src)
}

// addCall appends a deduplicated RelCalls edge to the owner fact.
func (w *rubyWalker) addCall(ownerIdx int, seen map[string]bool, target string) {
	if target == "" || seen[target] {
		return
	}
	seen[target] = true
	w.out[ownerIdx].Relations = append(w.out[ownerIdx].Relations,
		facts.Relation{Kind: facts.RelCalls, Target: target})
}

// methodNamedByString returns the method a string literal of the shape
// "Class#method" or "Class.method" names, or "" for any other string.
func methodNamedByString(literal string) string {
	text := strings.Trim(literal, "\"'")
	if text == "" || text[0] < 'A' || text[0] > 'Z' || strings.ContainsAny(text, " \t\n/") {
		return ""
	}
	sep := strings.LastIndexAny(text, "#.")
	if sep <= 0 || sep == len(text)-1 {
		return ""
	}
	name := text[sep+1:]
	for _, r := range name {
		if !isNameRune(r) {
			return ""
		}
	}
	if name[0] < 'a' || name[0] > 'z' {
		return ""
	}
	return name
}

// addNames records that the owner names target by symbol literal, as an
// argument to a call that is not itself a DSL the extractor reads: the method
// name is handed to something else to dispatch later. A reference, never a
// call, so call metrics are untouched.
func (w *rubyWalker) addNames(ownerIdx int, seen map[string]bool, target string) {
	key := "names:" + target
	if target == "" || seen[key] {
		return
	}
	seen[key] = true
	w.out[ownerIdx].Relations = append(w.out[ownerIdx].Relations,
		facts.Relation{Kind: facts.RelNames, Target: target})
}

// addCallToFact appends a RelCalls edge to an arbitrary fact (used for DSL
// references attached to the enclosing class), skipping exact duplicates.
func (w *rubyWalker) addCallToFact(idx int, target string) {
	if target == "" || idx < 0 || idx >= len(w.out) {
		return
	}
	for _, r := range w.out[idx].Relations {
		if r.Kind == facts.RelCalls && r.Target == target {
			return
		}
	}
	w.out[idx].Relations = append(w.out[idx].Relations,
		facts.Relation{Kind: facts.RelCalls, Target: target})
}

// rubyCallbackDSL are Rails class-body methods that take method-name symbol
// arguments (controller filters, ActiveRecord lifecycle callbacks, custom
// validations). `validates` is intentionally excluded — its first symbol is an
// attribute name, not a method.
var rubyCallbackDSL = map[string]bool{
	"before_action": true, "after_action": true, "around_action": true,
	"append_before_action": true, "prepend_before_action": true,
	"skip_before_action": true, "skip_after_action": true, "skip_around_action": true,
	"before_save": true, "after_save": true, "around_save": true,
	"before_create": true, "after_create": true, "around_create": true,
	"before_update": true, "after_update": true, "around_update": true,
	"before_destroy": true, "after_destroy": true, "around_destroy": true,
	"before_validation": true, "after_validation": true,
	"after_commit": true, "after_rollback": true, "after_initialize": true,
	"after_create_commit": true, "after_update_commit": true, "after_destroy_commit": true,
	"after_save_commit": true, "before_commit": true,
	"after_find": true, "after_touch": true,
	"validate": true,
}

// rubyMethodNamingDSL are class-body methods that name another method by
// symbol, directly or through an `if:`/`unless:`/`with:` option.
var rubyMethodNamingDSL = map[string]bool{
	"field": true, "helper_method": true, "rescue_from": true, "validates": true, "alias_method": true,
}

// symbolOptionArgs returns the simple_symbol values of the named keyword
// options in an argument list (`if: :admin?`, `with: :render_not_found`).
func symbolOptionArgs(args *sitter.Node, src []byte, keys ...string) []string {
	var out []string
	if args == nil {
		return out
	}
	wanted := map[string]bool{}
	for _, k := range keys {
		wanted[k] = true
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if kindOf(n) == "pair" {
			key := n.ChildByFieldName("key")
			value := n.ChildByFieldName("value")
			if key != nil && value != nil && kindOf(value) == "simple_symbol" && wanted[strings.TrimSuffix(rubyText(key, src), ":")] {
				out = append(out, strings.TrimPrefix(rubyText(value, src), ":"))
			}
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(args)
	return out
}

// rubySerializerDSL are ActiveModel::Serializer class-body methods whose symbol
// arguments name attributes/associations. Each declared name is backed by an
// optional same-named method (and an `include_<name>?` predicate) that the
// serializer framework invokes — never an explicit Ruby call — so they are folded
// in as references (see handleBodyCall). Applied only inside a serializer class.
var rubySerializerDSL = map[string]bool{
	"attributes": true, "attribute": true,
	"has_one": true, "has_many": true, "belongs_to": true, "has_and_belongs_to_many": true,
}

// rubyBuiltinConsts are Ruby core and common stdlib constants. A bare reference to
// one is real, but emitting a call edge to it inflates fan-in on monkey-patch
// reopenings (Discourse's freedom_patches `class Array`/`String`/`Time`), turning
// uninteresting core classes into spurious god-class / hotspot findings while
// never being a useful dead-code lead. They are skipped when recording bare
// constant references. Namespaced constants (Foo::Array) are unaffected.
var rubyBuiltinConsts = map[string]bool{
	"Object": true, "BasicObject": true, "Module": true, "Class": true, "Method": true,
	"UnboundMethod": true, "Proc": true, "Binding": true, "Data": true,
	"Array": true, "Hash": true, "String": true, "Symbol": true, "Set": true,
	"Integer": true, "Float": true, "Numeric": true, "Rational": true, "Complex": true,
	"Range": true, "Regexp": true, "MatchData": true, "Struct": true, "Enumerator": true,
	"TrueClass": true, "FalseClass": true, "NilClass": true,
	"Time": true, "Date": true, "DateTime": true,
	"Comparable": true, "Enumerable": true, "Kernel": true, "Math": true,
	"IO": true, "File": true, "Dir": true, "FileUtils": true, "Pathname": true,
	"StringIO": true, "Tempfile": true,
	"Thread": true, "Mutex": true, "ConditionVariable": true, "Queue": true,
	"SizedQueue": true, "Fiber": true, "ThreadGroup": true,
	"Exception": true, "StandardError": true, "RuntimeError": true, "ArgumentError": true,
	"TypeError": true, "NameError": true, "NoMethodError": true, "IndexError": true,
	"KeyError": true, "RangeError": true, "IOError": true, "NotImplementedError": true,
	"StopIteration": true, "ZeroDivisionError": true, "FrozenError": true,
	"Marshal": true, "ObjectSpace": true, "GC": true, "Process": true, "Signal": true,
	"Encoding": true, "Random": true, "SecureRandom": true, "Mutex_m": true,
	// Ubiquitous Rails/framework top-level constants. Bare references to these
	// appear in nearly every file (I18n.t, Rails.env, Logger.new, ...); recording a
	// call edge to them only inflates fan-in on a reopened module fact (e.g. I18n at
	// 273 dependents), producing spurious god-class/hotspot findings while never
	// being a useful dead-code lead. Qualified names (ActiveRecord::Base) are
	// matched separately and unaffected; app base classes (ApplicationRecord) are
	// deliberately NOT listed — they are legitimately central.
	"Rails": true, "I18n": true, "Logger": true, "GlobalID": true, "Mime": true,
}

// rubyNonCalls are bare identifiers that must not be treated as method-call
// references: keywords/builtins that commonly appear in expression position.
// (Most Ruby keywords — self, nil, super, yield, return — are their own AST node
// kinds and never reach the identifier case, but this guards the rest.)
var rubyNonCalls = map[string]bool{
	"self": true, "nil": true, "true": true, "false": true, "super": true,
	"yield": true, "return": true, "next": true, "break": true, "redo": true,
	"retry": true, "raise": true, "throw": true, "loop": true, "proc": true,
	"lambda": true, "puts": true, "print": true, "p": true, "require": true,
	"require_relative": true, "load": true, "freeze": true, "block_given?": true,
	"__method__": true, "private": true, "protected": true, "public": true,
	"module_function": true,
}

// collectLocals gathers the parameter names and locally-assigned variable names
// of a method so walkForCalls can tell a local variable read apart from an
// arg-less method call.
func collectLocals(method *sitter.Node, src []byte) map[string]bool {
	locals := map[string]bool{}
	if method == nil {
		return locals
	}
	if params := method.ChildByFieldName("parameters"); params != nil {
		collectIdentifiers(params, src, locals)
	}
	body := method.ChildByFieldName("body")
	collectAssignTargets(body, src, locals)
	// Block parameters (`things.each do |user| … end`, `{ |k, v| … }`) are locals
	// too — and, being the most common identifiers inside loops, are the main
	// source of false N+1 findings when their name coincides with an ActiveRecord
	// association (`user`, `comment`, …). collectLocals is method-wide, so a
	// block var here shadows a same-named bare method call elsewhere in the method;
	// that over-approximation is safe (it only suppresses over-emission) and matches
	// how assignment targets are already collected.
	collectBlockParams(body, src, locals)
	return locals
}

// collectBlockParams adds every block-parameter identifier in a subtree to out.
// The parameters field of a block/do_block is a block_parameters node whose
// identifiers (including those nested in destructured/splat/keyword params) name
// the block's locals; collectIdentifiers gathers them all.
func collectBlockParams(node *sitter.Node, src []byte, out map[string]bool) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "block", "do_block":
		if params := node.ChildByFieldName("parameters"); params != nil {
			collectIdentifiers(params, src, out)
		}
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		collectBlockParams(node.Child(i), src, out)
	}
}

// collectIdentifiers adds every identifier name in a subtree to out.
func collectIdentifiers(node *sitter.Node, src []byte, out map[string]bool) {
	if node == nil {
		return
	}
	if kindOf(node) == "identifier" {
		out[rubyText(node, src)] = true
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		collectIdentifiers(node.Child(i), src, out)
	}
}

// collectAssignTargets adds the left-hand identifier(s) of every assignment in a
// subtree to out (plain `x = …` and multiple-assignment `a, b = …`; setter
// calls like `self.x = …` are skipped).
func collectAssignTargets(node *sitter.Node, src []byte, out map[string]bool) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "assignment", "operator_assignment":
		switch left := node.ChildByFieldName("left"); {
		case left == nil:
		case kindOf(left) == "identifier":
			out[rubyText(left, src)] = true
		case kindOf(left) == "left_assignment_list":
			collectIdentifiers(left, src, out)
		}
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		collectAssignTargets(node.Child(i), src, out)
	}
}

// callTarget resolves a call node to a RelCalls target string, preserving the
// two-tier convention of the former regex scanner:
//   - constant / scope-resolution receiver  -> "Const.method" / "Ns::Class.method"
//     (always emitted, parentheses not required)
//   - lowercase variable receiver with args -> "var.method"
//     (only when arguments are present, to skip attribute reads)
//
// Bare calls without a receiver are not emitted (matching the regex behavior).
func (w *rubyWalker) callTarget(node *sitter.Node) string {
	recv := node.ChildByFieldName("receiver")
	method := node.ChildByFieldName("method")
	if recv == nil || method == nil {
		return ""
	}
	methodName := rubyText(method, w.src)
	if methodName == "" {
		return ""
	}

	switch kindOf(recv) {
	case "constant", "scope_resolution":
		return rubyText(recv, w.src) + "." + methodName
	case "self":
		// `self.foo` / `self.save!` — same-object dispatch. Emit the bare method
		// name so the method is recorded as used (dead-code precision). Bare target
		// (no "."), short-name matched, ignored by the coupling graph. `self.class`
		// as a receiver-only expression emits nothing here; the OUTER call (handled
		// in `case "call":` below) emits the real method.
		if methodName == "class" {
			return ""
		}
		return methodName
	case "identifier":
		if node.ChildByFieldName("arguments") != nil {
			return rubyText(recv, w.src) + "." + methodName
		}
	case "call":
		// `self.class.perform_when_readonly?`: the inner call is `self.class`
		// (receiver kind "self", method "class"). Emit the OUTER method name, bare.
		// No args-gate: the receiver is provably the class, so the name is provably
		// a method (unlike a lowercase-variable attribute read).
		if innerRecv := recv.ChildByFieldName("receiver"); innerRecv != nil && kindOf(innerRecv) == "self" {
			if innerMethod := recv.ChildByFieldName("method"); innerMethod != nil && rubyText(innerMethod, w.src) == "class" {
				return methodName
			}
		}
		// Chained call, e.g. Rails.logger.info(x): use the inner call's method
		// name as a pseudo-receiver when it is a lowercase identifier.
		inner := recv.ChildByFieldName("method")
		if inner != nil && kindOf(inner) == "identifier" && node.ChildByFieldName("arguments") != nil {
			return rubyText(inner, w.src) + "." + methodName
		}
	}
	return ""
}

// --- assignments (constants and explicit table names) ---

func (w *rubyWalker) handleAssignment(node *sitter.Node) {
	left := node.ChildByFieldName("left")
	if left == nil {
		return
	}
	// `@client = …` is a write. It is recorded here rather than in the
	// identifier walk because that walk cannot tell an assignment target from a
	// read, and the read/write split is what makes the fact useful: a method
	// that only reads a field is a candidate to move with it, one that writes
	// is not.
	if left.Kind() == "instance_variable" {
		w.metrics.recordFieldAccess(rubyText(left, w.src), true)
	}

	// CONSTANT = ... (all-caps only, matching the former regex).
	if kindOf(left) == "constant" {
		constName := rubyText(left, w.src)
		if !isAllCaps(constName) {
			return
		}
		scope := w.scopeQual()
		var fullName string
		if scope != "" {
			fullName = scope + "::" + constName
		} else {
			fullName = w.dir + "." + constName
		}
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindSymbol,
			Name:      fullName,
			File:      w.relFile,
			Line:      line(node),
			EndLine:   endLine(node),
			Column:    column(node),
			EndColumn: endColumn(node),
			Props: map[string]any{
				"symbol_kind": facts.SymbolConstant,
				"exported":    w.exported(),
				"language":    "ruby",
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
		})
		return
	}

	// self.table_name = "foo" on an ActiveRecord model. The declaration answers
	// the question the model's `table` prop asks, so it CORRECTS that prop rather
	// than becoming a fact of its own: nothing in the graph refers to a table
	// except through the model that uses it, and one declaration producing both a
	// wrong derived claim and its own correction is two facts where there is one
	// thing.
	if s := w.cur(); s != nil && s.isModel && kindOf(left) == "call" {
		if rubyText(left, w.src) == "self.table_name" {
			if tbl := firstStringArg(node.ChildByFieldName("right"), w.src); tbl != "" {
				w.setModelTable(tbl)
			}
		}
	}
}

// --- body-level DSL calls ---

func (w *rubyWalker) handleBodyCall(node *sitter.Node) {
	// Only bare method calls (no receiver) are DSL declarations.
	if node.ChildByFieldName("receiver") != nil {
		return
	}
	method := rubyText(node.ChildByFieldName("method"), w.src)
	args := node.ChildByFieldName("arguments")

	// Note: the macro NAME itself (`requires_login`, `cluster_concurrency`) is
	// recorded as a use of the enclosing class by the walkForCalls pass that
	// walkStatement now runs over every body-level call — so it is not repeated
	// here (that would emit a duplicate RelCalls edge). This function only folds in
	// the DSL's *symbol arguments* (callback/serializer method names), which
	// walkForCalls does not special-case.

	// Rails callback/validation DSL references methods by symbol literal
	// (`before_action :authenticate_user!`, `validate :check`). Record each as a
	// RelCalls edge on the enclosing class/module so callback-only methods are
	// not mis-reported as dead code.
	if rubyCallbackDSL[method] {
		if cur := w.cur(); cur != nil && cur.symFactIdx >= 0 {
			for _, name := range symbolArgs(args, w.src) {
				w.addCallToFact(cur.symFactIdx, name)
			}
			for _, name := range symbolOptionArgs(args, w.src, "if", "unless") {
				w.addCallToFact(cur.symFactIdx, name)
			}
		}
		return
	}

	// Other class-body DSL that names a method by symbol: `field :name` (GraphQL
	// types, Avo resources) is backed by a same-named method, `helper_method
	// :name` exposes one to views, `rescue_from X, with: :name` and `validates
	// ..., if: :name` dispatch to one, `alias_method :new, :old` calls the old.
	// Each is a reference, and without the edge the method it names looked
	// unreferenced.
	if rubyMethodNamingDSL[method] {
		if cur := w.cur(); cur != nil && cur.symFactIdx >= 0 {
			names := symbolOptionArgs(args, w.src, "if", "unless", "with")
			switch method {
			case "field", "helper_method":
				names = append(names, symbolArgs(args, w.src)...)
			case "alias_method":
				if all := symbolArgs(args, w.src); len(all) == 2 {
					names = append(names, all[1])
				}
			}
			for _, name := range names {
				w.addCallToFact(cur.symFactIdx, name)
			}
		}
		if method != "validates" {
			return
		}
	}

	// `delegate :a, :b, ..., to: X` generates methods that call each named method on
	// the target — a real reference. Fold the delegated method names in as calls on
	// the enclosing class so they are not mis-reported as dead. The `to:`/`prefix:`
	// keyword args are `pair` nodes, so symbolArgs (direct simple_symbol children
	// only) skips them.
	if method == "delegate" {
		if cur := w.cur(); cur != nil && cur.symFactIdx >= 0 {
			for _, name := range symbolArgs(args, w.src) {
				w.addCallToFact(cur.symFactIdx, name)
			}
		}
		return
	}

	// ActiveModel::Serializer attribute/association DSL: `attributes :a, :b`,
	// `attribute :c`, `has_one :user`, `has_many :posts`. Each declared name is
	// backed by a same-named method the serializer framework calls (when defined),
	// plus an optional `include_<name>?` predicate it calls to decide inclusion —
	// neither is an explicit Ruby call, so the backing methods look dead. Fold both
	// forms in as references on the enclosing serializer class. Gated on isSerializer
	// so the shared has_one/has_many/belongs_to names still reach the ActiveRecord
	// association handling below for models.
	if cur := w.cur(); cur != nil && cur.isSerializer && cur.symFactIdx >= 0 && rubySerializerDSL[method] {
		for _, name := range symbolArgs(args, w.src) {
			w.addCallToFact(cur.symFactIdx, name)
			w.addCallToFact(cur.symFactIdx, "include_"+name+"?")
		}
		return
	}

	switch method {
	case "require", "require_relative":
		path := firstStringArg(args, w.src)
		if path == "" {
			return
		}
		props := map[string]any{"language": "ruby"}
		if method == "require_relative" {
			props["require_relative"] = true
		}
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindDependency,
			Name:      w.dir + " -> " + path,
			File:      w.relFile,
			Line:      line(node),
			EndLine:   endLine(node),
			Column:    column(node),
			EndColumn: endColumn(node),
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: path}},
		})

	case "include", "extend", "prepend":
		mixin := firstConstArg(args, w.src)
		if mixin == "" || mixin == "ActiveSupport::Concern" {
			return
		}
		// A Sidekiq worker is a plain class that INCLUDES a module rather than
		// inheriting one, so the component cannot be read off the superclass. Fill it in
		// here, without overwriting a classification the superclass already produced.
		if w.isRails {
			if c := railsComponent("", "", []string{mixin}, w.relFile); c != "" {
				if s := w.cur(); s != nil && s.symFactIdx >= 0 {
					if p := w.out[s.symFactIdx].Props; p != nil {
						if _, set := p["rails_component"]; !set {
							p["rails_component"] = c
						}
					}
				}
			}
		}
		scope := w.scopeQual()
		if scope == "" {
			scope = w.dir
		}
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindDependency,
			Name:      scope + " -> " + mixin,
			File:      w.relFile,
			Line:      line(node),
			EndLine:   endLine(node),
			Column:    column(node),
			EndColumn: endColumn(node),
			Props: map[string]any{
				"language":   "ruby",
				"mixin_kind": method,
			},
			Relations: []facts.Relation{{Kind: facts.RelImplements, Target: mixin}},
		})

	case "attr_reader", "attr_writer", "attr_accessor":
		attrKind := strings.TrimPrefix(method, "attr_")
		scope := w.scopeQual()
		if scope == "" {
			scope = w.dir
		}
		for _, attr := range symbolArgs(args, w.src) {
			w.out = append(w.out, facts.Fact{
				Kind:      facts.KindSymbol,
				Name:      scope + "#" + attr,
				File:      w.relFile,
				Line:      line(node),
				EndLine:   endLine(node),
				Column:    column(node),
				EndColumn: endColumn(node),
				Props: map[string]any{
					"symbol_kind": facts.SymbolVariable,
					"exported":    w.exported(),
					"language":    "ruby",
					"attr_kind":   attrKind,
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
			})
		}

	case "openapi_spec_path":
		spec := firstStringArg(args, w.src)
		if spec == "" {
			return
		}
		scope := w.scopeQual()
		if scope == "" {
			scope = w.dir
		}
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindDependency,
			Name:      scope + " -> " + spec,
			File:      w.relFile,
			Line:      line(node),
			EndLine:   endLine(node),
			Column:    column(node),
			EndColumn: endColumn(node),
			Props: map[string]any{
				"language":  "ruby",
				"type":      "openapi_spec",
				"spec_file": spec,
			},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: spec}},
		})

	case "has_many", "has_one", "belongs_to", "has_and_belongs_to_many":
		if s := w.cur(); s == nil || !s.isModel {
			return
		}
		assoc := firstSymbolArg(args, w.src)
		if assoc == "" {
			return
		}
		target := assoc
		if method == "has_many" || method == "has_and_belongs_to_many" {
			target = singularize(assoc)
		}
		target = snakeToCamel(target)
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindDependency,
			Name:      w.relFile + ":" + method + " :" + assoc,
			File:      w.relFile,
			Line:      line(node),
			EndLine:   endLine(node),
			Column:    column(node),
			EndColumn: endColumn(node),
			Props: map[string]any{
				"language":         "ruby",
				"association_kind": method,
				// The raw association name as it is called in code (e.g. "posts" for
				// has_many :posts) — lets the perf analyzer flag lazy-loaded
				// association reads inside loops (N+1).
				"association": assoc,
			},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: target}},
		})

	case "scope":
		if s := w.cur(); s == nil || !s.isModel {
			return
		}
		name := firstSymbolArg(args, w.src)
		if name == "" {
			return
		}
		props := map[string]any{
			"symbol_kind": facts.SymbolFunc,
			"language":    "ruby",
			"scope":       true,
			"model":       w.cur().name,
		}
		if preloads := preloadsIn(args, w.src); len(preloads) > 0 {
			props["preloads"] = preloads
		}
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindSymbol,
			Name:      "scope:" + name,
			File:      w.relFile,
			Line:      line(node),
			EndLine:   endLine(node),
			Column:    column(node),
			EndColumn: endColumn(node),
			Props:     props,
		})
	}
}

// --- AST value helpers ---

// qualify joins the current scope with a simple name to form a Ruby-qualified name.
func (w *rubyWalker) qualify(name string) string {
	if scope := w.scopeQual(); scope != "" {
		return scope + "::" + name
	}
	return name
}

// constName returns the name of a constant or scope_resolution node (e.g. "Foo"
// or "Foo::Bar").
func (w *rubyWalker) constName(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	switch kindOf(node) {
	case "constant", "scope_resolution":
		return rubyText(node, w.src)
	}
	return ""
}

// superclassName extracts the superclass name from a `superclass` node
// (the "< Base" part of a class declaration).
// superclassName returns the superclass expression's text: a plain constant or
// scope resolution as written, and a CALL-form superclass — Sequel's dataset
// idiom `Sequel::Model(:customers)` — as receiver plus arguments, so
// sequelModelBase can read the literal table. Callers strip the call arguments
// (superclassBase) everywhere the base class name alone is meant.
func (w *rubyWalker) superclassName(node *sitter.Node) string {
	if node == nil {
		return ""
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "constant", "scope_resolution":
			return rubyText(c, w.src)
		case "call":
			recv := c.ChildByFieldName("receiver")
			if recv == nil {
				continue
			}
			switch kindOf(recv) {
			case "constant", "scope_resolution":
				return rubyText(c, w.src)
			}
		}
	}
	return ""
}

// bodyHasConcern reports whether a class/module body directly contains
// `extend ActiveSupport::Concern`.
func bodyHasConcern(body *sitter.Node, src []byte) bool {
	if body == nil {
		return false
	}
	for i := uint(0); i < body.ChildCount(); i++ {
		c := body.Child(i)
		if kindOf(c) != "call" || c.ChildByFieldName("receiver") != nil {
			continue
		}
		if rubyText(c.ChildByFieldName("method"), src) != "extend" {
			continue
		}
		if firstConstArg(c.ChildByFieldName("arguments"), src) == "ActiveSupport::Concern" {
			return true
		}
	}
	return false
}

// firstStringArg returns the content of the first string in an argument_list (or
// the string node itself when passed directly).
func firstStringArg(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	var find func(n *sitter.Node) *sitter.Node
	find = func(n *sitter.Node) *sitter.Node {
		if kindOf(n) == "string" {
			return n
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			if r := find(n.Child(i)); r != nil {
				return r
			}
		}
		return nil
	}
	s := find(node)
	if s == nil {
		return ""
	}
	for i := uint(0); i < s.ChildCount(); i++ {
		if kindOf(s.Child(i)) == "string_content" {
			return rubyText(s.Child(i), src)
		}
	}
	return ""
}

// firstConstArg returns the first constant / scope_resolution argument's text.
func firstConstArg(args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		c := args.Child(i)
		switch kindOf(c) {
		case "constant", "scope_resolution":
			return rubyText(c, src)
		}
	}
	return ""
}

// symbolArgs returns the names (without leading ':') of all simple_symbol args.
func symbolArgs(args *sitter.Node, src []byte) []string {
	var out []string
	if args == nil {
		return out
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		c := args.Child(i)
		if kindOf(c) == "simple_symbol" {
			out = append(out, strings.TrimPrefix(rubyText(c, src), ":"))
		}
	}
	return out
}

// firstSymbolArg returns the first simple_symbol name (without leading ':').
func firstSymbolArg(args *sitter.Node, src []byte) string {
	for _, s := range symbolArgs(args, src) {
		return s
	}
	return ""
}

// firstPositionalPath returns the first positional route-path argument — a string
// literal or a bare symbol — skipping keyword pairs (to:, via:, only:, ...). Unlike
// firstStringArg it does not descend into pair values, so `get :cities_by_zip` yields
// "cities_by_zip" and `get :x, to: 'c#a'` yields "x" (the path, not the handler).
func firstPositionalPath(args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		c := args.Child(i)
		switch kindOf(c) {
		case "string":
			for j := uint(0); j < c.ChildCount(); j++ {
				if kindOf(c.Child(j)) == "string_content" {
					return rubyText(c.Child(j), src)
				}
			}
		case "simple_symbol":
			return strings.TrimPrefix(rubyText(c, src), ":")
		case "pair":
			// Rails hash-rocket route form: `get 'path' => 'ctrl#action'`. The path is
			// the STRING key of the pair. A `to:`/`as:` keyword pair has a symbol/label
			// key (hash_key_symbol), not a string, so it is not treated as a path.
			if k := c.ChildByFieldName("key"); k != nil && kindOf(k) == "string" {
				for j := uint(0); j < k.ChildCount(); j++ {
					if kindOf(k.Child(j)) == "string_content" {
						return rubyText(k.Child(j), src)
					}
				}
			}
		}
	}
	return ""
}

// isAllCaps reports whether s is an ALL_CAPS constant name (letters uppercase,
// digits and underscores allowed). Matches the former constant regex.
func isAllCaps(s string) bool {
	if s == "" {
		return false
	}
	hasLetter := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

// line returns the 1-based start line of a node.
func line(node *sitter.Node) int {
	return int(node.StartPosition().Row) + 1
}

// endLine, column and endColumn complete the span the parser already holds.
// A fact carries them so a reader can underline what the extractor saw
// instead of guessing at a name's position.
func endLine(node *sitter.Node) int {
	return int(node.EndPosition().Row) + 1
}

func column(node *sitter.Node) int {
	return int(node.StartPosition().Column) + 1
}

func endColumn(node *sitter.Node) int {
	return int(node.EndPosition().Column) + 1
}

// rubyText returns the source text covered by a node (nil-safe).
func rubyText(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return string(src[node.StartByte():node.EndByte()])
}

// declaresAbstractClass reports whether a class body sets abstract_class.
func declaresAbstractClass(node *sitter.Node, src []byte) bool {
	body := node.ChildByFieldName("body")
	if body == nil {
		return false
	}
	for i := uint(0); i < body.ChildCount(); i++ {
		child := body.Child(i)
		if child.Kind() != "assignment" {
			continue
		}
		left := child.ChildByFieldName("left")
		right := child.ChildByFieldName("right")
		if left == nil || right == nil {
			continue
		}
		if rubyText(left, src) == "self.abstract_class" && rubyText(right, src) == "true" {
			return true
		}
	}
	return false
}

// blockParamName is the first parameter of a do/brace block, which is the
// element variable for every enumerable that matters here.
func blockParamName(block *sitter.Node, src []byte) string {
	if block == nil {
		return ""
	}
	params := block.ChildByFieldName("parameters")
	if params == nil {
		return ""
	}
	for i := uint(0); i < params.NamedChildCount(); i++ {
		child := params.NamedChild(i)
		if child != nil && child.Kind() == "identifier" {
			return rubyText(child, src)
		}
	}
	return ""
}

func isNameRune(r rune) bool {
	return r == '_' || r == '?' || r == '!' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

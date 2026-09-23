package dotnetextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	sitter "github.com/tree-sitter/go-tree-sitter"
	csharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
)

// ── Node text ───────────────────────────────────────────────────────────────

func nodeText(node *sitter.Node, src []byte) string { return tsutil.Text(node, src) }

func (w *astWalker) nameText(node *sitter.Node) string { return nodeText(node, w.src) }

func findChildByKind(node *sitter.Node, kind string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); kindOf(c) == kind {
			return c
		}
	}
	return nil
}

func sameNode(a, b *sitter.Node) bool {
	return a != nil && b != nil && a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte() &&
		kindOf(a) == kindOf(b)
}

// containsAny reports whether node is one of the listed nodes. Used to skip the
// loop parts already walked in the enclosing scope; nil entries never match.
func containsAny(nodes []*sitter.Node, node *sitter.Node) bool {
	for _, n := range nodes {
		if n != nil && sameNode(n, node) {
			return true
		}
	}
	return false
}

func contains(outer, inner *sitter.Node) bool {
	return outer != nil && inner != nil &&
		outer.StartByte() <= inner.StartByte() && outer.EndByte() >= inner.EndByte()
}

// operatorText returns a binary expression's operator, which the grammar exposes
// as an unnamed child between the operands rather than as node text.
func operatorText(node *sitter.Node, src []byte) string {
	if op := node.ChildByFieldName("operator"); op != nil {
		return nodeText(op, src)
	}
	return ""
}

func argCount(invocation *sitter.Node) int {
	args := invocation.ChildByFieldName("arguments")
	if args == nil {
		return 0
	}
	n := 0
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		if kindOf(args.Child(i)) == "argument" {
			n++
		}
	}
	return n
}

func paramCount(node *sitter.Node) int {
	params := node.ChildByFieldName("parameters")
	if params == nil {
		return 0
	}
	n := 0
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		if kindOf(params.Child(i)) == "parameter" {
			n++
		}
	}
	return n
}

// ── Modifiers and visibility ────────────────────────────────────────────────

// modifierSet collects a declaration's `modifier` children. The C# grammar emits
// each one as its own node rather than wrapping them, so there is no single text
// span to substring-match — and matching against the whole declaration's text
// would find `private` inside a body.
func modifierSet(node *sitter.Node, src []byte) map[string]bool {
	out := make(map[string]bool, 4)
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if kindOf(c) == "modifier" {
			out[nodeText(c, src)] = true
		}
	}
	return out
}

// exported reports whether a declaration is part of the surface other code can
// reach.
//
// C#'s defaults differ by position, and getting them wrong in either direction
// distorts the exported-surface explainer: a top-level type with no modifier is
// `internal` (assembly-private), a member with no modifier is `private`, and an
// interface member with no modifier is `public`. `protected` counts as exported
// because it is reachable by subclasses outside the assembly — but `private
// protected` does not, since that intersection is assembly-local.
func (w *astWalker) exported(mods map[string]bool, isType bool) bool {
	switch {
	case mods["public"]:
		return true
	case mods["private"]:
		return false
	case mods["protected"]:
		return true
	case mods["internal"] || mods["file"]:
		return false
	case isType:
		// No access modifier: a nested type is private, a top-level one internal.
		return false
	default:
		// A member with no access modifier is private — unless it is declared in
		// an interface, where the default is public.
		return w.inInterface()
	}
}

// memberName returns a member declaration's name. An explicitly implemented
// interface member (`void IFoo.Bar()`) still names Bar; the interface qualifier
// is a separate node.
func (w *astWalker) memberName(node *sitter.Node) string {
	if n := node.ChildByFieldName("name"); n != nil {
		return nodeText(n, w.src)
	}
	// destructor_declaration / conversion_operator_declaration have no name field.
	switch kindOf(node) {
	case "destructor_declaration":
		if len(w.typeStack) > 0 {
			return "~" + w.typeStack[len(w.typeStack)-1]
		}
	case "operator_declaration":
		if op := node.ChildByFieldName("operator"); op != nil {
			return "operator" + nodeText(op, w.src)
		}
	case "conversion_operator_declaration":
		if t := node.ChildByFieldName("type"); t != nil {
			return "operator " + simpleTypeName(nodeText(t, w.src))
		}
	}
	return ""
}

// collectMemberNames indexes a type body's directly declared method, property and
// field names, so a bare call inside one member resolves to a sibling. Nested
// types' members are excluded — they belong to a different scope.
func collectMemberNames(body *sitter.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	if body == nil {
		return out
	}
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			c := n.Child(i)
			switch kindOf(c) {
			case "method_declaration", "property_declaration":
				if nn := c.ChildByFieldName("name"); nn != nil {
					out[nodeText(nn, src)] = true
				}
			case "field_declaration", "event_field_declaration":
				if decl := findChildByKind(c, "variable_declaration"); decl != nil {
					for j := uint(0); j < uint(decl.ChildCount()); j++ {
						d := decl.Child(j)
						if kindOf(d) != "variable_declarator" {
							continue
						}
						if nn := d.ChildByFieldName("name"); nn != nil {
							out[nodeText(nn, src)] = true
						}
					}
				}
			case "preproc_if", "preproc_else", "preproc_elif":
				// Members declared inside a configuration guard are still members.
				scan(c)
			}
		}
	}
	scan(body)
	return out
}

// isDataHolderBody reports whether a type body declares state and no behaviour:
// at least one field or property, and no methods.
//
// It exists because the package-metrics explainer spares "data holder" packages
// its rigid — extract interfaces advice, which is not actionable for a bag of
// values, and it recognises that only through a dedicated language construct
// (Kotlin `data class`, Java/C# `record`). C# almost never uses one: jellyfin
// declares 1,552 classes and 13 records, while 278 of those classes are
// property-only carriers. So the exemption saw nothing, and a third of the
// explainer's findings on that repository were DTO, constant and attribute
// packages being told to extract interfaces.
//
// Constructors and destructors do not count as behaviour — a record has a
// constructor too, and initialising state is not the same as having any. Nested
// types are ignored, matching collectMemberNames: a nested class's methods belong
// to the nested class.
func isDataHolderBody(body *sitter.Node) bool {
	if body == nil {
		return false
	}
	hasState := false
	var scan func(*sitter.Node) bool // returns false when a method is found
	scan = func(node *sitter.Node) bool {
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			switch c := node.Child(i); kindOf(c) {
			case "method_declaration", "operator_declaration", "conversion_operator_declaration":
				return false
			case "field_declaration", "property_declaration", "event_field_declaration":
				hasState = true
			case "preproc_if", "preproc_else", "preproc_elif":
				if !scan(c) {
					return false
				}
			}
		}
		return true
	}
	return scan(body) && hasState
}

// countConstructors returns how many constructors a type body declares. One means
// its parameters are unambiguously injected dependencies; several means the type
// has convenience overloads and no single injection point.
func countConstructors(body *sitter.Node) int {
	if body == nil {
		return 0
	}
	n := 0
	var scan func(*sitter.Node)
	scan = func(node *sitter.Node) {
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			c := node.Child(i)
			switch kindOf(c) {
			case "constructor_declaration":
				n++
			case "preproc_if", "preproc_else", "preproc_elif":
				scan(c)
			}
		}
	}
	scan(body)
	return n
}

// extensionReceiver returns the type a static method extends, when its first
// parameter carries the `this` modifier.
func extensionReceiver(node *sitter.Node, src []byte) (string, bool) {
	params := node.ChildByFieldName("parameters")
	if params == nil {
		return "", false
	}
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		p := params.Child(i)
		if kindOf(p) != "parameter" {
			continue
		}
		// Only the FIRST parameter can be the extension receiver.
		if !modifierSet(p, src)["this"] {
			return "", false
		}
		return simpleTypeName(typeFullName(p.ChildByFieldName("type"), src)), true
	}
	return "", false
}

// ── Types ───────────────────────────────────────────────────────────────────

// typeFullName renders a type reference as written, minus the decoration that
// does not change which type is named: generic arguments, nullability, array
// rank and pointer depth.
func typeFullName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch kindOf(node) {
	case "nullable_type", "array_type", "pointer_type":
		return typeFullName(node.ChildByFieldName("type"), src)
	case "generic_name":
		return typeFullName(findChildByKind(node, "identifier"), src)
	case "qualified_name":
		q := typeFullName(node.ChildByFieldName("qualifier"), src)
		n := typeFullName(node.ChildByFieldName("name"), src)
		if q == "" {
			return n
		}
		return q + "." + n
	}
	return strings.TrimSpace(nodeText(node, src))
}

func simpleTypeName(full string) string {
	if i := strings.LastIndex(full, "."); i >= 0 {
		return full[i+1:]
	}
	if i := strings.Index(full, "<"); i >= 0 {
		return full[:i]
	}
	return full
}

// predefinedTypes are the C# keywords for runtime primitives. A reference to one
// resolves to nothing in the repository, so drawing an edge would dangle.
var predefinedTypes = map[string]bool{
	"bool": true, "byte": true, "sbyte": true, "char": true, "decimal": true,
	"double": true, "float": true, "int": true, "uint": true, "long": true,
	"ulong": true, "short": true, "ushort": true, "object": true, "string": true,
	"void": true, "dynamic": true, "nint": true, "nuint": true, "var": true,
}

// targetForType returns a relation target for a type reference: the name as
// written (dotted if the source qualified it, bare otherwise), with a file-local
// `using X = ...` alias substituted. resolveCSharpTargets binds it against the
// project-wide index; a name that resolves to nothing stays as written and
// matches no fact.
func (w *astWalker) targetForType(typeNode *sitter.Node) string {
	full := typeFullName(typeNode, w.src)
	if full == "" || predefinedTypes[full] {
		return ""
	}
	// A generic like `IReadOnlyList<Order>` names the container, not the element;
	// its own simple name is what a reference resolves to.
	if i := strings.Index(full, "<"); i >= 0 {
		full = full[:i]
	}
	if full == "" || predefinedTypes[full] || !isIdentifierPath(full) {
		return ""
	}
	return aliasOr(w.aliases, full)
}

// isIdentifierPath reports whether a string is shaped like a C# type reference —
// dot-separated identifiers and nothing else.
//
// It is a backstop, not the primary filter. typeFullName renders whatever node it
// is handed as that node's own source text, so a caller that reaches it with a
// punctuation token gets an edge to "," or ":" rather than an error. That happened
// once already (see baseTypes), and the failure is silent by nature: a phantom
// target is a node nobody declared, so nothing dangles and no count looks wrong
// until someone reads the edge list.
func isIdentifierPath(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		for i, r := range part {
			switch {
			case r == '_' || r == '@':
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			case r >= '0' && r <= '9' && i > 0:
			default:
				return false
			}
		}
	}
	return true
}

func aliasOr(aliases map[string]string, name string) string {
	if target, ok := aliases[name]; ok {
		return target
	}
	return name
}

// baseTypes returns the targets of a type's base list — its base class and every
// interface it implements. C# does not distinguish the two syntactically, and
// enola's RelImplements covers both, the same way the Java extractor maps
// `extends` and `implements` onto it.
func (w *astWalker) baseTypes(node *sitter.Node) []string {
	bl := findChildByKind(node, "base_list")
	if bl == nil {
		return nil
	}
	var out []string
	// NAMED children only. A base list's children include its `:` and `,` tokens,
	// and typeFullName renders any node as its own text — so walking every child
	// emitted an `implements` edge to ":" once per type that declared a base at
	// all (282 of them on a 500-file repository), each one a phantom node with a
	// large fan-in that the graph then carried into every traversal.
	for i := uint(0); i < uint(bl.NamedChildCount()); i++ {
		c := bl.NamedChild(i)
		switch kindOf(c) {
		case "argument_list":
			continue // the primary-constructor arguments to a base class
		case "primary_constructor_base_type":
			c = firstNamedChild(c)
		}
		if t := w.targetForType(c); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func firstNamedChild(node *sitter.Node) *sitter.Node {
	if node == nil || node.NamedChildCount() == 0 {
		return nil
	}
	return node.NamedChild(0)
}

// isTypeNameShaped reports whether a call's receiver looks like a type name rather
// than a value, which is what separates a static call `Guard.NotNull(x)` from an
// instance call `guard.NotNull(x)`.
//
// The test is the C# naming convention — types are PascalCase, locals and fields
// are not — applied only to a bare identifier. It cannot be exact without type
// resolution, and it is used only to decide whether to OFFER a target to the
// resolution pass; a receiver that is not a declared type resolves to nothing.
func isTypeNameShaped(recvNode *sitter.Node, recv string) bool {
	if kindOf(recvNode) != "identifier" && kindOf(recvNode) != "generic_name" {
		return false
	}
	if recv == "" {
		return false
	}
	r := rune(recv[0])
	return r >= 'A' && r <= 'Z'
}

// ── Loops ───────────────────────────────────────────────────────────────────

type loopClass int

const (
	// loopScaling runs a number of times that grows with the input — the only
	// class that contributes an exponent to the complexity estimate.
	loopScaling loopClass = iota
	// loopConstant runs a fixed number of times: a literal bound, or an iteration
	// over a literal collection.
	loopConstant
	// loopInfinite is `while (true)` / `for (;;)`. It adds no factor of n, but its
	// body still repeats, so a call inside one stays an N+1 candidate.
	loopInfinite
)

func (c loopClass) scales() bool  { return c == loopScaling }
func (c loopClass) repeats() bool { return c != loopConstant }

// syntacticLoopClass classifies a loop so a fixed-size one does not inflate a
// genuine O(n) into a false O(n²).
func syntacticLoopClass(node *sitter.Node, src []byte) loopClass {
	switch kindOf(node) {
	case "for_statement":
		cond := node.ChildByFieldName("condition")
		if cond == nil {
			return loopInfinite
		}
		if constantCondition(cond, src) {
			return loopConstant
		}
	case "while_statement", "do_statement":
		cond := node.ChildByFieldName("condition")
		if cond != nil && strings.TrimSpace(nodeText(cond, src)) == "true" {
			return loopInfinite
		}
	case "foreach_statement":
		if constantIterable(node.ChildByFieldName("right"), src) {
			return loopConstant
		}
	}
	return loopScaling
}

// constantCondition reports whether a for-loop's condition bounds it by a literal
// (`i < 10`), rather than by something derived from the input (`i < items.Count`).
func constantCondition(cond *sitter.Node, src []byte) bool {
	if kindOf(cond) != "binary_expression" {
		return false
	}
	right := cond.ChildByFieldName("right")
	if right == nil {
		return false
	}
	switch kindOf(right) {
	case "integer_literal":
		return true
	case "identifier":
		return isScreamingConst(nodeText(right, src))
	}
	return false
}

// constantIterable reports whether a foreach iterates something of fixed size —
// an inline array or collection literal, or an ALL_CAPS constant.
func constantIterable(value *sitter.Node, src []byte) bool {
	if value == nil {
		return false
	}
	switch kindOf(value) {
	case "array_creation_expression", "implicit_array_creation_expression",
		"collection_expression", "initializer_expression":
		return true
	case "identifier":
		return isScreamingConst(nodeText(value, src))
	}
	return false
}

// isScreamingConst reports whether a name is ALL_CAPS — the convention for a
// compile-time constant, whose size does not grow with the input.
func isScreamingConst(s string) bool {
	if s == "" {
		return false
	}
	hasUpper := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r == '_' || (r >= '0' && r <= '9'):
		default:
			return false
		}
	}
	return hasUpper
}

// iterators are LINQ and collection methods whose lambda argument runs once per
// element — i.e. a loop. A lambda passed to a method NOT in this set (a callback,
// a factory, a continuation) is deferred and is not a loop.
var iterators = map[string]bool{
	"Select": true, "SelectMany": true, "Where": true, "ForEach": true,
	"Any": true, "All": true, "First": true, "FirstOrDefault": true,
	"Last": true, "LastOrDefault": true, "Single": true, "SingleOrDefault": true,
	"Sum": true, "Min": true, "Max": true, "Average": true, "Count": true,
	"GroupBy": true, "OrderBy": true, "OrderByDescending": true, "ThenBy": true,
	"Aggregate": true, "TakeWhile": true, "SkipWhile": true, "RemoveAll": true,
	"ConvertAll": true, "Find": true, "FindAll": true, "Exists": true,
	"TrueForAll": true, "DistinctBy": true, "MaxBy": true, "MinBy": true,
}

// iteratorLambda returns the lambda argument of an iterator invocation, or nil
// when the call is not an element-wise iteration.
func iteratorLambda(node *sitter.Node, src []byte) *sitter.Node {
	fn := node.ChildByFieldName("function")
	if fn == nil || kindOf(fn) != "member_access_expression" {
		return nil
	}
	nameNode := fn.ChildByFieldName("name")
	if nameNode == nil || !iterators[simpleTypeName(nodeText(nameNode, src))] {
		return nil
	}
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if kindOf(a) != "argument" {
			continue
		}
		for j := uint(0); j < uint(a.NamedChildCount()); j++ {
			c := a.NamedChild(j)
			if kindOf(c) == "lambda_expression" || kindOf(c) == "anonymous_method_expression" {
				return c
			}
		}
	}
	return nil
}

// iteratorReceiverBounded reports whether an iterator runs over a fixed-size
// receiver — `new[] { a, b }.Select(...)` iterates twice however large the input.
func iteratorReceiverBounded(node *sitter.Node, src []byte) bool {
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return false
	}
	return constantIterable(fn.ChildByFieldName("expression"), src)
}

// ── I/O vocabulary ──────────────────────────────────────────────────────────

// ioMethods are calls that cross a process boundary — network, disk or database.
// A member invoking one is tagged io_direct, and computeCSharpPerformsIO
// propagates that up the call graph so a per-iteration call to a wrapper reads as
// an N+1 rather than as ordinary work.
var ioMethods = map[string]bool{
	// HttpClient and friends
	"GetAsync": true, "PostAsync": true, "PutAsync": true, "DeleteAsync": true,
	"PatchAsync": true, "SendAsync": true, "GetStringAsync": true,
	"GetStreamAsync": true, "GetByteArrayAsync": true, "PostAsJsonAsync": true,
	"PutAsJsonAsync": true, "GetFromJsonAsync": true, "ReadAsStringAsync": true,
	"DownloadString": true, "DownloadFile": true,
	// ADO.NET
	"ExecuteReader": true, "ExecuteNonQuery": true, "ExecuteScalar": true,
	"ExecuteReaderAsync": true, "ExecuteNonQueryAsync": true, "ExecuteScalarAsync": true,
	// Entity Framework
	"SaveChanges": true, "SaveChangesAsync": true, "ToListAsync": true,
	"ToArrayAsync": true, "FirstOrDefaultAsync": true, "SingleOrDefaultAsync": true,
	"AnyAsync": true, "CountAsync": true, "FindAsync": true, "ToDictionaryAsync": true,
	// Files and streams
	"ReadAllText": true, "ReadAllTextAsync": true, "ReadAllBytes": true,
	"ReadAllBytesAsync": true, "ReadAllLines": true, "ReadAllLinesAsync": true,
	"WriteAllText": true, "WriteAllTextAsync": true, "WriteAllBytes": true,
	"WriteAllBytesAsync": true, "OpenRead": true, "OpenWrite": true,
}

// ioStaticTypes are BCL static classes whose members are all I/O, so any call
// through them counts without enumerating the members.
var ioStaticTypes = map[string]bool{
	"File": true, "Directory": true, "Dns": true, "Process": true,
}

// ioConstructedTypes are types whose construction opens a handle to something
// outside the process.
var ioConstructedTypes = map[string]bool{
	"HttpClient": true, "FileStream": true, "StreamReader": true,
	"StreamWriter": true, "SqlConnection": true, "NpgsqlConnection": true,
	"Socket": true, "TcpClient": true, "WebClient": true,
}

// cheapMethods are obviously-cheap calls that are not worth recording as in-loop
// work. Keeping them out of calls_in_loop is what stops the metric reading as
// noise on every method that formats a string inside a loop.
var cheapMethods = map[string]bool{
	"ToString": true, "Equals": true, "GetHashCode": true, "Add": true,
	"Remove": true, "Contains": true, "Count": true, "Length": true,
	"IsEmpty": true, "Append": true, "AppendLine": true, "Format": true,
	"Substring": true, "Trim": true, "Split": true, "Join": true,
	"Parse": true, "TryParse": true, "ToArray": true, "ToList": true,
	"GetEnumerator": true, "CompareTo": true, "Clone": true, "Dispose": true,
	"nameof": true, "Select": true, "Where": true, "Any": true, "First": true,
}

// csharpKinds names C# node kinds without allocating; see tsutil.KindTable for why
// kindOf(Node) is worth avoiding (3.28 GB and 180 M allocations on one snapshot of
// dotnet/runtime).
//
// A package-level table is safe here because this extractor parses exactly one
// grammar with tree-sitter. The other four languages it reads — Razor, XAML, VB.NET
// and F# — are handled by scanners, not by a tree-sitter parse, so there is no second
// symbol-id space for these ids to be read against. TestKindTable_MatchesNodeKind
// asserts the equivalence over real sources.
var csharpKinds = tsutil.KindsFor(csharp.Language())

// kindOf is nodeText's counterpart for a node's type: the allocation-free spelling of
// kindOf(node).
func kindOf(node *sitter.Node) string { return csharpKinds.Of(node) }

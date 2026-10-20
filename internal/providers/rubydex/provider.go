package rubydex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

const builtInURI = "rubydex:built-in"

var indexableExtensions = map[string]bool{".rb": true, ".rake": true, ".rbs": true, ".ru": true}

// Indexable reports whether the engine indexes a file of this name, which is
// what decides whether its content belongs in the index cache key.
func Indexable(path string) bool {
	return indexableExtensions[filepath.Ext(path)]
}

// Result is what one run of the provider produced: the facts, the census the
// seam records, and a refusal when the workspace could not be read at all.
// Prism and PrismCensus are what the same index says under the Prism reference
// script's names: the calls whose receiver is the lexical enclosing class,
// which the Rubydex facts leave out, and new on a constant as an
// instantiation.
type Result struct {
	Facts       []facts.Fact
	Census      facts.ProviderCensus
	Prism       []facts.Fact
	PrismCensus facts.ProviderCensus
	Refusal     string
}

// Engine is the engine prop a fact this package writes under another
// provider's names carries, so a reader and the seam's pairing can tell which
// reader produced it.
const Engine = "rubydex"

type collector struct {
	g                   *graph
	root                string
	workspace           string
	facts               []facts.Fact
	unresolved          int
	aliasedReferences   int
	undefinedTargets    int
	enclosingReceivers  int
	untypedReceivers    int
	aliasedDeclarations int
	dependencyPaths     int
	bundleAbsent        bool
	documentCache       map[uint64][]span
	declarationNames    map[uint64]string
	ignored             func(file string) bool
	excludedDocuments   int
	excludedReferences  int
	prism               []facts.Fact
	otherReceivers      int
	unreadablePositions int
	classTypedSelf      int
	anonymousOwners     int
	anonymousReceivers  int
	anonymousCallers    int
	anonymousReferrers  int
	sources             map[string][]string
	callsAt             map[string]bool
}

type span struct {
	name        string
	anonymous   bool
	method      bool
	startLine   int
	endLine     int
	startColumn int
}

// Collect indexes the repository at root with the loaded library and returns
// the facts the reference script would have emitted for it.
//
// ignored reports whether a repo-relative file is excluded by the
// repository's configuration. The seam drops facts about such files after a
// provider returns, which for a built-in is work done to be thrown away: on a
// Rails monolith 1,520,163 of 2,155,664 facts. A built-in runs in the
// engine's own address space and can be told, so it is: an excluded document
// is skipped before its definitions are read, which also skips the graph
// calls behind them, and an excluded reference before its fact is built. A
// nil predicate accepts everything.
func Collect(ctx context.Context, lib *Library, root string, ignored func(file string) bool) Result {
	root, err := filepath.Abs(root)
	if err != nil {
		return Result{Refusal: err.Error()}
	}
	if _, err := os.Stat(filepath.Join(root, "Gemfile")); err != nil {
		return Result{Census: facts.ProviderCensus{ConstructsSkipped: 1, SkipCauses: []facts.CensusCause{{Cause: "no Gemfile: not a workspace Rubydex can index", Count: 1}}}}
	}
	c := &collector{root: root, workspace: "file://" + root + "/", documentCache: map[uint64][]span{}, declarationNames: map[uint64]string{}, ignored: ignored,
		sources: map[string][]string{}, callsAt: map[string]bool{}}
	paths, dependencyPaths, bundleAbsent := workspacePaths(ctx, root)
	c.dependencyPaths = dependencyPaths
	c.bundleAbsent = bundleAbsent

	c.g = lib.newGraph()
	defer c.g.close()
	c.g.index(paths)
	c.g.resolve()

	var documents []uint64
	for _, doc := range c.g.documents() {
		uri := c.g.documentURI(doc)
		if !c.inWorkspace(uri) {
			continue
		}
		if c.excluded(uri) {
			c.excludedDocuments++
			continue
		}
		documents = append(documents, doc)
	}
	for _, doc := range documents {
		for _, definition := range c.g.definitions(doc) {
			c.emitAncestry(definition)
		}
		references := c.g.methodReferences(doc)
		for _, reference := range references {
			if location, ok := c.g.methodReferenceLocation(reference.id); ok {
				c.callsAt[callKey(location.URI, location.StartLine, location.StartColumn, c.g.methodReferenceName(reference.id))] = true
			}
		}
		for _, reference := range references {
			c.emitCall(doc, reference)
		}
	}
	c.emitReferences(c.g.constantReferences())
	sort.Slice(c.facts, func(i, j int) bool { return c.facts[i].Name < c.facts[j].Name })
	sort.SliceStable(c.prism, func(i, j int) bool { return prismOrder(c.prism[i]) < prismOrder(c.prism[j]) })
	return Result{Facts: c.facts, Census: c.census(len(documents), c.g.diagnosticCount()),
		Prism: c.prism, PrismCensus: c.prismCensus(len(documents))}
}

func callKey(uri string, line, column int, name string) string {
	return uri + "\x00" + strconv.Itoa(line) + "\x00" + strconv.Itoa(column) + "\x00" + name
}

func prismOrder(f facts.Fact) string {
	return fmt.Sprintf("%s\x00%s\x00%09d", f.Name, f.File, f.Line)
}

// workspacePaths mirrors the gem's own listing: the workspace's top-level
// directories and Ruby files, then the lib and app directories of every gem the
// bundle reports, so Rails engines resolve like plain gems do. Without
// a bundle on PATH the workspace alone is indexed and the census says so.
func workspacePaths(ctx context.Context, root string) (paths []string, dependencyPaths int, bundleAbsent bool) {
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		full := filepath.Join(root, entry.Name())
		if entry.IsDir() || indexableExtensions[filepath.Ext(entry.Name())] {
			paths = append(paths, full)
		}
	}
	bundle, err := exec.LookPath("bundle")
	if err != nil {
		return paths, 0, true
	}
	cmd := exec.CommandContext(ctx, bundle, "list", "--paths")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return paths, 0, true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		for _, sub := range []string{"lib", "app"} {
			if info, err := os.Stat(filepath.Join(line, sub)); err == nil && info.IsDir() {
				paths = append(paths, filepath.Join(line, sub))
				dependencyPaths++
			}
		}
	}
	return paths, dependencyPaths, false
}

func (c *collector) excluded(uri string) bool {
	return c.ignored != nil && c.ignored(c.relative(uri))
}

func (c *collector) inWorkspace(uri string) bool {
	return strings.HasPrefix(uri, c.workspace)
}

func (c *collector) relative(uri string) string {
	return strings.TrimPrefix(uri, c.workspace)
}

func (c *collector) emitAncestry(definition cDefinition) {
	if definition.kind != definitionClass && definition.kind != definitionModule {
		return
	}
	declaration, ok := c.g.definitionDeclaration(definition.id)
	if !ok {
		return
	}
	if declaration.kind != declarationClass && declaration.kind != declarationModule {
		c.aliasedDeclarations++
		return
	}
	location, ok := c.g.definitionLocation(definition.id)
	if !ok {
		return
	}
	name := c.g.declarationName(declaration.id)
	if anonymous(name) {
		c.anonymousOwners++
		return
	}
	distance := 0
	for _, ancestor := range c.g.ancestors(declaration.id) {
		if c.builtIn(ancestor.id) {
			break
		}
		ancestorName := c.g.declarationName(ancestor.id)
		if anonymous(ancestorName) {
			c.anonymousOwners++
			distance++
			continue
		}
		if distance > 0 && ancestorName != name {
			c.facts = append(c.facts, c.fact("rubydex-ancestor: "+name+" -> "+ancestorName, location, "resolved",
				facts.Relation{Kind: "implements", Target: ancestorName},
				map[string]any{"ancestor_distance": distance, "declared_in_workspace": c.declaredInWorkspace(ancestor.id)}))
		}
		distance++
	}
}

func (c *collector) emitCall(doc uint64, reference cMethodReference) {
	receiver, ok := c.g.methodReferenceReceiver(reference.id)
	if !ok {
		c.untypedReceivers++
		return
	}
	location, ok := c.g.methodReferenceLocation(reference.id)
	if !ok {
		return
	}
	receiverFull := c.g.declarationName(receiver.id)
	receiverName := plainName(receiverFull)
	if anonymous(receiverFull) {
		c.anonymousReceivers++
		return
	}
	method := c.g.methodReferenceName(reference.id)
	enclosing, inAnonymousMethod := c.enclosingName(doc, location)
	if inAnonymousMethod {
		c.anonymousCallers++
		return
	}
	caller := enclosing
	if caller == "" {
		caller = c.relative(location.URI)
	}
	instantiated := method == "new" && singleton(receiverFull) && c.emitInstantiation(caller, receiverName, location)
	if instantiated && enclosing != "" && sameLexicalOwner(enclosing, receiverName) {
		c.enclosingReceivers++
		return
	}
	separator := "#"
	if singleton(receiverFull) {
		separator = "."
	}
	callee := receiverName + separator + method
	if enclosing != "" && sameLexicalOwner(enclosing, receiverName) {
		c.enclosingReceivers++
		c.emitEnclosingCall(caller, callee, location, singleton(receiverFull) && strings.Contains(enclosing, "#"))
		return
	}
	if !instantiated {
		c.otherReceivers++
	}
	c.facts = append(c.facts, c.fact("rubydex-call: "+caller+" -> "+callee, location, "constant-receiver",
		facts.Relation{Kind: "calls", Target: callee}, nil))
}

// emitEnclosingCall writes a call whose receiver is the lexical enclosing
// class the way the Prism reference script names it. The level is the
// script's: a receiver written as a constant is constant-receiver, self or no
// receiver is lexical-self. The callee is the receiver the engine resolved,
// so an explicit Board.find is the singleton the script spelled Board#find.
//
// An implicit or self receiver typed as the class's singleton inside an
// instance method is the engine reading a default parameter before it enters
// the method, where Ruby evaluates it on the instance; the two readings
// disagree, so nothing is written and the disagreement is counted.
func (c *collector) emitEnclosingCall(caller, callee string, location Location, classInsideInstanceMethod bool) {
	written, ok := c.writtenReceiver(location)
	if !ok {
		c.unreadablePositions++
		return
	}
	if classInsideInstanceMethod && written != receiverConstant {
		c.classTypedSelf++
		return
	}
	level := "lexical-self"
	if written == receiverConstant {
		level = "constant-receiver"
	}
	c.prism = append(c.prism, c.prismFact("prism-call: "+caller+" -> "+callee, location, level,
		facts.Relation{Kind: "calls", Target: callee}, nil))
}

// emitInstantiation writes new on a written constant as the script does, an
// instantiation of the class the engine resolved, and reports whether it did.
// A new whose receiver is self or implicit is a call, as it is to the script.
// The message sent to the new object in the same expression is named as
// one_shot_call only when the engine read a call of that name exactly there.
func (c *collector) emitInstantiation(caller, class string, location Location) bool {
	written, ok := c.writtenReceiver(location)
	if !ok || written != receiverConstant {
		return false
	}
	var extra map[string]any
	if chained := c.chainedCall(location); chained != "" {
		extra = map[string]any{"one_shot_call": chained}
	}
	c.prism = append(c.prism, c.prismFact("prism-new: "+caller+" -> "+class, location, "constant-receiver",
		facts.Relation{Kind: "instantiates", Target: class}, extra))
	return true
}

type writtenReceiverKind int

const (
	receiverNone writtenReceiverKind = iota
	receiverSelf
	receiverConstant
)

// writtenReceiver reads how the source spelled the receiver of the message at
// location: nothing before it, self, or anything else, which for a receiver
// the engine resolved to a declaration is a constant. The engine reports the
// receiver's declaration and not its syntax, and the script's levels are about
// the syntax.
func (c *collector) writtenReceiver(location Location) (writtenReceiverKind, bool) {
	line, ok := c.sourceLine(location.URI, location.StartLine)
	if !ok || location.StartColumn-1 > len(line) {
		return receiverNone, false
	}
	before := strings.TrimRight(line[:location.StartColumn-1], " \t")
	var operator string
	for _, candidate := range []string{"&.", "::", "."} {
		if strings.HasSuffix(before, candidate) {
			operator = candidate
			break
		}
	}
	if operator == "" {
		return receiverNone, true
	}
	receiver := strings.TrimRight(strings.TrimSuffix(before, operator), " \t")
	if strings.HasSuffix(receiver, "self") && (len(receiver) == len("self") || !identifierByte(receiver[len(receiver)-len("self")-1])) {
		return receiverSelf, true
	}
	return receiverConstant, true
}

// chainedCall is the message sent to the result of the new at location in the
// same expression, `Foo.new(...).bar`, read on its line and confirmed against
// a call the engine reports at that column, or "" when there is none or the
// expression does not read plainly on one line.
func (c *collector) chainedCall(location Location) string {
	line, ok := c.sourceLine(location.URI, location.StartLine)
	if !ok || location.EndLine != location.StartLine || location.EndColumn-1 > len(line) {
		return ""
	}
	i := location.EndColumn - 1
	if i < len(line) && line[i] == '(' {
		depth := 0
		for ; i < len(line); i++ {
			if line[i] == '(' {
				depth++
			} else if line[i] == ')' {
				depth--
				if depth == 0 {
					i++
					break
				}
			}
		}
		if depth != 0 {
			return ""
		}
	}
	rest := line[i:]
	switch {
	case strings.HasPrefix(rest, "&."):
		i += 2
	case strings.HasPrefix(rest, "."):
		i++
	default:
		return ""
	}
	start := i
	for i < len(line) && identifierByte(line[i]) {
		i++
	}
	if i < len(line) && (line[i] == '?' || line[i] == '!') {
		i++
	}
	name := line[start:i]
	if name == "" || !c.callsAt[callKey(location.URI, location.StartLine, start+1, name)] {
		return ""
	}
	return name
}

func identifierByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

func (c *collector) sourceLine(uri string, line int) (string, bool) {
	lines, read := c.sources[uri]
	if !read {
		data, err := os.ReadFile(filepath.Join(c.root, filepath.FromSlash(c.relative(uri))))
		if err == nil {
			lines = strings.Split(string(data), "\n")
		}
		c.sources[uri] = lines
	}
	if line < 1 || line > len(lines) {
		return "", false
	}
	return lines[line-1], true
}

func (c *collector) prismFact(name string, location Location, level string, relation facts.Relation, extra map[string]any) facts.Fact {
	f := c.fact(name, location, level, relation, extra)
	f.Props["engine"] = Engine
	return f
}

// A constant reference the engine reports, held with what decides its fate:
// where it sits, whether it is an inner segment of a qualified path, and what
// it resolved to.
type constantReference struct {
	id       uint64
	doc      uint64
	location Location
	inner    bool
	resolved bool
	target   cDeclaration
}

// emitReferences turns the workspace's constant references into dependency
// facts, one per qualified path. The engine reports a reference per segment,
// so `Foo::VERSION` arrives as `Foo` and as `VERSION`, adjacent on one line
// with the separator between them; the leaf is the dependency and the
// segments before it are its path, because a read of `Foo::VERSION` depends
// on `VERSION` and only names `Foo` on the way. A prefix emitted as its own
// dependency landed on whichever reopening of `Foo` a consumer took first.
func (c *collector) emitReferences(ids []cConstantReference) {
	byLine := map[string][]*constantReference{}
	var refs []*constantReference
	for _, id := range ids {
		location, ok := c.g.constantReferenceLocation(id.id)
		if !ok || !c.inWorkspace(location.URI) {
			continue
		}
		if c.excluded(location.URI) {
			c.excludedReferences++
			continue
		}
		ref := &constantReference{id: id.id, location: location}
		ref.doc, _ = c.g.constantReferenceDocument(id.id)
		ref.target, ref.resolved = c.g.resolvedConstantReference(id.id)
		key := location.URI + "\x00" + strconv.Itoa(location.StartLine)
		byLine[key] = append(byLine[key], ref)
		refs = append(refs, ref)
	}
	for _, line := range byLine {
		for _, prefix := range line {
			for _, next := range line {
				if prefix != next && prefix.location.EndColumn+len("::") == next.location.StartColumn {
					prefix.inner = true
				}
			}
		}
	}
	for _, ref := range refs {
		if ref.inner {
			continue
		}
		c.emitReference(ref, c.pathPrefixes(byLine, ref))
	}
}

// pathPrefixes walks back from a leaf over the adjacent segments on its line
// and returns their written names, outermost first.
//
// Adjacency is a property of one line, so a candidate qualifies only when it
// is another reference and its end column belongs to the line the walk is on.
// A reference spanning lines carries an end column from its last line, and
// `<LibDDWAF>` at lib_ddwaf.rb:262 spans to 267 with end column 25 against
// start column 27: without both conditions it satisfies the arithmetic
// against itself, is returned as its own predecessor, and the walk never ends.
func (c *collector) pathPrefixes(byLine map[string][]*constantReference, leaf *constantReference) []string {
	line := byLine[leaf.location.URI+"\x00"+strconv.Itoa(leaf.location.StartLine)]
	var prefixes []string
	current := leaf
	for {
		var previous *constantReference
		for _, candidate := range line {
			if candidate == current || candidate.location.EndLine != current.location.StartLine {
				continue
			}
			if candidate.location.EndColumn+len("::") == current.location.StartColumn {
				previous = candidate
			}
		}
		if previous == nil {
			break
		}
		prefixes = append(prefixes, c.writtenName(previous))
		current = previous
	}
	for i, j := 0, len(prefixes)-1; i < j; i, j = i+1, j-1 {
		prefixes[i], prefixes[j] = prefixes[j], prefixes[i]
	}
	return prefixes
}

// A declaration referenced many times is marshalled across the seam once:
// a Rails monolith resolves 1,624,360 references to 132,603 declarations, so the
// same name is asked for a dozen times on average. An unresolved reference
// carries its own written name and has nothing to share.
func (c *collector) writtenName(ref *constantReference) string {
	if !ref.resolved {
		return c.g.constantReferenceName(ref.id)
	}
	if name, known := c.declarationNames[ref.target.id]; known {
		return name
	}
	name := plainName(c.g.declarationName(ref.target.id))
	if c.declarationNames == nil {
		c.declarationNames = map[uint64]string{}
	}
	c.declarationNames[ref.target.id] = name
	return name
}

func (c *collector) emitReference(ref *constantReference, prefixes []string) {
	referrer := ""
	if ref.doc != 0 {
		var inAnonymousMethod bool
		referrer, inAnonymousMethod = c.enclosingName(ref.doc, ref.location)
		if inAnonymousMethod {
			c.anonymousReferrers++
			return
		}
	}
	if referrer == "" {
		referrer = c.relative(ref.location.URI)
	}
	written := strings.Join(append(append([]string{}, prefixes...), c.writtenName(ref)), "::")
	if !ref.resolved {
		c.unresolved++
		c.facts = append(c.facts, c.missFact("rubydex-ref: "+referrer+" -> "+written, ref.location, "unresolved", prefixes))
		return
	}
	targetFull := c.g.declarationName(ref.target.id)
	if singleton(targetFull) {
		return
	}
	if ref.target.kind == declarationAlias {
		c.aliasedReferences++
		c.facts = append(c.facts, c.missFact("rubydex-ref: "+referrer+" -> "+written, ref.location, "alias", prefixes))
		return
	}
	if anonymous(targetFull) {
		c.anonymousOwners++
		return
	}
	targetName := plainName(targetFull)
	props := map[string]any{"declared_in_workspace": c.declaredInWorkspace(ref.target.id)}
	if len(prefixes) > 0 {
		props["path_prefixes"] = prefixes
	}
	files, located := c.definingFiles(ref.target.id)
	switch {
	case !located:
		c.undefinedTargets++
		props["resolution_cause"] = "no_definition"
	case len(files) == 1:
		props["target_file"] = files[0]
	}
	c.facts = append(c.facts, c.fact("rubydex-ref: "+referrer+" -> "+targetName, ref.location, "resolved",
		facts.Relation{Kind: "depends_on", Target: targetName}, props))
}

// definingFiles lists the workspace files a declaration is defined in, so a
// consumer can tell which of a reopened name's files a dependency lands on.
// The second result is false when the engine reports no location at all.
func (c *collector) definingFiles(declaration uint64) ([]string, bool) {
	seen := map[string]bool{}
	var files []string
	located := false
	for _, definition := range c.g.declarationDefinitions(declaration) {
		loc, ok := c.g.definitionLocation(definition.id)
		if !ok {
			continue
		}
		located = true
		if !c.inWorkspace(loc.URI) {
			continue
		}
		if rel := c.relative(loc.URI); !seen[rel] {
			seen[rel] = true
			files = append(files, rel)
		}
	}
	sort.Strings(files)
	return files, located
}

// missFact is a reference that resolved to nothing a rule can walk to. It is
// a dependency fact with no relation, so the miss is counted where it
// happened and named by its cause rather than dropped.
func (c *collector) missFact(name string, location Location, cause string, prefixes []string) facts.Fact {
	props := map[string]any{"resolution_level": "name-only", "resolution_cause": cause}
	if len(prefixes) > 0 {
		props["path_prefixes"] = prefixes
	}
	return facts.Fact{
		Kind:  facts.KindDependency,
		Name:  name,
		File:  c.relative(location.URI),
		Line:  location.StartLine,
		Props: props,
	}
}

func (c *collector) fact(name string, location Location, level string, relation facts.Relation, extra map[string]any) facts.Fact {
	props := map[string]any{"resolution_level": level}
	for k, v := range extra {
		props[k] = v
	}
	return facts.Fact{
		Kind:      facts.KindDependency,
		Name:      name,
		File:      c.relative(location.URI),
		Line:      location.StartLine,
		Props:     props,
		Relations: []facts.Relation{relation},
	}
}

// enclosingName is the innermost named class, module or method whose span holds
// the line, named the way the graph names it, or empty at the top level. A
// Class.new or Module.new block is skipped: the engine names it by a hash of
// the absolute path and an offset, so the same repository would give different
// fact names in a clone, a worktree and CI. A call written in the block body
// belongs to the method that contains the block. A call inside a def within
// the block runs when the anonymous class's method is called, not when the
// outer method runs, so there is no honest caller to name: the second result
// is true and the caller writes nothing.
func (c *collector) enclosingName(doc uint64, location Location) (string, bool) {
	spans, cached := c.documentCache[doc]
	if !cached {
		for _, definition := range c.g.definitions(doc) {
			if definition.kind != definitionClass && definition.kind != definitionModule && definition.kind != definitionMethod {
				continue
			}
			declaration, ok := c.g.definitionDeclaration(definition.id)
			if !ok {
				continue
			}
			name := plainName(c.g.declarationName(declaration.id))
			span := span{name: name, anonymous: anonymous(name), method: definition.kind == definitionMethod}
			if loc, ok := c.g.definitionLocation(definition.id); ok {
				span.startLine, span.endLine, span.startColumn = loc.StartLine, loc.EndLine, loc.StartColumn
			}
			spans = append(spans, span)
		}
		c.documentCache[doc] = spans
	}
	var innermost, named *span
	for i := range spans {
		s := &spans[i]
		if s.startLine > location.StartLine || s.endLine < location.StartLine {
			continue
		}
		if innermost == nil || s.startLine > innermost.startLine || (s.startLine == innermost.startLine && s.startColumn > innermost.startColumn) {
			innermost = s
		}
		if s.anonymous {
			continue
		}
		if named == nil || s.startLine > named.startLine || (s.startLine == named.startLine && s.startColumn > named.startColumn) {
			named = s
		}
	}
	if innermost != nil && innermost.anonymous && innermost.method {
		return "", true
	}
	if named == nil {
		return "", false
	}
	return named.name, false
}

func (c *collector) declaredInWorkspace(declaration uint64) bool {
	for _, definition := range c.g.declarationDefinitions(declaration) {
		if loc, ok := c.g.definitionLocation(definition.id); ok && c.inWorkspace(loc.URI) {
			return true
		}
	}
	return false
}

func (c *collector) builtIn(declaration uint64) bool {
	for _, definition := range c.g.declarationDefinitions(declaration) {
		if loc, ok := c.g.definitionLocation(definition.id); ok && loc.URI == builtInURI {
			return true
		}
	}
	return false
}

func (c *collector) census(filesSeen, diagnostics int) facts.ProviderCensus {
	var causes []facts.CensusCause
	add := func(cause string, count int) {
		if count > 0 {
			causes = append(causes, facts.CensusCause{Cause: cause, Count: count})
		}
	}
	add("unresolved constant reference", c.unresolved)
	add("reference resolves to a constant alias", c.aliasedReferences)
	add("resolved declaration has no definition location", c.undefinedTargets)
	add("receiver is the lexical enclosing class", c.enclosingReceivers)
	add("receiver resolves to no constant", c.untypedReceivers)
	add("declaration is a constant alias, not a class", c.aliasedDeclarations)
	add("declaration is anonymous: the engine names it by an id that depends on the checkout path", c.anonymousOwners+c.anonymousReceivers)
	add("caller is a method of an anonymous class", c.anonymousCallers+c.anonymousReferrers)
	add("rubydex diagnostic", diagnostics)
	add("document excluded by the repository's ignore globs", c.excludedDocuments)
	add("reference in a file the repository's ignore globs exclude", c.excludedReferences)
	if c.bundleAbsent {
		add("bundle not on PATH or not readable: workspace indexed without its gems", 1)
	}
	skipped := 0
	for _, cause := range causes {
		skipped += cause.Count
	}
	return facts.ProviderCensus{
		FilesSeen:          filesSeen,
		DeclarationsParsed: len(c.facts),
		ConstructsSkipped:  skipped,
		SkipCauses:         causes,
	}
}

// prismCensus accounts for the method references the Prism names did not
// write: the ones whose receiver the engine typed as no constant, which the
// script writes name-only, the ones on another class, which are the Rubydex
// facts' rubydex-call, and the ones whose source position could not be read.
func (c *collector) prismCensus(filesSeen int) facts.ProviderCensus {
	var causes []facts.CensusCause
	add := func(cause string, count int) {
		if count > 0 {
			causes = append(causes, facts.CensusCause{Cause: cause, Count: count})
		}
	}
	add("receiver resolves to no constant", c.untypedReceivers)
	add("receiver is another class: a rubydex-call fact", c.otherReceivers)
	add("source position of the call could not be read", c.unreadablePositions)
	add("implicit receiver typed as the class inside an instance method", c.classTypedSelf)
	add("caller is a method of an anonymous class", c.anonymousCallers)
	add("receiver is anonymous: the engine names it by an id that depends on the checkout path", c.anonymousReceivers)
	add("document excluded by the repository's ignore globs", c.excludedDocuments)
	skipped := 0
	for _, cause := range causes {
		skipped += cause.Count
	}
	return facts.ProviderCensus{
		FilesSeen:          filesSeen,
		DeclarationsParsed: len(c.prism),
		ConstructsSkipped:  skipped,
		SkipCauses:         causes,
	}
}

func sameLexicalOwner(enclosing, receiver string) bool {
	owner := enclosing
	if i := strings.LastIndexAny(enclosing, "#."); i >= 0 {
		owner = enclosing[:i]
	}
	return owner == receiver
}

func anonymous(name string) bool {
	return strings.Contains(name, "<anonymous>")
}

func singleton(name string) bool {
	return strings.HasSuffix(name, ">")
}

func plainName(name string) string {
	if i := strings.LastIndex(name, "::<"); i >= 0 && strings.HasSuffix(name, ">") {
		name = name[:i]
	}
	return strings.TrimSuffix(name, "()")
}
